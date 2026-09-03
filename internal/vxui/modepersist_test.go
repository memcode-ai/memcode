package vxui

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestModeChangesGoThroughSetMode is the mechanical form of "the permission mode
// persists". config.Mode has always documented itself as "persisted when the user
// cycles it", and cmd/interactive.go has always READ it at startup — but nothing
// wrote it back, so Shift+Tab and /mode silently reset on every restart.
//
// The write now lives in exactly one place, setMode, which changes the session and
// saves the config together. This guard keeps it that way: a second call site that
// skips the save would reintroduce the bug invisibly, since the only symptom is a
// setting that quietly forgets.
func TestModeChangesGoThroughSetMode(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var offenders []string

	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		// Walk each function; record any SetMode call outside setMode itself.
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Name.Name == "setMode" {
				continue
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "SetMode" {
					return true
				}
				offenders = append(offenders,
					fset.Position(call.Pos()).String()+" (in "+fn.Name.Name+")")
				return true
			})
		}
	}

	if len(offenders) > 0 {
		t.Errorf("SetMode called outside setMode — these changes will not persist:\n  %s\n\n"+
			"Route the change through setMode so the session and the config move together.",
			strings.Join(offenders, "\n  "))
	}
}
