package runtime

import (
	"path/filepath"
	"strings"
)

// Optional observer interfaces the stream-json protocol driver implements to
// receive structured events the TUI renders on its own (diffs, tool activity).
// The runtime checks for them at the emit sites via a type assertion; a plain
// UIObserver that doesn't implement them (the TUI, the nop) simply gets nothing
// extra, so this stays additive and touches no existing implementer.
type diffEmitter interface {
	EmitDiff(path, language, unified string, added, removed int, newFile bool)
}
type toolEmitter interface {
	EmitTool(name, target string, failed bool)
}

// emitDiff forwards a structured file change to a diffEmitter observer, if any.
// For a new file, body is the file content (counted as all-additions); otherwise
// body is a unified diff.
func (s *Session) emitDiff(path, body string, newFile bool) {
	de, ok := s.observer.(diffEmitter)
	if !ok || body == "" {
		return
	}
	var added, removed int
	if newFile {
		added = strings.Count(strings.TrimRight(body, "\n"), "\n") + 1
	} else {
		added, removed = diffCounts(body)
	}
	de.EmitDiff(path, diffLang(path), body, added, removed, newFile)
}

// emitTool forwards tool activity to a toolEmitter observer, if any.
func (s *Session) emitTool(name, target string, failed bool) {
	if te, ok := s.observer.(toolEmitter); ok {
		te.EmitTool(name, target, failed)
	}
}

func diffCounts(unified string) (added, removed int) {
	for _, line := range strings.Split(unified, "\n") {
		switch {
		case strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++"):
			added++
		case strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---"):
			removed++
		}
	}
	return added, removed
}

func diffLang(path string) string {
	return strings.TrimPrefix(filepath.Ext(path), ".")
}
