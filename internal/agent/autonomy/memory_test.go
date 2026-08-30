package autonomy

import (
	"strings"
	"testing"
)

func TestMemoryAppendReadAndDedup(t *testing.T) {
	home := t.TempDir()
	if got := ReadMemory(home); got != "" {
		t.Fatalf("fresh agent should have no memory, got %q", got)
	}
	if err := AppendMemory(home, "Tim is a US citizen and needs no visa sponsorship (he said so directly)."); err != nil {
		t.Fatal(err)
	}
	if err := AppendMemory(home, "Prefers backend roles at Series B-D startups."); err != nil {
		t.Fatal(err)
	}
	mem := ReadMemory(home)
	if !strings.Contains(mem, "US citizen") || !strings.Contains(mem, "Series B-D") {
		t.Fatalf("memory missing entries: %q", mem)
	}

	// Re-learning the same thing must not grow the file: every line is replayed
	// into the model on every future wake, so duplicates cost context forever.
	if err := AppendMemory(home, "prefers backend roles at series b-d startups."); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(ReadMemory(home), "Series B-D"); n != 1 {
		t.Fatalf("duplicate note recorded %d times", n)
	}

	// A multi-line note collapses to one line — the file is a list, and a
	// stray newline would fake a second entry.
	if err := AppendMemory(home, "line one\nline two"); err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(ReadMemory(home), "\n") {
		if strings.TrimSpace(line) == "line two" {
			t.Fatal("multi-line note split into separate entries")
		}
	}

	if err := AppendMemory(home, "   "); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ReadMemory(home), "- \n") {
		t.Fatal("blank note recorded")
	}
}
