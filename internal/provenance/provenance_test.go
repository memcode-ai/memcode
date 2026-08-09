package provenance

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/memcode-ai/memcode/internal/objectives"
	"github.com/memcode-ai/memcode/internal/store"
	"github.com/memcode-ai/memcode/internal/structure"
)

func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// These tests exercise the non-git provenance (resolution, subsystem, tests,
// deps, objectives). Git-derived fields (introduced/evolved) are covered by the
// live dogfood, not unit tests, to avoid requiring a repo fixture.
func TestWhyResolvesAndLinks(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"package.json":               `{"name":"root","private":true,"workspaces":["packages/*"]}`,
		"pnpm-workspace.yaml":        "packages:\n  - packages/*\n",
		"packages/app/package.json":  `{"name":"@demo/app","dependencies":{"@demo/lib":"workspace:*"}}`,
		"packages/app/index.ts":      "export const x = 1\n",
		"packages/app/index.test.ts": "test('x', () => {})\n",
		"packages/lib/package.json":  `{"name":"@demo/lib"}`,
	})

	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := structure.Scan(ctx, s, root); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if _, err := objectives.New(s).Add(ctx, "ship app", 1, ""); err != nil {
		t.Fatalf("Add objective: %v", err)
	}

	// Provenance for a file: subsystem + tests + serves.
	p, err := Why(ctx, s, root, "packages/app/index.ts")
	if err != nil {
		t.Fatalf("Why(file): %v", err)
	}
	if p.Subsystem != "packages/app" {
		t.Errorf("subsystem = %q, want packages/app", p.Subsystem)
	}
	if len(p.TestedBy) != 1 || p.TestedBy[0] != "packages/app/index.test.ts" {
		t.Errorf("tested_by = %v, want [packages/app/index.test.ts]", p.TestedBy)
	}
	if len(p.Serves) != 1 || p.Serves[0].Title != "ship app" {
		t.Errorf("serves = %+v, want one 'ship app' objective", p.Serves)
	}

	// Provenance for a subsystem name: dependency edges.
	ps, err := Why(ctx, s, root, "packages/app")
	if err != nil {
		t.Fatalf("Why(subsystem): %v", err)
	}
	if len(ps.DependsOn) != 1 || ps.DependsOn[0] != "packages/lib" {
		t.Errorf("depends_on = %v, want [packages/lib]", ps.DependsOn)
	}

	pl, err := Why(ctx, s, root, "lib") // basename match
	if err != nil {
		t.Fatalf("Why(basename): %v", err)
	}
	if len(pl.DependedBy) != 1 || pl.DependedBy[0] != "packages/app" {
		t.Errorf("depended_by = %v, want [packages/app]", pl.DependedBy)
	}
}

func TestWhyUnknownTarget(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeFiles(t, root, map[string]string{"go.mod": "module x\n\ngo 1.26\n"})
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := structure.Scan(ctx, s, root); err != nil {
		t.Fatal(err)
	}
	if _, err := Why(ctx, s, root, "does-not-exist"); err == nil {
		t.Fatal("expected error for unknown target")
	}
}
