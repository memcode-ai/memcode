package catalog

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// The canonical catalog lives at the REPO ROOT (/models.json) — this embedded
// copy exists only because go:embed cannot reach outside its package. Any
// drift means someone edited one side only;
// fail loudly so the two catalogs can never disagree about money or windows.
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
		t.Fatal("catalog/models.json has drifted from the canonical root /models.json — edit the ROOT file and copy it over this one, never the reverse")
	}
}
