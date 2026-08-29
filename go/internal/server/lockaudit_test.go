// Structural audit of every state-lock region in this package.
//
// The lesson this generalizes came from the credential gate: a slot acquired
// without a deferred release is a slot that never comes back if the code between
// acquire and release panics. The global state lock is the same shape and a much
// worse outcome. net/http recovers a handler panic and keeps the process alive,
// so a panic inside a bare Lock()/Unlock() region does not crash hearthd — it
// leaves srv.st locked forever. Every subsequent request blocks on it: no
// creates, no execs, no deletes, no /metrics, no sweep. A total, silent control
// plane outage from a process that still answers its listener and still passes a
// TCP health check.
//
// So the rule is: acquire and release in one statement pair —
//
//	srv.st.Lock()
//	defer srv.st.Unlock()
//
// and where the critical section is only part of a function (because the rest of
// it dials an agent or writes to sqlite, and the global lock must not be held
// across that), scope the defer with a closure rather than unlocking by hand:
//
//	func() {
//	    srv.st.Lock()
//	    defer srv.st.Unlock()
//	    ...
//	}()
//
// This is the same idiom join.go already uses for the credential-gate release.
//
// Test files are not audited: a panic there fails a test, it does not take out a
// control plane.
package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"testing"
)

// unlocksByHand lists the functions still permitted to release the state lock
// with a bare call, and why. Adding a name here is the deliberate act the audit
// exists to force; it is not a place to park new code.
//
// Every entry is in a file outside the scope of the change that introduced this
// audit (expose.go, templates.go, metrics.go). They are all the same shape as the
// converted sites — resolve under the lock, unlock, then do slow I/O — so they are
// convertible to the closure idiom in exactly the same way, and they carry the
// same panic exposure until they are. They are recorded rather than fixed so the
// audit can be enforcing from the moment it lands.
var unlocksByHand = map[string]string{
	"exposeCommon":       "expose.go: resolves under the lock, unlocks, then calls the agent to install a DNAT rule, then re-locks to record the row",
	"reExposeFork":       "expose.go: locks once per parent expose inside a loop, with the agent call between iterations",
	"unexposeSandbox":    "expose.go: same shape as exposeCommon, in reverse",
	"listRoutes":         "expose.go: snapshots under the lock and formats the response body outside it",
	"ensureRoute":        "expose.go: resolves under the lock, unlocks, then may call the agent",
	"captureRootfs":      "templates.go: resolves under the lock, unlocks, then runs a rootfs capture that can take minutes",
	"serveTenantMetrics": "metrics.go: snapshots under the lock and formats the response body outside it",
}

// stateLockCall reports whether the expression is a `<x>.st.Lock()` /
// `<x>.st.Unlock()` call, and which of the two.
func stateLockCall(e ast.Expr) (method string, ok bool) {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return "", false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || (sel.Sel.Name != "Lock" && sel.Sel.Name != "Unlock") {
		return "", false
	}
	inner, ok := sel.X.(*ast.SelectorExpr)
	if !ok || inner.Sel.Name != "st" {
		return "", false
	}
	if _, ok := inner.X.(*ast.Ident); !ok {
		return "", false
	}
	return sel.Sel.Name, true
}

// stmtIsStateCall reports whether a plain (non-deferred) statement is one of the
// two state-lock calls.
func stmtIsStateCall(s ast.Stmt, want string) bool {
	es, ok := s.(*ast.ExprStmt)
	if !ok {
		return false
	}
	m, ok := stateLockCall(es.X)
	return ok && m == want
}

// stmtIsDeferredUnlock reports whether a statement is `defer <x>.st.Unlock()`.
func stmtIsDeferredUnlock(s ast.Stmt) bool {
	d, ok := s.(*ast.DeferStmt)
	if !ok {
		return false
	}
	m, ok := stateLockCall(d.Call)
	return ok && m == "Unlock"
}

