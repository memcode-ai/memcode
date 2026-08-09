package structure

import (
	"os"
	"path/filepath"
	"strings"
)

// ResolvePath reports whether target names an existing path (relative to the
// working directory or the repo root) and returns its slash path relative to
// root. Shared by the context compiler and the provenance command.
func ResolvePath(root, target string) (rel string, isPath bool) {
	for _, c := range []string{target, filepath.Join(root, target)} {
		abs, err := filepath.Abs(c)
		if err != nil {
			continue
		}
		if _, err := os.Stat(abs); err != nil {
			continue
		}
		r, err := filepath.Rel(root, abs)
		if err != nil || strings.HasPrefix(r, "..") {
			continue
		}
		return filepath.ToSlash(r), true
	}
	return "", false
}

// ContainingSubsystem returns the subsystem whose key is the longest prefix of
// rel (the most specific enclosing subsystem).
func ContainingSubsystem(subs []Subsystem, rel string) (Subsystem, bool) {
	rel = filepath.ToSlash(rel)
	var best Subsystem
	bestLen := -1
	for _, s := range subs {
		if s.Key == "." {
			if bestLen < 0 {
				best, bestLen = s, 0
			}
			continue
		}
		if rel == s.Key || strings.HasPrefix(rel, s.Key+"/") {
			if len(s.Key) > bestLen {
				best, bestLen = s, len(s.Key)
			}
		}
	}
	return best, bestLen >= 0
}
