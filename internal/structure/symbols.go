package structure

// Symbol-level drill-down: the per-language layer the package doc reserves.
// Go-only for now, extracted with go/parser (stdlib, CGO-free — deliberately
// NOT tree-sitter, which would break the pure-Go build posture). The output
// feeds the personalized-PageRank repo map (repomap.go): top-level declarations
// become graph nodes, identifier references inside declaration bodies become
// edges attributed to their enclosing declaration.

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// Symbol is one top-level declaration (Go via AST; TS/JS/Python via LSP).
type Symbol struct {
	Name     string `json:"name"`           // bare identifier (methods too — matching is by name)
	Recv     string `json:"recv,omitempty"` // method receiver type, for display
	Kind     string `json:"kind"`           // func | method | type | const | var | class | …
	Lang     string `json:"lang"`           // go | ts | py — reference matching stays within a language group
	Path     string `json:"path"`           // file, relative to the repo root
	Line     int    `json:"line"`
	EndLine  int    `json:"end_line,omitempty"` // full-range end (extern symbols; enclosing-attribution)
	Decl     string `json:"decl"`               // the declaration's first source line, trimmed
	Exported bool   `json:"exported"`
}

// SymbolGraph is the repo-wide symbol reference graph.
type SymbolGraph struct {
	Symbols []Symbol
	// Refs[from][to] = reference count: the body of Symbols[from] mentions the
	// name of Symbols[to]. Name-collision targets split weight (see extractRefs).
	Refs map[int]map[int]float64
	// FileRefs counts references INTO each symbol from other files' top level
	// (var initializers etc.) — kept per-symbol, feeds in-degree display.
	InDeg []float64
}

// ExtractGoSymbols parses every non-test Go file under root (repo-tracked,
// vendored trees dropped) and returns the symbol reference graph. Parse errors
// skip the file — a broken working-tree file must not break orientation.
//
// Reference resolution exploits Go's visibility rules instead of guessing:
// a BARE identifier can only reference a symbol in the SAME package (dir), and
// a cross-package reference is always an import selector (pkg.Name) — resolved
// exactly via the module path from go.mod. Selectors on non-package values
// (x.Method()) can't be typed without a checker, so they match METHOD symbols
// by name repo-wide with the weight split across candidates.
func ExtractGoSymbols(ctx context.Context, root string, files []string) SymbolGraph {
	g := SymbolGraph{Refs: map[int]map[int]float64{}}
	fset := token.NewFileSet()
	type parsed struct {
		path string
		file *ast.File
	}
	var trees []parsed
	for _, rel := range files {
		if ctx.Err() != nil {
			return g
		}
		if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			continue
		}
		f, err := parser.ParseFile(fset, rel, src, parser.SkipObjectResolution)
		if err != nil {
			continue
		}
		trees = append(trees, parsed{rel, f})
		g.Symbols = append(g.Symbols, fileSymbols(fset, rel, f, src)...)
	}

	idx := symIndex{
		modPath: modulePath(root),
		byDir:   map[string]map[string][]int{},
		methods: map[string][]int{},
	}
	// Short names (i, ok, err) are noise — they'd wire everything to everything;
	// require length ≥ 3 for name matching.
	for i, s := range g.Symbols {
		if len(s.Name) < 3 {
			continue
		}
		dir := path.Dir(s.Path)
		if idx.byDir[dir] == nil {
			idx.byDir[dir] = map[string][]int{}
		}
		idx.byDir[dir][s.Name] = append(idx.byDir[dir][s.Name], i)
		if s.Kind == "method" && !genericMethod[s.Name] {
			idx.methods[s.Name] = append(idx.methods[s.Name], i)
		}
	}
	// Enclosing-symbol lookup per file, by declaration position order.
	byFile := map[string][]int{}
	for i, s := range g.Symbols {
		byFile[s.Path] = append(byFile[s.Path], i)
	}

	g.InDeg = make([]float64, len(g.Symbols))
	for _, t := range trees {
		extractRefs(fset, t.file, t.path, idx, byFile[t.path], &g)
	}
	return g
}

// genericMethod names satisfy fmt/io/error interfaces everywhere — every
// .String() in the repo would split across dozens of unrelated implementations
// and drag buffer plumbing into the top ranks. Not worth graph edges.
var genericMethod = map[string]bool{
	"String": true, "Error": true, "Read": true, "Write": true, "Close": true,
}

// symIndex is the resolution index for reference extraction.
type symIndex struct {
	modPath string                      // module path from go.mod ("" = no cross-package resolution)
	byDir   map[string]map[string][]int // package dir → name → symbol indices
	methods map[string][]int            // method name → symbol indices (repo-wide)
}

