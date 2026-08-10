package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/memcode-ai/memcode/internal/agent/tools"
)

// code_query is a DETERMINISTIC codebase oracle: it answers "where does X live /
// how does Y work" by decomposing the question into terms, searching with ripgrep,
// and RANKING the hits into evidence — all in cheap local code, with NO model calls.
// It's a non-LLM mini-scout: where a ranked text+structure pass can locate the
// answer, it collapses a grep→read→grep model loop into one tool call + curated
// evidence. v1 is lexical + a light declaration boost (no embeddings, no full
// parser — those come later if this earns it). Returns evidence, never a verdict:
// the model still reads the top hits to confirm.

// cqStopwords are dropped from a natural-language query before searching.
var cqStopwords = map[string]bool{
	"the": true, "a": true, "an": true, "is": true, "are": true, "was": true, "were": true,
	"where": true, "what": true, "which": true, "who": true, "how": true, "why": true, "when": true,
	"does": true, "do": true, "did": true, "can": true, "could": true, "should": true, "would": true,
	"in": true, "on": true, "of": true, "to": true, "for": true, "and": true, "or": true, "with": true,
	"it": true, "this": true, "that": true, "these": true, "those": true, "we": true, "you": true,
	"code": true, "file": true, "files": true, "use": true, "used": true, "uses": true, "get": true,
}

var cqToken = regexp.MustCompile(`[A-Za-z0-9_]+`)

// cqDecl matches a line that LOOKS like a declaration (definition), across common
// languages — so "where X is defined" ranks above where it's merely mentioned.
var cqDecl = regexp.MustCompile(`(?i)\b(func|type|class|interface|struct|enum|trait|impl|def|fn|const|let|var|public|private|protected|static|export|function|module)\b`)

func cqTokenize(q string) []string {
	seen := map[string]bool{}
	var terms []string
	for _, t := range cqToken.FindAllString(strings.ToLower(q), -1) {
		if len(t) < 3 || cqStopwords[t] || seen[t] {
			continue
		}
		seen[t] = true
		terms = append(terms, t)
	}
	return terms
}

type cqHit struct {
	line int
	text string
	hits map[string]bool // which terms this line contains
	decl bool
}

type cqFile struct {
	path     string
	distinct map[string]bool // distinct terms anywhere in this file's matches
	pathHit  int             // terms appearing in the path itself
	best     []cqHit         // top evidence lines
	score    int
}

func (s *Session) codeQuery(ctx context.Context, input json.RawMessage) toolResult {
	var in tools.CodeQueryInput
	if err := json.Unmarshal(input, &in); err != nil {
		return errResult(err.Error())
	}
	// Hidden tier: internal machinery, no marker at all (see toolLineStat's tier contract).
	text, ok := CodeQuery(ctx, s.root, in.Query, in.Scope)
	if !ok {
		return errResult(text)
	}
	return textResult(text)
}

// CodeQuery is the deterministic "where does X live" oracle over root — a
// ranked search with no model loop. Shared by the agent tool (Session.codeQuery)
// and the standalone `memcode mcp serve` memory server, so an external agent
// gets the same answer. ok=false marks a usage error (no terms, bad scope,
// timeout); a no-match result is ok=true with a guidance string.
func CodeQuery(ctx context.Context, root, query, scope string) (string, bool) {
	terms := cqTokenize(query)
	if len(terms) == 0 {
		return "code_query: couldn't extract search terms — use ripgrep with a specific string.", false
	}
	sctx, cancel := context.WithTimeout(ctx, searchTimeout)
	defer cancel()
	sc := "."
	if scope != "" {
		if _, err := safeJoin(root, scope); err != nil {
			return err.Error(), false
		}
		sc = scope
	}
	alt := "(" + strings.Join(terms, "|") + ")"

	var cmd *exec.Cmd
	switch {
	case hasExec("rg"):
		cmd = exec.CommandContext(sctx, "rg", "--line-number", "--no-heading", "--color", "never",
			"-i", "--glob", "!**/node_modules/**", "-e", alt, sc)
	case inGitRepo(sctx, root):
		cmd = exec.CommandContext(sctx, "git", "-C", root, "grep", "-nI", "--no-color", "--untracked",
			"-i", "-E", "-e", alt, "--", sc, ":(exclude,glob)**/node_modules/**")
	default:
		cmd = exec.CommandContext(sctx, "grep", "-rnIE", "-i", alt, sc)
	}
	cmd.Dir = root
	out, _ := cmd.Output()
	if sctx.Err() == context.DeadlineExceeded {
		return "code_query timed out — narrow the query or pass a `scope`.", false
	}

	files := cqRank(string(out), terms)
	if len(files) == 0 {
		return fmt.Sprintf("code_query: no matches for terms %v — try ripgrep with a precise string, or widen the scope.", terms), true
	}
	return cqFormat(query, terms, files), true
}

