package vxui

import (
	"reflect"
	"testing"
)

func TestTokenAtAndMentionQuery(t *testing.T) {
	r := []rune("fix @cli/ma please")
	start, tok := tokenAt(r, 9) // cursor after "@cli/ma"... rune 9 is inside the token
	if tok == "" || start != 4 {
		t.Fatalf("tokenAt = %d %q", start, tok)
	}
	// Cursor at end of "@cli/ma" (index 11).
	if st, q, ok := mentionQuery(r, 11); !ok || q != "cli/ma" || st != 4 {
		t.Fatalf("mentionQuery = %d %q %v", st, q, ok)
	}
	// After the trailing space: no active token.
	if _, _, ok := mentionQuery([]rune("@done "), 6); ok {
		t.Fatal("mention must close after whitespace")
	}
	// Mid-word @ (an email) opens no mention: the token starts at 'a', not '@'.
	if _, _, ok := mentionQuery([]rune("mail a@b.com"), 12); ok {
		t.Fatal("a@b.com must not open a mention")
	}
	// Bare "@" → empty query, active.
	if _, q, ok := mentionQuery([]rune("@"), 1); !ok || q != "" {
		t.Fatalf("bare @ should be active with empty query, got %q %v", q, ok)
	}
}

func TestMatchFilesRanking(t *testing.T) {
	files := []string{
		"cli/main.go",
		"cli/cmd/agent.go",
		"cli/internal/agent/runtime/exec.go",
		"api/main.go",
		"README.md",
	}
	got := matchFiles(files, "main", 8)
	// Both main.go basenames lead (basename prefix), shorter path first.
	if len(got) < 2 || got[0] != "api/main.go" || got[1] != "cli/main.go" {
		t.Fatalf("basename-prefix ranking wrong: %v", got)
	}
	got = matchFiles(files, "cli/", 8)
	if len(got) != 3 || got[0] != "cli/main.go" {
		t.Fatalf("path-prefix matches wrong: %v", got)
	}
	// Subsequence still reachable.
	if got = matchFiles(files, "cmagt", 8); len(got) != 1 || got[0] != "cli/cmd/agent.go" {
		t.Fatalf("subsequence match wrong: %v", got)
	}
	if got = matchFiles(files, "zzz", 8); len(got) != 0 {
		t.Fatalf("no-match must be empty: %v", got)
	}
	// Empty query lists head of the corpus, capped.
	if got = matchFiles(files, "", 2); !reflect.DeepEqual(got, files[:2]) {
		t.Fatalf("empty query: %v", got)
	}
}

func TestLongestCommonPrefix(t *testing.T) {
	if p := longestCommonPrefix([]string{"cli/cmd/agent.go", "cli/cmd/arch.go"}); p != "cli/cmd/a" {
		t.Fatalf("lcp = %q", p)
	}
	if p := longestCommonPrefix(nil); p != "" {
		t.Fatalf("lcp(nil) = %q", p)
	}
	if p := longestCommonPrefix([]string{"a", "b"}); p != "" {
		t.Fatalf("disjoint lcp = %q", p)
	}
}
