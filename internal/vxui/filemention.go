package vxui

// @-file mentions and Tab path-completion for the composer.
//
// Typing "@" starts a file mention: a picker (rendered like the slash menu,
// sharing slashSel for the highlighted row) fuzzy-matches the token against
// the repo file list (repofiles.List — tracked + untracked-not-ignored, the
// same universe the glob tool sees). ↑↓ navigate, Tab/Enter complete the
// token in place; typing narrows. Tab outside a mention completes a bare
// path token against the same list (unique match wins, otherwise the longest
// common prefix extends).
//
// The file list is cached per repo root at the package level (one TUI per
// process) so the picker costs one git ls-files per session, refreshed when
// stale.

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/memcode-ai/memcode/internal/repofiles"
)

const fileMenuCap = 8

// --- repo file cache ---------------------------------------------------------

var fileCache struct {
	sync.Mutex
	root    string
	files   []string
	fetched time.Time
}

const fileCacheTTL = 30 * time.Second

// repoFiles returns the (cached) repo file list for root.
func repoFiles(ctx context.Context, root string) []string {
	fileCache.Lock()
	defer fileCache.Unlock()
	if fileCache.root == root && time.Since(fileCache.fetched) < fileCacheTTL {
		return fileCache.files
	}
	fileCache.root = root
	fileCache.files = repofiles.List(ctx, root)
	fileCache.fetched = time.Now()
	return fileCache.files
}

// --- token under the cursor ----------------------------------------------------

// tokenAt returns the whitespace-delimited token containing the insertion
// point (scanning left from cursor) and its rune start index. A cursor at the
// very start, or right after whitespace, yields an empty token.
func tokenAt(runes []rune, cursor int) (start int, tok string) {
	if cursor > len(runes) {
		cursor = len(runes)
	}
	start = cursor
	for start > 0 && !isSpaceRune(runes[start-1]) {
		start--
	}
	return start, string(runes[start:cursor])
}

func isSpaceRune(r rune) bool { return r == ' ' || r == '\t' || r == '\n' }

// mentionQuery extracts the active @-mention query from the composer: the
// token under the cursor when it starts with "@" (and the "@" opens the token,
// so emails like a@b don't trigger). ok=false when no mention is active.
func mentionQuery(runes []rune, cursor int) (start int, query string, ok bool) {
	start, tok := tokenAt(runes, cursor)
	if !strings.HasPrefix(tok, "@") {
		return 0, "", false
	}
	return start, tok[1:], true
}

// --- matching ------------------------------------------------------------------

// matchFiles ranks repo paths against a query: basename prefix beats path
// prefix beats basename substring beats path substring beats subsequence.
// Case-insensitive; ties break on shorter, then lexicographic, path. An empty
// query returns the first files as-is (something to pick from right away).
func matchFiles(files []string, query string, limit int) []string {
	if query == "" {
		if len(files) > limit {
			return files[:limit]
		}
		return files
	}
	q := strings.ToLower(query)
	type scored struct {
		path  string
		score int
	}
	var out []scored
	for _, f := range files {
		lf := strings.ToLower(f)
		base := lf
		if i := strings.LastIndexByte(lf, '/'); i >= 0 {
			base = lf[i+1:]
		}
		var sc int
		switch {
		case strings.HasPrefix(base, q):
			sc = 5
		case strings.HasPrefix(lf, q):
			sc = 4
		case strings.Contains(base, q):
			sc = 3
		case strings.Contains(lf, q):
			sc = 2
		case isSubsequence(q, lf):
			sc = 1
		default:
			continue
		}
		out = append(out, scored{f, sc})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].score != out[j].score {
			return out[i].score > out[j].score
		}
		if len(out[i].path) != len(out[j].path) {
			return len(out[i].path) < len(out[j].path)
		}
		return out[i].path < out[j].path
	})
	if len(out) > limit {
		out = out[:limit]
	}
	paths := make([]string, len(out))
	for i, s := range out {
		paths[i] = s.path
	}
	return paths
}

func isSubsequence(needle, hay string) bool {
	nr := []rune(needle)
	i := 0
	for _, r := range hay {
		if i < len(nr) && r == nr[i] {
			i++
		}
	}
	return i == len(nr)
}

// longestCommonPrefix of a candidate set (for bare-Tab completion).
func longestCommonPrefix(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	p := paths[0]
	for _, s := range paths[1:] {
		for !strings.HasPrefix(s, p) {
			p = p[:len(p)-1]
			if p == "" {
				return ""
			}
		}
	}
	return p
}

// --- appState integration --------------------------------------------------------

// fileMenu returns the @-mention matches for the current composer/cursor, or
// nil when no mention is active. Shares slashSel with the slash menu (only one
// can be open — a slash menu needs the composer to START with "/", a mention
// needs an @token under the cursor; "/cmd @x" contains a space so the slash
// menu is closed).
func (s *appState) fileMenu() []string {
	runes := []rune(s.composer)
	_, q, ok := mentionQuery(runes, s.cursor)
	if !ok {
		return nil
	}
	return matchFiles(repoFiles(s.w.ctx, s.w.sess.Root()), q, fileMenuCap)
}

// completeMention replaces the active @token with "@<path> " and moves the
// cursor after it.
func (s *appState) completeMention(path string) {
	runes := []rune(s.composer)
	start, _, ok := mentionQuery(runes, s.cursor)
	if !ok {
		return
	}
	ins := "@" + path + " "
	s.composer = string(runes[:start]) + ins + string(runes[s.cursor:])
	s.cursor = start + len([]rune(ins))
	s.slashSel = 0
}

// fileMenuKey handles one key while the mention picker is open. Returns true
// when the key was consumed (↑↓ navigate, Tab/Enter complete). Everything else
// falls through so typing keeps narrowing the query.
func (s *appState) fileMenuKey(ks string, menu []string) bool {
	sel := s.clampedSel(len(menu))
	switch ks {
	case "Up":
		if sel > 0 {
			s.SetState(func() { s.slashSel = sel - 1 })
		}
		return true
	case "Down":
		if sel < len(menu)-1 {
			s.SetState(func() { s.slashSel = sel + 1 })
		}
		return true
	case "Tab", "Enter":
		s.SetState(func() { s.completeMention(menu[sel]) })
		return true
	}
	return false
}

// tabCompletePath completes a bare path token under the cursor (no "@"):
// a unique prefix match completes fully; several matches extend to their
// longest common prefix. Returns true when the composer changed.
func (s *appState) tabCompletePath() bool {
	runes := []rune(s.composer)
	start, tok := tokenAt(runes, s.cursor)
	if tok == "" || strings.HasPrefix(tok, "@") || strings.HasPrefix(tok, "/") {
		return false // empty, a mention (own flow), or a slash command
	}
	var cands []string
	for _, f := range repoFiles(s.w.ctx, s.w.sess.Root()) {
		if strings.HasPrefix(f, tok) {
			cands = append(cands, f)
		}
	}
	replace := func(text string) {
		s.composer = string(runes[:start]) + text + string(runes[s.cursor:])
		s.cursor = start + len([]rune(text))
		s.slashSel = 0
	}
	switch {
	case len(cands) == 0:
		return false
	case len(cands) == 1:
		s.SetState(func() { replace(cands[0] + " ") })
		return true
	default:
		lcp := longestCommonPrefix(cands)
		if len(lcp) <= len(tok) {
			return false // nothing more to extend
		}
		s.SetState(func() { replace(lcp) })
		return true
	}
}
