package assemble

import (
	"context"
	"os"
	"path/filepath"
	"testing"

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

func setup(t *testing.T) (context.Context, store.Store, string) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"package.json":              `{"name":"root","private":true,"workspaces":["packages/*"]}`,
		"pnpm-workspace.yaml":       "packages:\n  - packages/*\n",
		"README.md":                 "# demo\n",
		"packages/app/package.json": `{"name":"@demo/app","dependencies":{"@demo/lib":"workspace:*"}}`,
		"packages/app/index.ts":     "import {permission} from '@demo/lib'\nexport const x = 1\n",
		"packages/lib/package.json": `{"name":"@demo/lib"}`,
		"packages/lib/index.ts":     "export const permission = true\n",
	})

	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if _, err := structure.Scan(ctx, s, root); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	return ctx, s, root
}

func TestContextPathTarget(t *testing.T) {
	ctx, s, root := setup(t)

	pack, err := Context(ctx, s, root, "packages/app/index.ts")
	if err != nil {
		t.Fatalf("Context: %v", err)
	}
	if pack.Subsystem != "packages/app" {
		t.Errorf("subsystem = %q, want packages/app", pack.Subsystem)
	}
	// The target file is the top relevant file.
	if len(pack.RelevantFiles) == 0 || pack.RelevantFiles[0].Ref != "packages/app/index.ts" {
		t.Fatalf("expected target file first, got %+v", pack.RelevantFiles)
	}
	// Dependency edge surfaces.
	var hasDep bool
	for _, d := range pack.Dependencies {
		if d.Ref == "packages/lib" {
			hasDep = true
		}
	}
	if !hasDep {
		t.Errorf("expected dependency on packages/lib, got %+v", pack.Dependencies)
	}
	// Every relevant file carries a reason (provenance).
	for _, it := range pack.RelevantFiles {
		if it.Reason == "" {
			t.Errorf("relevant file %q has no reason", it.Ref)
		}
	}
}

func TestContextQueryTarget(t *testing.T) {
	ctx, s, root := setup(t)

	pack, err := Context(ctx, s, root, "permission")
	if err != nil {
		t.Fatalf("Context: %v", err)
	}
	// Both files mention "permission"; they should rank in.
	var found int
	for _, f := range pack.RelevantFiles {
		if f.Ref == "packages/app/index.ts" || f.Ref == "packages/lib/index.ts" {
			found++
		}
	}
	if found < 2 {
		t.Fatalf("expected both files matching 'permission', got %+v", pack.RelevantFiles)
	}
}

func TestContextNormalizesEmptySlices(t *testing.T) {
	ctx, s, root := setup(t)
	pack, err := Context(ctx, s, root, "packages/lib")
	if err != nil {
		t.Fatalf("Context: %v", err)
	}
	// lib has no dependencies, but the field must be non-nil (marshals as []).
	if pack.Dependencies == nil || pack.Constraints == nil {
		t.Error("empty slices should be non-nil after normalize")
	}
}
