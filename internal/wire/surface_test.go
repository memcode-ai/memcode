package wire

import (
	"crypto/sha256"
	"encoding/hex"
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strings"
	"testing"
)

// The protocol vocabulary is VENDORED into four modules, and nothing in the
// compiler notices when they drift.
//
// memcode (the source of truth), repoagent, builder and gateway-cloud each hold
// their own copy of this file. A change to one that does not reach the others
// produces no build error and no test failure — the structs still compile, the
// events still arrive, and the consumers quietly disagree about what an event
// means. That is precisely how assistant_delta came to carry rendered terminal
// output for every client at once.
//
// So the surface is hashed. If this fails, the vocabulary changed: update every
// copy in the SAME change, then update the constant in all four tests. It is
// meant to be a deliberate act, not a surprise.
//
// The hash covers what the wire actually promises — message names and values,
// payload field names, types and json tags — and deliberately not comments,
// import order or formatting, which are allowed to differ between copies.
const protocolSurface = "295ebe32fda90537a4eb711863ab20c4317d657aa89633aceab0da5b31652d9a"

func TestProtocolSurfaceMatchesEveryVendoredCopy(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "streamjson.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	ast.Inspect(f, func(n ast.Node) bool {
		switch d := n.(type) {
		case *ast.ValueSpec:
			for i, name := range d.Names {
				if !strings.HasPrefix(name.Name, "Msg") || i >= len(d.Values) {
					continue
				}
				if lit, ok := d.Values[i].(*ast.BasicLit); ok {
					lines = append(lines, "const "+name.Name+"="+lit.Value)
				}
			}
		case *ast.TypeSpec:
			st, ok := d.Type.(*ast.StructType)
			if !ok {
				return true
			}
			for _, fld := range st.Fields.List {
				tag := ""
				if fld.Tag != nil {
					tag = fld.Tag.Value
				}
				typ := protoTypeName(fld.Type)
				for _, nm := range fld.Names {
					lines = append(lines, "field "+d.Name.Name+"."+nm.Name+" "+typ+" "+tag)
				}
			}
		}
		return true
	})
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	got := hex.EncodeToString(sum[:])
	if got != protocolSurface {
		t.Errorf("the protocol surface changed.\n  got  %s\n  want %s\n\n"+
			"Update EVERY vendored copy of streamjson.go in this same change "+
			"(memcode, repoagent, builder, gateway-cloud), then set this constant "+
			"to the new hash in all four. Surface:\n%s",
			got, protocolSurface, strings.Join(lines, "\n"))
	}
}

func protoTypeName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		return protoTypeName(t.X) + "." + t.Sel.Name
	case *ast.StarExpr:
		return "*" + protoTypeName(t.X)
	case *ast.ArrayType:
		return "[]" + protoTypeName(t.Elt)
	case *ast.MapType:
		return "map[" + protoTypeName(t.Key) + "]" + protoTypeName(t.Value)
	}
	return "?"
}
