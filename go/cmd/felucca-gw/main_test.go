// Structural test for felucca-gw's listener, mirroring feluccad's
// TestEveryListenerIsBounded (cmd/feluccad/main_test.go). main() binds a real
// port and blocks, so the bounds are checked against the SOURCE.
//
// It exists because this is the listener the previous round of listener
// hardening did not reach, and it is the INTERNET-FACING one: felucca-gw is the
// tenant ingress. It set ReadHeaderTimeout and nothing else — Go's 1 MiB
// per-connection header default, no idle bound.
//
// The required set is NOT feluccad's, and the difference is the point. felucca-gw
// is a reverse proxy: ReadTimeout and WriteTimeout are whole-request deadlines
// and would cut WebSocket upgrades, SSE streams and long uploads, so they are
// deliberately zero here. "Deliberately" is what this enforces — they must be
// written out, so the choice is visible and re-made rather than forgotten, while
// the three bounds that CAN hold on a streaming proxy must be real values.
package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"testing"
)

// serveFuncs are the net/http helpers that construct a server implicitly, with
// the package defaults and no way to bound anything.
var serveFuncs = map[string]bool{
	"ListenAndServe": true, "ListenAndServeTLS": true, "Serve": true, "ServeTLS": true,
}

// bounded are the fields every http.Server literal in this package must set to a
// real (non-zero) value.
var bounded = []string{"MaxHeaderBytes", "ReadHeaderTimeout", "IdleTimeout"}

// declared are the fields that must appear in the literal but may legitimately be
// zero on a streaming reverse proxy — as long as the zero is written down.
var declared = []string{"ReadTimeout", "WriteTimeout"}

// isZeroLit reports whether an expression is the literal 0.
func isZeroLit(e ast.Expr) bool {
	lit, ok := e.(*ast.BasicLit)
	return ok && lit.Kind == token.INT && lit.Value == "0"
}

func TestIngressListenerIsBounded(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob package sources: %v", err)
	}
	fset := token.NewFileSet()
	servers, scanned := 0, 0
	for _, name := range files {
		if name == "main_test.go" {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		scanned++
		ast.Inspect(f, func(n ast.Node) bool {
			// (1) No package-level http.ListenAndServe(...) & friends: those
			// serve with Go's defaults, which is what this test is about.
			if call, ok := n.(*ast.CallExpr); ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
					if x, ok := sel.X.(*ast.Ident); ok && x.Name == "http" && serveFuncs[sel.Sel.Name] {
						t.Errorf("%s: http.%s serves with net/http's defaults (1 MiB headers, no timeouts) — build an http.Server with the bounds below",
							fset.Position(call.Pos()), sel.Sel.Name)
					}
				}
			}
			// (2) Every http.Server literal carries the bounds.
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			sel, ok := lit.Type.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Server" {
				return true
			}
			if x, ok := sel.X.(*ast.Ident); !ok || x.Name != "http" {
				return true
			}
			servers++
			set := map[string]ast.Expr{}
			for _, el := range lit.Elts {
				if kv, ok := el.(*ast.KeyValueExpr); ok {
					if k, ok := kv.Key.(*ast.Ident); ok {
						set[k.Name] = kv.Value
					}
				}
			}
			for _, want := range bounded {
				v, ok := set[want]
				if !ok {
					t.Errorf("%s: this http.Server does not set %s — the bound must hold on the internet-facing ingress listener",
						fset.Position(lit.Pos()), want)
					continue
				}
				if isZeroLit(v) {
					t.Errorf("%s: %s is 0, which is no bound at all — this listener is unauthenticated tenant ingress",
						fset.Position(lit.Pos()), want)
				}
			}
			for _, want := range declared {
				if _, ok := set[want]; !ok {
					t.Errorf("%s: this http.Server does not mention %s — it may be 0 on a streaming reverse proxy (it would cut WebSocket upgrades and SSE), but write the 0 down with the reason so the choice is deliberate and stays reviewed",
						fset.Position(lit.Pos()), want)
				}
			}
			return true
		})
	}
	// Guard the audit itself: a glob that matched nothing would pass silently.
	if scanned == 0 {
		t.Fatal("audited no package sources; the glob is wrong")
	}
	if servers < 1 {
		t.Fatal("found no http.Server literal; felucca-gw serves ingress, so the audit is not seeing its listener")
	}
}
