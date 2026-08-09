package checkpoint

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, root, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestSnapshotRestoreAcrossTurns(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a.go", "v1")
	write(t, root, "b.go", "b1")
	l := New(root, "sess_test")

	// Turn 1 edits a.go and CREATES c.go.
	cp1 := l.Begin("first edit")
	cp1.Snapshot("a.go")
	cp1.Snapshot("c.go") // does not exist yet
	write(t, root, "a.go", "v2")
	write(t, root, "c.go", "created")

	// Turn 2 edits a.go again and b.go. Double-snapshot of a.go must keep the
	// turn-2 pre-image (v2), not overwrite it with anything else.
	cp2 := l.Begin("second edit")
	cp2.Snapshot("a.go")
	cp2.Snapshot("a.go") // once per checkpoint
	cp2.Snapshot("b.go")
	write(t, root, "a.go", "v3")
	write(t, root, "b.go", "b2")

	if got := l.List(); len(got) != 2 || got[0].Seq != 1 || len(got[1].Files) != 2 {
		t.Fatalf("List = %+v", got)
	}

	// Rewind to before turn 2 only: a.go → v2, b.go → b1, c.go stays.
	if _, err := l.Restore(2); err != nil {
		t.Fatal(err)
	}
	if read(t, root, "a.go") != "v2" || read(t, root, "b.go") != "b1" || read(t, root, "c.go") != "created" {
		t.Fatalf("partial rewind wrong: a=%q b=%q", read(t, root, "a.go"), read(t, root, "b.go"))
	}
	// Consumed checkpoints are dropped; turn 1 remains.
	if got := l.List(); len(got) != 1 || got[0].Seq != 1 {
		t.Fatalf("after restore List = %+v", got)
	}

	// Rewind to before turn 1: a.go → v1, created file deleted.
	restored, err := l.Restore(1)
	if err != nil {
		t.Fatal(err)
	}
	if read(t, root, "a.go") != "v1" {
		t.Fatalf("a.go = %q, want v1", read(t, root, "a.go"))
	}
	if _, err := os.Stat(filepath.Join(root, "c.go")); !os.IsNotExist(err) {
		t.Fatal("c.go must be deleted (the edit created it)")
	}
	joined := strings.Join(restored, "\n")
	if !strings.Contains(joined, "a.go") || !strings.Contains(joined, "c.go") {
		t.Fatalf("restored list incomplete: %v", restored)
	}

	// Nothing left: restoring again errors.
	if _, err := l.Restore(1); err == nil {
		t.Fatal("empty log must refuse to restore")
	}
}

func TestEmptyTurnWritesNothing(t *testing.T) {
	root := t.TempDir()
	l := New(root, "s")
	cp := l.Begin("no edits")
	if !cp.Empty() {
		t.Fatal("fresh checkpoint must be empty")
	}
	if got := l.List(); len(got) != 0 {
		t.Fatalf("empty turn must not appear: %+v", got)
	}
	if _, err := os.Stat(filepath.Join(root, ".memcode", "checkpoints", "s")); err == nil {
		// The dir may exist only if something was written — an empty Begin must not create it.
		entries, _ := os.ReadDir(filepath.Join(root, ".memcode", "checkpoints", "s"))
		if len(entries) != 0 {
			t.Fatalf("empty turn left files: %v", entries)
		}
	}
}

func TestSeqSurvivesReopen(t *testing.T) {
	root := t.TempDir()
	write(t, root, "x", "1")
	l := New(root, "s")
	cp := l.Begin("t1")
	cp.Snapshot("x")
	l2 := New(root, "s") // reopen (a resumed session)
	cp2 := l2.Begin("t2")
	cp2.Snapshot("x")
	if got := l2.List(); len(got) != 2 || got[1].Seq <= got[0].Seq {
		t.Fatalf("seq must keep increasing across reopen: %+v", got)
	}
}

// A partially-failed Restore must KEEP the consumed checkpoints so the rewind can be retried
// — they are the only copy of the pre-images. Regression for the bug where RemoveAll ran even
// when some files failed to restore, destroying the undo data.
func TestRestoreKeepsPreimagesOnPartialFailure(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a.go", "v1")
	l := New(root, "s")

	cp := l.Begin("edit")
	cp.Snapshot("a.go")   // existed → pre-image v1
	cp.Snapshot("newdir") // does not exist → restore = delete
	write(t, root, "a.go", "v2")
	// Turn "newdir" into a NON-EMPTY directory so os.Remove fails during restore.
	write(t, root, filepath.Join("newdir", "sub", "f"), "x")

	// First restore partially fails (a.go → v1 succeeds; removing newdir fails).
	if _, err := l.Restore(1); err == nil {
		t.Fatal("expected a partial-failure error")
	}
	if read(t, root, "a.go") != "v1" {
		t.Fatalf("a.go should have been restored to v1")
	}
	if got := l.List(); len(got) != 1 {
		t.Fatalf("consumed checkpoint must be retained for retry, List = %+v", got)
	}

	// Fix the obstruction and retry — now it succeeds AND consumes the checkpoint.
	if err := os.RemoveAll(filepath.Join(root, "newdir")); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Restore(1); err != nil {
		t.Fatalf("retry restore should succeed: %v", err)
	}
	if got := l.List(); len(got) != 0 {
		t.Fatalf("successful restore should consume the checkpoint, List = %+v", got)
	}
}