// modulePath reads the module path from root/go.mod (cheap line scan).
func modulePath(root string) string {
	data, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(line, "module "); ok {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}

// fileSymbols lifts a file's top-level declarations into Symbols.
func fileSymbols(fset *token.FileSet, rel string, f *ast.File, src []byte) []Symbol {
	lines := strings.Split(string(src), "\n")
	declLine := func(pos token.Pos) (int, string) {
		p := fset.Position(pos)
		text := ""
		if p.Line-1 >= 0 && p.Line-1 < len(lines) {
			text = strings.TrimSpace(lines[p.Line-1])
		}
		return p.Line, text
	}
	var out []Symbol
	for _, d := range f.Decls {
		switch decl := d.(type) {
		case *ast.FuncDecl:
			if decl.Name.Name == "init" && decl.Recv == nil {
				continue // package init: never referenced, pure declaration noise
			}
			line, text := declLine(decl.Pos())
			s := Symbol{Name: decl.Name.Name, Kind: "func", Lang: "go", Path: rel, Line: line, Decl: text,
				Exported: decl.Name.IsExported()}
			if decl.Recv != nil && len(decl.Recv.List) > 0 {
				s.Kind = "method"
				s.Recv = recvTypeName(decl.Recv.List[0].Type)
			}
			out = append(out, s)
		case *ast.GenDecl:
			for _, spec := range decl.Specs {
				switch sp := spec.(type) {
				case *ast.TypeSpec:
					line, text := declLine(sp.Pos())
					out = append(out, Symbol{Name: sp.Name.Name, Kind: "type", Lang: "go", Path: rel,
						Line: line, Decl: text, Exported: sp.Name.IsExported()})
				case *ast.ValueSpec:
					kind := "var"
					if decl.Tok == token.CONST {
						kind = "const"
					}
					for _, name := range sp.Names {
						if name.Name == "_" {
							continue
						}
						line, text := declLine(name.Pos())
						out = append(out, Symbol{Name: name.Name, Kind: kind, Lang: "go", Path: rel,
							Line: line, Decl: text, Exported: name.IsExported()})
					}
				}
			}
		}
	}
	return out
}

func recvTypeName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.StarExpr:
		return recvTypeName(t.X)
	case *ast.Ident:
		return t.Name
	case *ast.IndexExpr: // generic receiver T[P]
		return recvTypeName(t.X)
	case *ast.IndexListExpr:
		return recvTypeName(t.X)
	}
	return ""
}

// extractRefs walks a file and records symbol references attributed to the
// enclosing top-level declaration. Resolution by identifier position:
//   - import selector pkg.Name → symbols named Name in that package's dir (exact)
//   - other selector x.Name    → method symbols named Name, weight split
//   - bare identifier Name     → symbols named Name in THIS package's dir
func extractRefs(fset *token.FileSet, f *ast.File, rel string, idx symIndex, fileSyms []int, g *SymbolGraph) {
	// position-ordered symbol starts for enclosing-decl attribution
	sort.Slice(fileSyms, func(a, b int) bool { return g.Symbols[fileSyms[a]].Line < g.Symbols[fileSyms[b]].Line })
	encloser := func(line int) int {
		at := -1
		for _, si := range fileSyms {
			if g.Symbols[si].Line <= line {
				at = si
			} else {
				break
			}
		}
		return at
	}
	// Local import name → in-module package dir.
	imports := map[string]string{}
	for _, imp := range f.Imports {
		p := strings.Trim(imp.Path.Value, `"`)
		var dir string
		if p == idx.modPath {
			dir = "."
		} else if idx.modPath != "" && strings.HasPrefix(p, idx.modPath+"/") {
			dir = p[len(idx.modPath)+1:]
		} else {
			continue // stdlib / external — their symbols aren't ours
		}
		name := path.Base(p)
		if imp.Name != nil {
			name = imp.Name.Name
		}
		imports[name] = dir
	}

	addRef := func(from int, targets []int, line int) {
		if len(targets) == 0 {
			return
		}
		w := 1.0 / float64(len(targets))
		for _, to := range targets {
			// The declaration itself is not a reference.
			if from == to || (g.Symbols[to].Path == rel && g.Symbols[to].Line == line) {
				continue
			}
			g.InDeg[to] += w
			if from < 0 {
				continue // top-level ref outside any decl — counts for in-degree only
			}
			m := g.Refs[from]
			if m == nil {
				m = map[int]float64{}
				g.Refs[from] = m
			}
			m[to] += w
		}
	}

	selfDir := path.Dir(rel)
	isImportSel := func(sel *ast.SelectorExpr) (string, bool) {
		if x, ok := sel.X.(*ast.Ident); ok {
			dir, ok := imports[x.Name]
			return dir, ok
		}
		return "", false
	}
	handledSel := map[*ast.Ident]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.CallExpr:
			// x.Method() — a CALLED value selector matches method symbols by name
			// (weight split). Non-called selectors (field reads, x.Text) match
			// nothing: without a type checker they're indistinguishable from
			// fields, and counting them drowned real methods in phantom refs.
			if sel, ok := node.Fun.(*ast.SelectorExpr); ok {
				if _, imp := isImportSel(sel); !imp {
					handledSel[sel.Sel] = true
					line := fset.Position(sel.Sel.Pos()).Line
					addRef(encloser(line), idx.methods[sel.Sel.Name], line)
				}
			}
		case *ast.SelectorExpr:
			if handledSel[node.Sel] {
				return true
			}
			handledSel[node.Sel] = true
			if dir, imp := isImportSel(node); imp {
				if x, ok := node.X.(*ast.Ident); ok {
					handledSel[x] = true // the package qualifier is not a symbol ref
				}
				line := fset.Position(node.Sel.Pos()).Line
				addRef(encloser(line), idx.byDir[dir][node.Sel.Name], line)
			}
		case *ast.Ident:
			if handledSel[node] {
				return true
			}
			line := fset.Position(node.Pos()).Line
			addRef(encloser(line), idx.byDir[selfDir][node.Name], line)
		}
		return true
	})
}
