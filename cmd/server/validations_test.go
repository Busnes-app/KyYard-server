package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// runServer starts the validation loop and waits for it before the store closes, so a rollback in
// flight is sent and recorded. Removing either half fails here.
func TestServerRunsAndAwaitsTheValidationLoop(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var started, awaited string
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.GoStmt:
			if sel, ok := n.Call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "RunValidations" && len(n.Call.Args) == 2 {
				if id, ok := n.Call.Args[1].(*ast.Ident); ok {
					started = id.Name
				}
			}
		case *ast.UnaryExpr:
			if id, ok := n.X.(*ast.Ident); ok && n.Op == token.ARROW && started != "" && id.Name == started {
				awaited = id.Name
			}
		}
		return true
	})
	if started == "" || awaited != started {
		t.Fatalf("RunValidations started with done %q, awaited %q", started, awaited)
	}
}
