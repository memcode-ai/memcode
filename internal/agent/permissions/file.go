package permissions

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Remembered approvals live in a plaintext file the user owns and can edit
// directly — NOT buried in the SQLite state. .memcode/state.db is for the event
// log and machine-derived projections; security policy ("what may run without
// asking") is the user's to read, edit, review in a PR, and check in.
//
// Format: one glob per line ('*' = wildcard); blank lines and '#' comments are
// ignored; a trailing tab + "trusted" marks a rule that may also match
// catastrophic commands. Rules are repo-wide (the file already lives in the repo).

const fileName = "permissions"

const fileHeader = `# memcode permissions — commands the agent may run without asking.
#
# One glob per line; '*' is a wildcard (e.g. "go test *", "git status").
# Blank lines and lines starting with '#' are ignored.
# Add a tab + "trusted" after a pattern to also allow matching catastrophic
# commands (rm -rf, git reset --hard, publish, cloud deploys). Use sparingly.
#
# Edit this file freely — it's yours. ` + "`memcode approve`" + ` appends here, and the
# "don't ask again" approval option adds a line too.
`

// FilePath is the permissions file for a repo rooted at root.
func FilePath(root string) string {
	return filepath.Join(root, ".memcode", fileName)
}

// Load parses the permissions file into approval rules. A missing file means no
// rules (not an error). Malformed lines are skipped, not fatal.
func Load(root string) ([]Approval, error) {
	data, err := os.ReadFile(FilePath(root))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Approval
	for _, line := range strings.Split(string(data), "\n") {
		if a, ok := parseLine(line); ok {
			out = append(out, a)
		}
	}
	return out, nil
}

// Append adds a rule (idempotent — a duplicate pattern is a no-op), creating the
// file with its header if needed. One O_APPEND|O_CREATE open + a single write:
// the old stat-then-create-then-reopen sequence raced concurrent writers (two
// processes could each "create" and one header/rule got clobbered). The dedupe
// check reads through the same open handle, so check-and-append can no longer
// interleave with another process's create.
func Append(root, pattern string, trusted bool) error {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return nil
	}
	path := FilePath(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	data, err := io.ReadAll(f) // reads from offset 0 (O_APPEND moves only writes)
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if a, ok := parseLine(line); ok && a.Pattern == pattern {
			return nil // already present
		}
	}
	var b strings.Builder
	if len(data) == 0 {
		b.WriteString(fileHeader) // brand-new file: header + rule land in ONE write
	}
	b.WriteString(pattern)
	if trusted {
		b.WriteString("\ttrusted")
	}
	b.WriteString("\n")
	_, err = f.WriteString(b.String()) // O_APPEND: a single atomic append
	return err
}

// Remove deletes every rule whose pattern equals pattern, rewriting the file.
// Returns whether anything was removed.
func Remove(root, pattern string) (bool, error) {
	pattern = strings.TrimSpace(pattern)
	data, err := os.ReadFile(FilePath(root))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var kept []string
	removed := false
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if a, ok := parseLine(line); ok && a.Pattern == pattern {
			removed = true
			continue
		}
		kept = append(kept, line)
	}
	if !removed {
		return false, nil
	}
	return true, os.WriteFile(FilePath(root), []byte(strings.Join(kept, "\n")+"\n"), 0o644)
}

// parseLine turns one file line into an Approval (repo-wide, never-expiring).
// Returns ok=false for blanks and comments.
func parseLine(line string) (Approval, bool) {
	// Strip only the line terminator, NOT internal whitespace: the trust marker is a
	// literal TAB + "trusted" (see Append), which must stay distinguishable from a pattern
	// that merely ends in the word "trusted" (`cat trusted` is the pattern `cat trusted`,
	// NOT a trusted `cat` rule). Splitting on any whitespace conflated the two — a
	// permission-escalating misparse of the user-editable file.
	line = strings.TrimRight(line, "\r\n")
	if t := strings.TrimSpace(line); t == "" || strings.HasPrefix(t, "#") {
		return Approval{}, false
	}
	trusted := false
	if strings.HasSuffix(line, "\ttrusted") {
		trusted = true
		line = line[:len(line)-len("\ttrusted")]
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return Approval{}, false
	}
	return Approval{Pattern: line, Trusted: trusted}, true
}
