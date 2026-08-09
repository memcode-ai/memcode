package scripts

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestValidSlug(t *testing.T) {
	cases := map[string]bool{
		"rebuild-cli":   true,
		"a":             true,
		"a1-b2":         true,
		"":              false,
		"Rebuild-CLI":   false,
		"rebuild_cli":   false,
		"-rebuild":      false,
		"rebuild space": false,
	}
	for slug, want := range cases {
		if got := ValidSlug(slug); got != want {
			t.Errorf("ValidSlug(%q) = %v, want %v", slug, got, want)
		}
	}
}

func TestSaveRejectsBadInput(t *testing.T) {
	root := t.TempDir()
	if _, err := Save(root, "Bad Slug", "desc", "echo hi"); err == nil {
		t.Fatal("expected error for invalid slug")
	}
	if _, err := Save(root, "ok-slug", "", "echo hi"); err == nil {
		t.Fatal("expected error for empty description")
	}
	if _, err := Save(root, "ok-slug", "desc", "   \n\n "); err == nil {
		t.Fatal("expected error for empty body")
	}
}

func TestSaveGetListRoundtrip(t *testing.T) {
	root := t.TempDir()
	sc, err := Save(root, "rebuild-cli", "Rebuild the CLI binary", "cd cli && go build ./...")
	if err != nil {
		t.Fatal(err)
	}
	if sc.Slug != "rebuild-cli" || sc.Description != "Rebuild the CLI binary" {
		t.Fatalf("unexpected script: %+v", sc)
	}
	if sc.CreatedAt.IsZero() {
		t.Fatal("expected CreatedAt to be set")
	}
	if sc.RunCount != 0 || !sc.LastRunAt.IsZero() {
		t.Fatalf("fresh script should have no run history: %+v", sc)
	}

	got, ok := Get(root, "rebuild-cli")
	if !ok {
		t.Fatal("expected to find saved script")
	}
	if got.Body != "cd cli && go build ./..." {
		t.Fatalf("body = %q", got.Body)
	}

	// File is a real, executable shell script with a shebang.
	raw, err := os.ReadFile(scriptPath(root, "rebuild-cli"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(raw), "#!/bin/sh\n") {
		t.Fatalf("missing shebang: %q", raw)
	}
	if fi, err := os.Stat(scriptPath(root, "rebuild-cli")); err != nil || fi.Mode().Perm()&0o111 == 0 {
		t.Fatalf("expected executable perms, got %v (err %v)", fi.Mode(), err)
	}

	if _, err := Save(root, "commit-push-deploy", "Commit, push, deploy", "git add -A && git commit -m x && git push"); err != nil {
		t.Fatal(err)
	}
	list, err := List(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Slug != "commit-push-deploy" || list[1].Slug != "rebuild-cli" {
		t.Fatalf("unexpected list (want sorted by slug): %+v", list)
	}
}

func TestListOnMissingDirReturnsEmpty(t *testing.T) {
	root := t.TempDir()
	list, err := List(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("expected empty list, got %v", list)
	}
}

func TestGetMissingReturnsFalse(t *testing.T) {
	root := t.TempDir()
	if _, ok := Get(root, "does-not-exist"); ok {
		t.Fatal("expected ok=false for a missing script")
	}
}

func TestUpsertPreservesHistory(t *testing.T) {
	root := t.TempDir()
	if _, err := Save(root, "rebuild-cli", "v1 description", "go build ./v1"); err != nil {
		t.Fatal(err)
	}
	first, _ := Get(root, "rebuild-cli")
	created := first.CreatedAt

	RecordRun(root, "rebuild-cli")
	RecordRun(root, "rebuild-cli")
	afterRuns, _ := Get(root, "rebuild-cli")
	if afterRuns.RunCount != 2 {
		t.Fatalf("run count = %d, want 2", afterRuns.RunCount)
	}
	if afterRuns.LastRunAt.IsZero() {
		t.Fatal("expected LastRunAt to be set after RecordRun")
	}

	updated, err := Save(root, "rebuild-cli", "v2 description", "go build ./v2")
	if err != nil {
		t.Fatal(err)
	}
	if updated.Description != "v2 description" || updated.Body != "go build ./v2" {
		t.Fatalf("update didn't take: %+v", updated)
	}
	if !updated.CreatedAt.Equal(created) {
		t.Fatalf("CreatedAt should be preserved across update: got %v, want %v", updated.CreatedAt, created)
	}
	if updated.RunCount != 2 {
		t.Fatalf("RunCount should be preserved across update: got %d, want 2", updated.RunCount)
	}
	if updated.LastRunAt.IsZero() {
		t.Fatal("LastRunAt should be preserved across update")
	}
}

func TestRecordRunOnMissingScriptIsNoop(t *testing.T) {
	root := t.TempDir()
	// Must not panic or create anything.
	RecordRun(root, "nope")
	if _, ok := Get(root, "nope"); ok {
		t.Fatal("RecordRun must not create a script that doesn't exist")
	}
}

func TestDeleteMovesToTrash(t *testing.T) {
	root := t.TempDir()
	if _, err := Save(root, "throwaway", "desc", "echo bye"); err != nil {
		t.Fatal(err)
	}
	if err := Delete(root, "throwaway"); err != nil {
		t.Fatal(err)
	}
	if _, ok := Get(root, "throwaway"); ok {
		t.Fatal("expected script to be gone from the live store")
	}
	entries, err := os.ReadDir(trashDir(root))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !strings.HasPrefix(entries[0].Name(), "throwaway-") {
		t.Fatalf("expected one trashed file prefixed throwaway-, got %v", entries)
	}
}

func TestDeleteMissingErrors(t *testing.T) {
	root := t.TempDir()
	if err := Delete(root, "nope"); err == nil {
		t.Fatal("expected error deleting a script that was never saved")
	}
}

func TestHeaderRoundtripPreservesFields(t *testing.T) {
	root := t.TempDir()
	if _, err := Save(root, "roundtrip", "desc with spaces: and stuff", "echo one\necho two\n"); err != nil {
		t.Fatal(err)
	}
	RecordRun(root, "roundtrip")

	sc, ok := Get(root, "roundtrip")
	if !ok {
		t.Fatal("expected to load saved script")
	}
	if sc.Description != "desc with spaces: and stuff" {
		t.Fatalf("description = %q", sc.Description)
	}
	if sc.Body != "echo one\necho two" {
		t.Fatalf("body = %q", sc.Body)
	}
	if sc.RunCount != 1 || sc.LastRunAt.IsZero() {
		t.Fatalf("expected run history preserved: %+v", sc)
	}
}

func TestLoadFallsBackToMtimeWhenCreatedMissing(t *testing.T) {
	root := t.TempDir()
	dir := Dir(root)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "handwritten.sh")
	body := "#!/bin/sh\n# description: hand written, no created field\n\necho hi\n"
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	sc, ok := Get(root, "handwritten")
	if !ok {
		t.Fatal("expected to load hand-written script")
	}
	if sc.CreatedAt.IsZero() {
		t.Fatal("expected CreatedAt fallback to file mtime")
	}
	if time.Since(sc.CreatedAt) > time.Minute {
		t.Fatalf("CreatedAt fallback too far off: %v", sc.CreatedAt)
	}
}