// stmtList returns the statement list a node carries, if it is one of the nodes
// that holds one.
func stmtList(n ast.Node) ([]ast.Stmt, bool) {
	switch b := n.(type) {
	case *ast.BlockStmt:
		return b.List, true
	case *ast.CaseClause:
		return b.Body, true
	case *ast.CommClause:
		return b.Body, true
	}
	return nil, false
}

// loopBetweenHereAndTheFunctionScope reports whether the innermost enclosing
// function scope is reached only after passing through a for/range statement —
// i.e. whether a `defer` written here would pile up rather than release at the
// end of the iteration.
func loopBetweenHereAndTheFunctionScope(stack []ast.Node) bool {
	for i := len(stack) - 1; i >= 0; i-- {
		switch stack[i].(type) {
		case *ast.ForStmt, *ast.RangeStmt:
			return true
		case *ast.FuncLit, *ast.FuncDecl:
			return false
		}
	}
	return false
}

func TestEveryStateLockRegionIsPanicSafe(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob package sources: %v", err)
	}
	fset := token.NewFileSet()
	locks, scanned := 0, 0
	usedException := map[string]bool{}

	for _, name := range files {
		if len(name) > 8 && name[len(name)-8:] == "_test.go" {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		scanned++
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			fname := fn.Name.Name
			_, excused := unlocksByHand[fname]

			var stack []ast.Node
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if n == nil {
					stack = stack[:len(stack)-1]
					return false
				}
				stack = append(stack, n)

				// (1) A bare Unlock: the release this audit is about.
				if s, ok := n.(ast.Stmt); ok && stmtIsStateCall(s, "Unlock") && !excused {
					t.Errorf("%s: %s releases the state lock with a bare Unlock. A panic before it reaches this line leaves srv.st held forever and every later request blocks on it — net/http keeps the process up, so it is a silent total outage. Wrap the critical section in a closure with `defer srv.st.Unlock()`; if it genuinely must unlock early, add %q to unlocksByHand with the reason.",
						fset.Position(s.Pos()), fname, fname)
				}

				list, ok := stmtList(n)
				if !ok {
					return true
				}
				for i, s := range list {
					if !stmtIsStateCall(s, "Lock") {
						continue
					}
					locks++
					if i+1 < len(list) && stmtIsDeferredUnlock(list[i+1]) {
						// A defer inside a loop body does not release at the
						// end of the iteration: it piles up until the function
						// returns, and the second Lock() self-deadlocks.
						if loopBetweenHereAndTheFunctionScope(stack) {
							t.Errorf("%s: %s takes the state lock inside a loop and defers the Unlock — that defer runs at FUNCTION return, so the next iteration deadlocks on the lock this one still holds. Put the whole critical section in a closure called from the loop body instead.",
								fset.Position(s.Pos()), fname)
						}
						continue
					}
					if excused {
						usedException[fname] = true
						continue
					}
					t.Errorf("%s: %s takes the state lock without `defer srv.st.Unlock()` on the very next line. See the package comment in lockaudit_test.go: a panic inside the region deadlocks the whole control plane. Use the closure idiom, or add %q to unlocksByHand with the reason it must unlock early.",
						fset.Position(s.Pos()), fname, fname)
				}
				return true
			})
		}
	}

	// Guard the audit itself.
	if scanned == 0 {
		t.Fatal("audited no package sources; the glob is wrong")
	}
	if locks == 0 {
		t.Fatal("found no state-lock sites; the matcher is wrong, so this audit proves nothing")
	}

	// And guard the exception list: an entry that no longer corresponds to a
	// bare-unlock site is a licence nobody is using, and it would silently excuse
	// a future one written into that same function.
	var dead []string
	for name := range unlocksByHand {
		if !usedException[name] {
			dead = append(dead, name)
		}
	}
	sort.Strings(dead)
	for _, name := range dead {
		t.Errorf("unlocksByHand lists %q, but that function no longer unlocks the state lock by hand — drop the entry rather than leaving a standing exemption for whatever is written there next", name)
	}
}
