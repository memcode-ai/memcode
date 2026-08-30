package autonomy

import (
	"os"
	"path/filepath"
	"strings"
)

// memoryFile is the agent's durable semantic memory — the same memory.md every
// memcode agent already has in its home, injected into ordinary conversations
// by the runtime. An unattended agent writes to it with the `remember` tool and
// reads it back on every wake, so what it learns once ("Tim is a US citizen and
// needs no sponsorship") is known forever and never asked again.
//
// This replaces a structured `facts` table that carried key/value/source/
// confirmed/sensitivity. That table's provenance was never actually enforced —
// nothing read Confirmed to gate anything — and it meant an agent had two
// unrelated places to put what it knew, only one of which a human could read.
//
// The tradeoff is deliberate and worth naming: prose cannot distinguish "you
// told me this" from "I inferred it from your resume" from "a website said so",
// nor mark a claim as safe to assert on the user's behalf, nor mark it stale.
// That distinction becomes load-bearing the moment an agent fills in a form or
// sends a message stating something about the user. When that lands, structured
// provenance should come back as its own thing in the store (machine-checkable,
// alongside policies and the action journal) — not by reviving this table.
const memoryFile = "memory.md"

func memoryPath(home string) string { return filepath.Join(home, memoryFile) }

// ReadMemory returns the agent's memory, or "" when it has none yet.
func ReadMemory(home string) string {
	b, err := os.ReadFile(memoryPath(home))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// AppendMemory adds one durable note. Append-only and deduplicated: a wake that
// re-learns something it already recorded must not grow the file without bound,
// since every line is replayed into the model on every future wake.
func AppendMemory(home, note string) error {
	note = strings.TrimSpace(strings.ReplaceAll(note, "\n", " "))
	if note == "" {
		return nil
	}
	existing := ReadMemory(home)
	for _, line := range strings.Split(existing, "\n") {
		if strings.EqualFold(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "- ")), note) {
			return nil // already known
		}
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(memoryPath(home), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	var b strings.Builder
	if existing == "" {
		b.WriteString("# Memory\n\n")
	}
	b.WriteString("- " + note + "\n")
	_, err = f.WriteString(b.String())
	return err
}
