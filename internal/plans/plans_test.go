package plans

import (
	"strings"
	"testing"
)

// TestSaveListReadAndReviseInPlace: a saved plan is listed (with title) and read back; saving
// again with the RETURNED slug updates the SAME file (a revision), not a new one.
func TestSaveListReadAndReviseInPlace(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // os.UserHomeDir() honors $HOME — keep the real ~/.memcode untouched

	slug, err := Save("", "# Rebuild the auth module\n\n## Steps\n1. do it")
	if err != nil {
		t.Fatal(err)
	}
	if slug == "" {
		t.Fatal("Save must return a non-empty slug")
	}

	refs, err := List()
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 {
		t.Fatalf("expected 1 saved plan, got %d", len(refs))
	}
	if refs[0].Slug != slug {
		t.Fatalf("listed slug %q != saved %q", refs[0].Slug, slug)
	}
	if refs[0].Title != "Rebuild the auth module" {
		t.Fatalf("title should strip the leading #, got %q", refs[0].Title)
	}

	// Revise: same slug → same file, updated content, still ONE plan.
	slug2, err := Save(slug, "# Rebuild the auth module\n\n## Steps\n1. do it\n2. verify")
	if err != nil {
		t.Fatal(err)
	}
	if slug2 != slug {
		t.Fatalf("revising with a slug must reuse it: got %q want %q", slug2, slug)
	}
	refs, _ = List()
	if len(refs) != 1 {
		t.Fatalf("a revision must update in place, not add a file; got %d plans", len(refs))
	}
	md, err := Read(slug)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(md, "2. verify") {
		t.Fatalf("read should return the revised content, got %q", md)
	}
}

// TestSaveMintsDistinctSlugs: two fresh saves land in two distinct files.
func TestSaveMintsDistinctSlugs(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	a, err := Save("", "# Plan A")
	if err != nil {
		t.Fatal(err)
	}
	b, err := Save("", "# Plan B")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatalf("two fresh plans must get distinct slugs, both got %q", a)
	}
	if refs, _ := List(); len(refs) != 2 {
		t.Fatalf("expected 2 distinct plans, got %d", len(refs))
	}
}

// TestListEmptyWhenNone: no plans dir → empty list, no error (not a failure).
func TestListEmptyWhenNone(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	refs, err := List()
	if err != nil {
		t.Fatalf("List on a missing dir must not error: %v", err)
	}
	if len(refs) != 0 {
		t.Fatalf("expected no plans, got %d", len(refs))
	}
}
