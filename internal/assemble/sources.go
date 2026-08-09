package assemble

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/memcode-ai/memcode/internal/repofiles"
	"github.com/memcode-ai/memcode/internal/structure"
)

// targetFiles returns the file(s) the target directly names.
func targetFiles(root, rel string) []Item {
	abs := filepath.Join(root, rel)
	info, err := os.Stat(abs)
	if err != nil {
		return nil
	}
	if !info.IsDir() {
		return []Item{{Ref: rel, Reason: "the target file", Score: 1000}}
	}
	des, err := os.ReadDir(abs)
	if err != nil {
		return nil
	}
	var out []Item
	for _, de := range des {
		if de.IsDir() || strings.HasPrefix(de.Name(), ".") {
			continue
		}
		out = append(out, Item{
			Ref:    filepath.ToSlash(filepath.Join(rel, de.Name())),
			Reason: "in the target directory",
			Score:  500,
		})
	}
	return out
}

// keyFiles surfaces the files most worth reading in a subsystem.
func keyFiles(root string, sub structure.Subsystem) []Item {
	dir := filepath.Join(root, sub.Key)
	var out []Item
	add := func(name, reason string, score int) {
		if fileExists(filepath.Join(dir, name)) {
			out = append(out, Item{
				Ref:    filepath.ToSlash(filepath.Join(sub.Key, name)),
				Reason: reason,
				Score:  score,
			})
		}
	}
	if sub.Manifest != "" {
		add(sub.Manifest, "subsystem manifest", 300)
	}
	add("README.md", "subsystem overview", 250)
	for _, entry := range []string{"main.go", "index.ts", "index.js", "mod.rs", "lib.rs", "__init__.py"} {
		add(entry, "subsystem entry point", 200)
	}
	return out
}

type match struct {
	path  string
	count int
}

// searchFiles is a Go-native, dependency-free content search (case-insensitive)
// over the project's non-ignored files. Bounded so it stays fast on large repos.
func searchFiles(ctx context.Context, root, query string) []match {
	needle := bytes.ToLower([]byte(query))
	if len(needle) == 0 {
		return nil
	}
	const (
		maxFiles    = 2500
		maxReadSize = 256 * 1024
		maxResults  = 20
	)
	var results []match
	scanned := 0
	for _, rel := range repofiles.List(ctx, root) {
		if scanned >= maxFiles {
			break
		}
		if isBinaryName(filepath.Base(rel)) {
			continue
		}
		scanned++
		if c := countMatches(filepath.Join(root, rel), needle, maxReadSize); c > 0 {
			results = append(results, match{path: rel, count: c})
		}
	}

	sort.SliceStable(results, func(i, j int) bool {
		if results[i].count != results[j].count {
			return results[i].count > results[j].count
		}
		return results[i].path < results[j].path
	})
	if len(results) > maxResults {
		results = results[:maxResults]
	}
	return results
}

func countMatches(path string, needle []byte, limit int) int {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	buf := make([]byte, limit)
	n, _ := io.ReadFull(f, buf)
	if n == 0 {
		return 0
	}
	return bytes.Count(bytes.ToLower(buf[:n]), needle)
}

// recommendedReads is an ordered, deduped reading list within budget reach.
func recommendedReads(root string, topo structure.Result, sub structure.Subsystem, haveSub bool, rel string, isPath bool) []Item {
	var out []Item
	seen := map[string]bool{}
	add := func(ref, reason string) {
		if ref == "" || seen[ref] || !fileExists(filepath.Join(root, ref)) {
			return
		}
		seen[ref] = true
		out = append(out, Item{Ref: ref, Reason: reason})
	}

	for _, doc := range topo.Docs {
		if doc == "README.md" || doc == "CLAUDE.md" || doc == "AGENTS.md" {
			add(doc, "project intent / conventions")
		}
	}
	if haveSub {
		if sub.Manifest != "" {
			add(filepath.ToSlash(filepath.Join(sub.Key, sub.Manifest)), "subsystem manifest")
		}
		add(filepath.ToSlash(filepath.Join(sub.Key, "README.md")), "subsystem overview")
	}
	if isPath {
		if info, err := os.Stat(filepath.Join(root, rel)); err == nil && !info.IsDir() {
			add(rel, "the target")
		}
	}
	if len(out) > 6 {
		out = out[:6]
	}
	return out
}

func budgetFor(root string, reads []Item) Budget {
	b := Budget{Limit: defaultTokenBudget}
	for _, it := range reads {
		if info, err := os.Stat(filepath.Join(root, it.Ref)); err == nil {
			b.Estimated += int(info.Size()) / 4 // ~4 bytes/token
		}
	}
	b.Truncated = b.Estimated > b.Limit
	return b
}

// --- small fs helpers ---

func isBinaryName(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".ico", ".pdf", ".zip", ".gz",
		".tar", ".exe", ".dll", ".so", ".dylib", ".bin", ".db", ".wasm", ".lock":
		return true
	}
	return false
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
