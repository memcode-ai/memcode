package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// layoutMaxEntries bounds the layout fact: enough for a real top level, small
// enough to stay a glance in the cached prompt prefix.
const layoutMaxEntries = 40

// layoutFact is a one-glance, deterministic map of the root's top level: each
// folder marked as its own git repository, empty, or with how many entries it
// holds, then the top-level files. It is a FACT the model reads before deciding
// where to look, so research starts from the real shape of the folder: a
// workspace of unrelated repos, a monorepo, or one project, and whether the
// folder a task names is a fresh start. Hidden entries are skipped. "" when the
// root can't be read.
func layoutFact(root string) string {
	ents, err := os.ReadDir(root)
	if err != nil {
		return ""
	}
	var dirs, files []string
	for _, e := range ents {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		if e.IsDir() {
			dirs = append(dirs, describeDir(filepath.Join(root, name), name))
		} else {
			files = append(files, name)
		}
	}
	sort.Strings(dirs)
	sort.Strings(files)
	lines := append([]string(nil), dirs...)
	if len(files) > 0 {
		lines = append(lines, "files: "+strings.Join(files, ", "))
	}
	if len(lines) == 0 {
		return "(empty)"
	}
	if len(lines) > layoutMaxEntries {
		more := len(lines) - layoutMaxEntries
		lines = append(lines[:layoutMaxEntries], fmt.Sprintf("… and %d more", more))
	}
	return strings.Join(lines, "\n")
}

// describeDir is one layout line: "name/ — git repo · 48 entries", "name/ — empty".
// It reads one directory level only, so it stays cheap on any tree.
func describeDir(path, name string) string {
	ents, err := os.ReadDir(path)
	if err != nil {
		return name + "/"
	}
	visible, repo := 0, false
	for _, e := range ents {
		switch {
		case e.Name() == ".git":
			repo = true
		case strings.HasPrefix(e.Name(), "."):
		default:
			visible++
		}
	}
	var parts []string
	if repo {
		parts = append(parts, "git repo")
	}
	switch visible {
	case 0:
		parts = append(parts, "empty")
	case 1:
		parts = append(parts, "1 entry")
	default:
		parts = append(parts, fmt.Sprintf("%d entries", visible))
	}
	return name + "/ — " + strings.Join(parts, " · ")
}
