package knowledge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every embedded pack must parse cleanly, expose a name + triggers + non-empty Facts, and keep
// its injected digest under the cap. A malformed pack would silently drop out of the catalog.
func TestCatalogPacksWellFormed(t *testing.T) {
	cat := Catalog()
	if len(cat) == 0 {
		t.Fatal("catalog is empty — no packs embedded/parsed")
	}
	for _, p := range cat {
		if p.Name == "" {
			t.Error("pack with empty name")
		}
		if len(p.Triggers) == 0 {
			t.Errorf("pack %q has no triggers", p.Name)
		}
		if strings.TrimSpace(p.Facts) == "" {
			t.Errorf("pack %q has empty Facts (the authoritative half is required)", p.Name)
		}
	}
}

// The pack that motivated the whole feature must carry the VERCEL_ENV fact and steer away from
// inventing a custom NEXT_PUBLIC_* flag — that exact wrong move is what triggered this work.
func TestVercelPackHasEnvFact(t *testing.T) {
	p, ok := Get("vercel")
	if !ok {
		t.Fatal("vercel pack missing")
	}
	if !strings.Contains(p.Facts, "VERCEL_ENV") {
		t.Error("vercel pack must state the VERCEL_ENV fact")
	}
	if !strings.Contains(p.Facts, "NEXT_PUBLIC_") {
		t.Error("vercel pack must warn against inventing a custom NEXT_PUBLIC_* env flag")
	}
}

// Detect is the deterministic repo fingerprint (the ONLY automatic signal): a marker file or a
// manifest dep flags the stack, which the session pointer then names for the model.
func TestDetectByRepoFingerprint(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "vercel.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !contains(names(Detect(root)), "vercel") {
		t.Error("a repo with vercel.json should detect the vercel pack")
	}

	dep := t.TempDir()
	if err := os.WriteFile(filepath.Join(dep, "package.json"),
		[]byte(`{"dependencies":{"next":"15.0.0","react":"19.0.0"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	d := names(Detect(dep))
	if !contains(d, "next") || !contains(d, "react") {
		t.Errorf("package.json deps next+react should detect both packs, got %v", d)
	}
}

func names(ps []Pack) []string {
	var out []string
	for _, p := range ps {
		out = append(out, p.Name)
	}
	return out
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
