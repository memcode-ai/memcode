package runtime

import "testing"

func TestCQTokenize(t *testing.T) {
	terms := cqTokenize("where does the TUI collapse a large paste?")
	got := map[string]bool{}
	for _, x := range terms {
		got[x] = true
	}
	for _, want := range []string{"tui", "collapse", "large", "paste"} {
		if !got[want] {
			t.Errorf("missing term %q in %v", want, terms)
		}
	}
	for _, drop := range []string{"where", "does", "the", "a"} {
		if got[drop] {
			t.Errorf("stopword %q should be dropped: %v", drop, terms)
		}
	}
}

// TestCQRank: a file matching more of the query (path + declaration + density)
// outranks a file with a lone incidental mention.
func TestCQRank(t *testing.T) {
	terms := []string{"paste", "composer", "wrap", "collapse"}
	rgOut := "" +
		"internal/tui/composer_wrap.go:12:func composerVisualRows(v string) int // wrap + paste collapse\n" +
		"internal/tui/tui.go:421:m.composerPasted = true // a pasted paste\n" +
		"internal/util/x.go:5:// incidentally mentions paste once\n"
	files := cqRank(rgOut, terms)
	if len(files) == 0 {
		t.Fatal("expected ranked files")
	}
	if files[0].path != "internal/tui/composer_wrap.go" {
		t.Errorf("top hit should be composer_wrap.go (path+decl+density), got %q (scores: %+v)", files[0].path, files)
	}
	// The incidental util mention must rank last.
	if files[len(files)-1].path != "internal/util/x.go" {
		t.Errorf("incidental mention should rank last, got %q", files[len(files)-1].path)
	}
	// The top file's best evidence should surface the declaration line first.
	if !files[0].best[0].decl {
		t.Error("declaration line should be the first evidence")
	}
}