// cqRank parses `path:line:text` matches, groups by file, and scores each file by
// how much of the query it covers + path matches + whether it holds a declaration.
func cqRank(rgOut string, terms []string) []cqFile {
	byPath := map[string]*cqFile{}
	lines := strings.Split(strings.TrimRight(rgOut, "\n"), "\n")
	const maxLines = 4000
	for i, ln := range lines {
		if i >= maxLines || ln == "" {
			continue
		}
		// path:line:content  (path may contain no colon on the left of the first two)
		p1 := strings.IndexByte(ln, ':')
		if p1 < 0 {
			continue
		}
		p2 := strings.IndexByte(ln[p1+1:], ':')
		if p2 < 0 {
			continue
		}
		path := ln[:p1]
		lineNo, _ := strconv.Atoi(ln[p1+1 : p1+1+p2])
		text := ln[p1+1+p2+1:]
		low := strings.ToLower(text)

		f := byPath[path]
		if f == nil {
			lowPath := strings.ToLower(path)
			f = &cqFile{path: path, distinct: map[string]bool{}}
			for _, t := range terms {
				if strings.Contains(lowPath, t) {
					f.pathHit++
				}
			}
			byPath[path] = f
		}
		h := cqHit{line: lineNo, text: strings.TrimSpace(text), hits: map[string]bool{}, decl: cqDecl.MatchString(text)}
		for _, t := range terms {
			if strings.Contains(low, t) {
				h.hits[t] = true
				f.distinct[t] = true
			}
		}
		f.best = append(f.best, h)
	}

	out := make([]cqFile, 0, len(byPath))
	for _, f := range byPath {
		// Evidence: declarations first, then term-density, cap at 3 lines.
		sort.SliceStable(f.best, func(i, j int) bool {
			if f.best[i].decl != f.best[j].decl {
				return f.best[i].decl
			}
			return len(f.best[i].hits) > len(f.best[j].hits)
		})
		if len(f.best) > 3 {
			f.best = f.best[:3]
		}
		decl := 0
		for _, h := range f.best {
			if h.decl {
				decl = 1
				break
			}
		}
		isTest := strings.Contains(f.path, "_test.") || strings.Contains(f.path, ".test.") || strings.Contains(f.path, "/test")
		f.score = len(f.distinct)*3 + f.pathHit*4 + decl*2
		if isTest {
			f.score -= 1 // tests are evidence, but the implementation usually ranks first
		}
		out = append(out, *f)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].score > out[j].score })
	if len(out) > 8 {
		out = out[:8]
	}
	return out
}

func cqFormat(query string, terms []string, files []cqFile) string {
	top := files[0].score
	band := func(s int) string {
		switch {
		case top > 0 && s >= top:
			return "high"
		case top > 0 && s*2 >= top:
			return "medium"
		default:
			return "low"
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "code_query %q  (terms: %s)\n", query, strings.Join(terms, ", "))
	fmt.Fprintf(&b, "ranked candidates (read_file the top ones to confirm):\n")
	for _, f := range files {
		matched := make([]string, 0, len(f.distinct))
		for _, t := range terms {
			if f.distinct[t] {
				matched = append(matched, t)
			}
		}
		fmt.Fprintf(&b, "\n▸ %s  [%s]  matched: %s\n", f.path, band(f.score), strings.Join(matched, ", "))
		for _, h := range f.best {
			mark := "  "
			if h.decl {
				mark = "» " // a declaration — likely where it's defined
			}
			fmt.Fprintf(&b, "    %s%d: %s\n", mark, h.line, clip(h.text, 160))
		}
	}
	return truncate(b.String(), maxToolOutput)
}
