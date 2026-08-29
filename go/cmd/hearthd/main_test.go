// Structural test for hearthd's listeners. main() binds real ports and blocks,
// so the invariant maxHeaderBytes states — "on EVERY listener" — is checked
// against the SOURCE, the same way the package's dial audit is
// (TestNoDialPathCanCarryTheAdminToken in internal/server).
//
// It exists because the comment was wrong: the ACME :80 listener, public
// whenever tls_domain is set, was a bare http.ListenAndServe with Go's 1 MiB
// header default and no read or write timeout at all. A prose invariant that
// nothing checks is a claim, not a property.
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

// required are the fields every http.Server literal in this package must set.
var required = []string{"MaxHeaderBytes", "ReadTimeout", "WriteTimeout"}

func TestEveryListenerIsBounded(t *testing.T) {
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
						t.Errorf("%s: http.%s serves with net/http's defaults (1 MiB headers, no timeouts) — build an http.Server with the same bounds as the other listeners",
							fset.Position(call.Pos()), sel.Sel.Name)
					}
				}
			}
			// (2) Every http.Server literal sets the bounds.
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
			set := map[string]bool{}
			for _, el := range lit.Elts {
				if kv, ok := el.(*ast.KeyValueExpr); ok {
					if k, ok := kv.Key.(*ast.Ident); ok {
						set[k.Name] = true
					}
				}
			}
			for _, want := range required {
				if !set[want] {
					t.Errorf("%s: this http.Server does not set %s — the bound must hold on every listener",
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
	// The plain listener, the :443 TLS listener, and the ACME :80 listener.
	if servers < 3 {
		t.Fatalf("found only %d http.Server literals; hearthd has three listeners, so the audit is not seeing them all", servers)
	}
}
