// Package scripts is the repo-local store of reusable multi-step command sequences — a
// proven recipe ("rebuild the cli", "commit, push, deploy") saved once and replayed by name
// instead of re-derived every time. Each script is a real shell file at
// .memcode/scripts/<slug>.sh: a shebang line followed by a hand-parsed `#`-comment metadata
// header (description/created/last-run/run-count — no YAML dependency, matching the
// internal/skills package's existing convention) and then the command body.
//
// .memcode is gitignored, so scripts are per-machine and a hard delete isn't
// git-recoverable — Delete therefore moves a script to a .trash/ subdirectory rather than
// removing it outright.
package scripts

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/memcode-ai/memcode/internal/atomicfile"
)

// Script is one saved command sequence.
type Script struct {
	Slug        string
	Description string
	Body        string
	CreatedAt   time.Time
	LastRunAt   time.Time // zero value: never run
	RunCount    int
	Path        string
}

// slugRe is the same lowercase-hyphen charset the skills package validates against.
var slugRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// ValidSlug reports whether slug is the required lowercase-letters/digits/hyphens charset.
func ValidSlug(slug string) bool {
	return slugRe.MatchString(slug)
}

// Dir returns the repo-local scripts directory (.memcode/scripts). It may not exist yet.
func Dir(root string) string {
	return filepath.Join(root, ".memcode", "scripts")
}

// trashDir is where Delete parks a removed script instead of hard-deleting it.
func trashDir(root string) string {
	return filepath.Join(Dir(root), ".trash")
}

func scriptPath(root, slug string) string {
	return filepath.Join(Dir(root), slug+".sh")
}

// List returns every saved (non-trashed) script, sorted by slug. An absent directory is not
// an error — it just means nothing has been saved yet.
func List(root string) ([]Script, error) {
	dir := Dir(root)
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Script
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sh") {
			continue
		}
		sc, err := load(filepath.Join(dir, e.Name()))
		if err != nil {
			continue // skip anything unreadable/malformed rather than fail the whole list
		}
		out = append(out, sc)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out, nil
}

// Get returns one saved script by exact slug.
func Get(root, slug string) (Script, bool) {
	sc, err := load(scriptPath(root, slug))
	if err != nil {
		return Script{}, false
	}
	return sc, true
}

// Save writes (or upserts) a script under .memcode/scripts/<slug>.sh. Validates the slug and
// requires a description and a non-empty body; updating an existing slug preserves its
// CreatedAt/RunCount/LastRunAt history rather than resetting it.
func Save(root, slug, description, body string) (Script, error) {
	slug = strings.TrimSpace(slug)
	if !ValidSlug(slug) {
		return Script{}, fmt.Errorf("script slug must be lowercase letters, digits, and hyphens (got %q)", slug)
	}
	description = strings.TrimSpace(description)
	if description == "" {
		return Script{}, errors.New("script description is required")
	}
	body = strings.TrimRight(body, "\n")
	if strings.TrimSpace(body) == "" {
		return Script{}, errors.New("script command is required")
	}
	sc := Script{Slug: slug, Description: description, Body: body, CreatedAt: time.Now().UTC()}
	if existing, err := load(scriptPath(root, slug)); err == nil {
		sc.CreatedAt = existing.CreatedAt
		sc.RunCount = existing.RunCount
		sc.LastRunAt = existing.LastRunAt
	}
	if err := os.MkdirAll(Dir(root), 0o755); err != nil {
		return Script{}, err
	}
	p := scriptPath(root, slug)
	if err := atomicfile.WriteFile(p, []byte(render(sc)), 0o755); err != nil {
		return Script{}, err
	}
	sc.Path = p
	return sc, nil
}

// RecordRun best-effort bumps the run counter and last-run timestamp after a successful run.
// Errors are swallowed — a lost stat bump is never worth failing the command that just ran.
func RecordRun(root, slug string) {
	p := scriptPath(root, slug)
	sc, err := load(p)
	if err != nil {
		return
	}
	sc.RunCount++
	sc.LastRunAt = time.Now().UTC()
	_ = atomicfile.WriteFile(p, []byte(render(sc)), 0o755)
}

// Delete soft-deletes a script: moves it to .memcode/scripts/.trash/<slug>-<unixnano>.sh
// rather than removing it outright, since .memcode is gitignored (a hard delete wouldn't be
// git-recoverable).
func Delete(root, slug string) error {
	p := scriptPath(root, slug)
	if _, err := os.Stat(p); err != nil {
		return fmt.Errorf("no saved script named %q", slug)
	}
	trash := trashDir(root)
	if err := os.MkdirAll(trash, 0o755); err != nil {
		return err
	}
	dest := filepath.Join(trash, fmt.Sprintf("%s-%d.sh", slug, time.Now().UnixNano()))
	return os.Rename(p, dest)
}

// render serializes a Script back to its on-disk shebang + comment-header + body form.
func render(sc Script) string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("# description: " + sc.Description + "\n")
	b.WriteString("# created: " + sc.CreatedAt.Format(time.RFC3339) + "\n")
	if !sc.LastRunAt.IsZero() {
		b.WriteString("# last-run: " + sc.LastRunAt.Format(time.RFC3339) + "\n")
	}
	b.WriteString("# run-count: " + strconv.Itoa(sc.RunCount) + "\n")
	b.WriteString("\n")
	b.WriteString(sc.Body)
	b.WriteString("\n")
	return b.String()
}

// load reads and parses one script file: an optional shebang, then a contiguous run of
// `# key: value` header comments, a blank-line separator, then the body (everything else).
func load(p string) (Script, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return Script{}, err
	}
	lines := strings.Split(string(b), "\n")
	i := 0
	if i < len(lines) && strings.HasPrefix(lines[i], "#!") {
		i++
	}
	sc := Script{Slug: strings.TrimSuffix(filepath.Base(p), ".sh"), Path: p}
	for i < len(lines) && strings.HasPrefix(lines[i], "#") {
		k, v, found := strings.Cut(strings.TrimPrefix(lines[i], "#"), ":")
		if found {
			switch strings.TrimSpace(k) {
			case "description":
				sc.Description = strings.TrimSpace(v)
			case "created":
				if t, err := time.Parse(time.RFC3339, strings.TrimSpace(v)); err == nil {
					sc.CreatedAt = t
				}
			case "last-run":
				if t, err := time.Parse(time.RFC3339, strings.TrimSpace(v)); err == nil {
					sc.LastRunAt = t
				}
			case "run-count":
				if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
					sc.RunCount = n
				}
			}
		}
		i++
	}
	for i < len(lines) && strings.TrimSpace(lines[i]) == "" {
		i++
	}
	sc.Body = strings.TrimRight(strings.Join(lines[i:], "\n"), "\n")
	if strings.TrimSpace(sc.Body) == "" {
		return Script{}, fmt.Errorf("script %s has no body", p)
	}
	if sc.CreatedAt.IsZero() {
		if fi, err := os.Stat(p); err == nil {
			sc.CreatedAt = fi.ModTime() // fallback for a hand-edited file missing the header
		}
	}
	return sc, nil
}
