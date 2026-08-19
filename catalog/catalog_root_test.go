package catalog

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// The canonical catalog is THIS package's models.json (go:embed cannot reach
// outside the package); the repo-root /models.json is a generated copy (see
// the //go:generate directive in catalog.go). Any drift means someone edited
// one side without regenerating; fail loudly so the two catalogs can never
// disagree about money or windows.
func TestEmbeddedCatalogMatchesRoot(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Skip("cannot locate source file")
	}
	root := filepath.Join(filepath.Dir(thisFile), "..", "models.json")
	rootBytes, err := os.ReadFile(root)
	if err != nil {
		// Outside the monorepo (published module, vendored build) there is no
		// root file — nothing to compare.
		t.Skipf("root models.json not found (%v) — skipping outside the monorepo", err)
	}
	if !bytes.Equal(rootBytes, catalogData) {
		t.Fatal("root /models.json has drifted from catalog/models.json — edit catalog/models.json and run `go generate ./catalog` to sync the root copy")
	}
}
