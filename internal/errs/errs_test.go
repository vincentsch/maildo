package errs

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"
)

func TestEveryCodeConstantHasExitMapping(t *testing.T) {
	// The exit map is intentionally unexported, so this parses the source file
	// and verifies new Code* constants are added to the map explicitly.
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "errs.go", nil, 0)
	if err != nil {
		t.Fatalf("parse errs.go: %v", err)
	}
	codes := map[string]string{}
	var exitKeys []ast.Expr
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, spec := range gen.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range value.Names {
				if strings.HasPrefix(name.Name, "Code") && i < len(value.Values) {
					if lit, ok := value.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
						unquoted, err := strconv.Unquote(lit.Value)
						if err != nil {
							t.Fatalf("unquote %s: %v", lit.Value, err)
						}
						codes[name.Name] = unquoted
					}
				}
				if name.Name == "exits" && len(value.Values) == 1 {
					exitKeys = collectExitKeys(t, value.Values[0])
				}
			}
		}
	}
	if len(codes) == 0 {
		t.Fatal("no Code* constants found")
	}
	exits := map[string]bool{}
	for _, key := range exitKeys {
		switch key := key.(type) {
		case *ast.Ident:
			value, ok := codes[key.Name]
			if !ok {
				t.Fatalf("exits map references unknown constant %s", key.Name)
			}
			exits[value] = true
		case *ast.BasicLit:
			if key.Kind == token.STRING {
				unquoted, err := strconv.Unquote(key.Value)
				if err != nil {
					t.Fatalf("unquote exit key: %v", err)
				}
				exits[unquoted] = true
			}
		}
	}
	for name, code := range codes {
		if !exits[code] {
			t.Fatalf("%s (%q) is missing from exits map", name, code)
		}
	}
}

func collectExitKeys(t *testing.T, expr ast.Expr) []ast.Expr {
	t.Helper()
	lit, ok := expr.(*ast.CompositeLit)
	if !ok {
		t.Fatalf("exits is %T, want composite literal", expr)
	}
	var out []ast.Expr
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		out = append(out, kv.Key)
	}
	return out
}
