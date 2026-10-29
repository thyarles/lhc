package kube

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestNothingElseRunsKubectl keeps the read-only promise: kubectl runs only
// through Get. Outside this package no code may name kubectl's path, hand
// "kubectl" to Run or LookPath, or put it in a Shell script.
func TestNothingElseRunsKubectl(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	self, _ := filepath.Abs(".")
	fset := token.NewFileSet()
	checked := 0
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path == self || strings.HasPrefix(d.Name(), ".") || d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		checked++
		rel, _ := filepath.Rel(root, path)
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.BasicLit:
				if s, ok := literal(n); ok && strings.HasSuffix(s, "/kubectl") {
					t.Errorf("%s:%d: %q: run kubectl through kube.Cluster.Get", rel, fset.Position(n.Pos()).Line, s)
				}
			case *ast.CallExpr:
				name := ""
				switch fn := n.Fun.(type) {
				case *ast.SelectorExpr:
					name = fn.Sel.Name
				case *ast.Ident:
					name = fn.Name
				}
				if name != "Run" && name != "Shell" && name != "LookPath" {
					return true
				}
				for _, a := range n.Args {
					ast.Inspect(a, func(m ast.Node) bool {
						lit, ok := m.(*ast.BasicLit)
						if !ok {
							return true
						}
						if s, _ := literal(lit); s == "kubectl" || name == "Shell" && strings.Contains(s, "kubectl") {
							t.Errorf("%s:%d: %s(%q): run kubectl through kube.Cluster.Get", rel, fset.Position(lit.Pos()).Line, name, s)
						}
						return true
					})
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked < 50 {
		t.Fatalf("only %d files checked: is the module root right?", checked)
	}
}

func literal(l *ast.BasicLit) (string, bool) {
	if l.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(l.Value)
	return s, err == nil
}
