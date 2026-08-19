// Package overview synthesizes a CANONICAL current-state overview of a project —
// the answer to "what is this now?" — from fresh signals (recent commits, active
// objectives, current claims, recent-active subsystems) rather than rebuilding it
// from random old docs/memories each time. The result is cached keyed by HEAD, so
// the overview is stable between commits and regenerated when work moves on.
//
// This exists to fix "sounding smart while being stale": a current-state question
// is a SYNTHESIS task over current evidence, not a recall over old framing.
package overview

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/memcode-ai/memcode/internal/gitlog"
	"github.com/memcode-ai/memcode/internal/llm"
	"github.com/memcode-ai/memcode/internal/objectives"
	"github.com/memcode-ai/memcode/internal/stack"
	"github.com/memcode-ai/memcode/internal/store"
	"github.com/memcode-ai/memcode/internal/structure"
)

// Overview is the synthesized current-state summary, cached in current_state.
// It records the git SNAPSHOT it was generated against (branch + HEAD + a hash of
// the dirty working tree). The overview is a DERIVED artifact of git truth: any
// change to that snapshot — a commit, a branch switch, an edit — invalidates it.
// Git state is the freshness boundary, never time. "verified green but not yet
// committed" must never survive a commit.
type Overview struct {
	Text        string    `json:"text"`
	HeadSHA     string    `json:"head_sha"`
	Branch      string    `json:"branch,omitempty"`
	DirtyHash   string    `json:"dirty_hash,omitempty"` // hash of `git status --porcelain`; "" = clean
	GeneratedAt time.Time `json:"generated_at"`
}

// repoSnapshot is memcode's canonical view of git truth at a moment: the branch,
// the committed HEAD, and a hash of the uncommitted working tree.
type repoSnapshot struct {
	Branch    string
	HeadSHA   string
	DirtyHash string
}

// matches reports whether an overview was generated against this exact git state.
// A snapshot we couldn't read (no HeadSHA) matches NOTHING — we regenerate rather
// than ever serve a summary whose truth we can't verify.
func (s repoSnapshot) matches(o Overview) bool {
	return s.HeadSHA != "" && o.HeadSHA == s.HeadSHA && o.Branch == s.Branch && o.DirtyHash == s.DirtyHash
}

const (
	stateScope = "repo"
	stateLayer = "overview"
)

// Load returns the cached overview and whether it's still fresh — fresh meaning it
// was generated against the CURRENT git snapshot (branch + HEAD + dirty state). A
// commit, branch switch, or edit since generation makes it stale.
func Load(ctx context.Context, st store.Store, root string) (Overview, bool) {
	state, ok, err := st.GetState(ctx, stateScope, stateLayer)
	if err != nil || !ok || len(state.Body) == 0 {
		return Overview{}, false
	}
	var o Overview
	if json.Unmarshal(state.Body, &o) != nil {
		return Overview{}, false
	}
	return o, snapshot(ctx, root).matches(o)
}

// Synthesize gathers current-state evidence, makes one model call to produce a
// concise current overview, caches it keyed by the git snapshot, and returns it.
// Synthesize builds the overview. It is now a DETERMINISTIC cockpit briefing —
// rendered from facts (root README identity, the subsystem topology + each
// component's own doc, the parsed StackFacts, recent commits, churn-hot dirs), NOT
// a model summary. The model used to flatten these same facts into a prose blurb and
// invent component internals ("Firestore: conversation history"); rendering facts
// directly is crisp like `memcode stack` and can't hallucinate. runner/model are
// unused (kept for the cache-API signature).
func Synthesize(ctx context.Context, st store.Store, runner llm.ModelRunner, root, model string) (Overview, error) {
	snap := snapshot(ctx, root)
	o := Overview{
		Text: briefing(ctx, st, root, snap), HeadSHA: snap.HeadSHA, Branch: snap.Branch,
		DirtyHash: snap.DirtyHash, GeneratedAt: time.Now().UTC(),
	}
	body, _ := json.Marshal(o)
	_ = st.PutState(ctx, store.State{Scope: stateScope, Layer: stateLayer, Body: body, RefreshedAt: o.GeneratedAt})
	return o, nil
}

// briefing renders the deterministic overview: Header / Purpose / Architecture /
// Stack / Current motion / Entry points. Every line is grounded in a fact source;
// nothing is inferred or invented.
func briefing(ctx context.Context, st store.Store, root string, snap repoSnapshot) string {
	var b strings.Builder
	name := filepath.Base(root)
	identity := repoIdentity(root)
	facts, _ := (stack.LocalStackDetector{}).Detect(ctx, root)

	b.WriteString(name)
	if identity != "" {
		b.WriteString("\n\nPurpose")
		for _, ln := range wrapText(identity, 70) { // prose reads better narrow; tables stay full
			b.WriteString("\n  " + ln)
		}
	}

	// Repo — VCS + monorepo/workspace shape (git · monorepo · Turborepo …).
	if rl := stack.RepoLine(facts.Repo); rl != "" {
		b.WriteString("\n\nRepo\n  " + rl)
	}

	// Architecture — real subsystems, each role from its OWN doc (no guessing). Shared
	// config/tooling packages get a neutral label, not a bare "node".
	if topo, err := structure.Load(ctx, st); err == nil && len(topo.Subsystems) > 0 {
		subs := structure.ByHotness(topo.Subsystems)
		b.WriteString("\n\nArchitecture")
		for i, s := range subs {
			if i >= 8 {
				break
			}
			role := componentRole(root, s)
			if role == "" {
				if isConfigPkg(s.Key) {
					role = "shared workspace config"
				} else {
					role = s.Ecosystem + " module"
				}
			}
			fmt.Fprintf(&b, "\n  %-16s %s", s.Key, role)
		}

		// Flow — the architecture diagram EXTRACTED VERBATIM from the docs (ARCH.md /
		// README), if one exists. Never synthesized: a generated flow hallucinates or
		// goes stale, so it's present-when-documented, absent otherwise. (`/arch` shows
		// every diagram, un-wrapped, for wide ones.)
		if d := firstDiagram(root); d != "" {
			b.WriteString("\n\nFlow (from docs)\n" + indentLines(d, "  "))
		}

		// Entry points — churn-hottest CODE subsystems (skip shared config/tooling).
		var entries []string
		for _, s := range subs {
			if s.Recent > 0 && !isConfigPkg(s.Key) && len(entries) < 4 {
				entries = append(entries, s.Key+"/")
			}
		}
		if len(entries) > 0 {
			b.WriteString("\n\nEntry points\n  " + strings.Join(entries, "  ·  "))
		}
	}

	// Stack — the canonical StackFacts block, verbatim (same facts as `memcode stack`).
	if sb := stack.Brief(facts); sb != "" {
		b.WriteString("\n\nStack\n" + sb)
	}

	// Current motion — human focus (objectives) + recent commits, not a raw git feed.
	b.WriteString("\n\nCurrent motion")
	if objs, err := objectives.New(st).Current(ctx); err == nil && len(objs) > 0 {
		b.WriteString("\n  Focus: " + strings.TrimSpace(objs[0].Title))
	}
	if snap.DirtyHash != "" {
		fmt.Fprintf(&b, "\n  - uncommitted changes on %s", orRepo(snap.Branch))
	} else if commits := gitlog.Recent(ctx, root, ".", 6); len(commits) > 0 {
		b.WriteString("\n  Recent:")
		for _, c := range commits {
			fmt.Fprintf(&b, "\n    - %s", strings.TrimSpace(c.Subject))
		}
	}

	return strings.TrimRight(b.String(), "\n")
}

// repoIdentity extracts the project's one-line identity from the root README — the
// blockquote tagline if present, else the first prose paragraph. Human-authored, so
// it's the most reliable "what is this".
func repoIdentity(root string) string {
	b, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil {
		return ""
	}
	var quote, prose []string
	for _, ln := range strings.Split(string(b), "\n") {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, ">") {
			quote = append(quote, cleanMD(strings.TrimSpace(strings.TrimPrefix(t, ">"))))
			continue
		}
		if len(quote) > 0 {
			break // blockquote ended — that's the tagline
		}
		if t == "" || strings.HasPrefix(t, "#") || strings.HasPrefix(t, "![") ||
			strings.HasPrefix(t, "[!") || strings.HasPrefix(t, "```") {
			continue
		}
		prose = append(prose, cleanMD(t))
		if len(prose) >= 2 {
			break
		}
	}
	if len(quote) > 0 {
		return strings.Join(filterEmpty(quote), " ")
	}
	return strings.Join(prose, " ")
}

// componentRole reads a subsystem's OWN documentation for a one-line role: its
// README first, then any doc file the topology recorded. Empty if it has none —
// the caller falls back to the ecosystem rather than inventing a responsibility.
func componentRole(root string, s structure.Subsystem) string {
	candidates := []string{filepath.Join(s.Key, "README.md")}
	candidates = append(candidates, s.Docs...)
	for _, rel := range candidates {
		if r := firstSentence(filepath.Join(root, rel)); r != "" {
			return r
		}
	}
	return ""
}

// firstSentence returns the first prose sentence of a markdown/doc file, skipping
// headings, badges and code fences.
func firstSentence(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, ln := range strings.Split(string(b), "\n") {
		t := strings.TrimSpace(ln)
		if t == "" || strings.HasPrefix(t, "#") || strings.HasPrefix(t, ">") ||
			strings.HasPrefix(t, "![") || strings.HasPrefix(t, "[!") || strings.HasPrefix(t, "```") {
			continue
		}
		t = cleanMD(t)
		if i := strings.Index(t, ". "); i > 0 {
			t = t[:i+1]
		}
		if len(t) > 140 {
			t = t[:139] + "…"
		}
		return t
	}
	return ""
}

// cleanMD strips light markdown emphasis so a tagline reads as plain text.
func cleanMD(s string) string {
	s = strings.ReplaceAll(s, "**", "")
	s = strings.ReplaceAll(s, "`", "")
	return strings.TrimSpace(s)
}

// isConfigPkg reports whether a subsystem is shared tooling/config (eslint, tsconfig,
// prettier, *-config) rather than a real code surface — so it's labeled as config and
// kept out of "entry points" (you don't start reading a repo at eslint-config).
func isConfigPkg(key string) bool {
	k := strings.ToLower(key)
	return strings.Contains(k, "eslint") || strings.Contains(k, "tsconfig") ||
		strings.Contains(k, "prettier") || strings.HasSuffix(k, "-config") ||
		strings.HasSuffix(k, "/config")
}

// wrapText word-wraps plain prose to width (breaks at spaces, never mid-word) so a
// long Purpose paragraph reads as a narrow column instead of running full-width.
func wrapText(s string, width int) []string {
	var lines []string
	cur := ""
	for _, w := range strings.Fields(s) {
		switch {
		case cur == "":
			cur = w
		case len(cur)+1+len(w) > width:
			lines = append(lines, cur)
			cur = w
		default:
			cur += " " + w
		}
	}
	if cur != "" {
		lines = append(lines, cur)
	}
	return lines
}

func filterEmpty(in []string) []string {
	out := in[:0]
	for _, s := range in {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// Get returns a fresh overview: the cache if HEAD-current, else a fresh synthesis.
func Get(ctx context.Context, st store.Store, runner llm.ModelRunner, root, model string) (Overview, error) {
	if o, fresh := Load(ctx, st, root); fresh {
		return o, nil
	}
	return Synthesize(ctx, st, runner, root, model)
}

// gather assembles the current-state evidence block. It LEADS with the live
// working-tree status (ground truth for commit state) so the synthesizer states
// "committed" vs "in flight" from git reality, not from stale objectives/notes —
// the bug that made it narrate "not yet committed" right after a commit.
func gather(ctx context.Context, st store.Store, root string, snap repoSnapshot) string {
	var b strings.Builder

	b.WriteString("WORKING TREE RIGHT NOW (GROUND TRUTH for commit status — derive 'committed' vs 'in flight' from THIS line, never from objectives/notes below):\n")
	if snap.DirtyHash == "" {
		fmt.Fprintf(&b, "- CLEAN — every change is committed. Branch %s at %s. Do NOT describe any work as uncommitted/in-flight/not-yet-committed.\n\n", orRepo(snap.Branch), short(snap.HeadSHA))
	} else {
		fmt.Fprintf(&b, "- DIRTY — uncommitted changes are present on branch %s at %s:\n", orRepo(snap.Branch), short(snap.HeadSHA))
		porc := gitPorcelain(ctx, root)
		lines := strings.Split(porc, "\n")
		for i, ln := range lines {
			if i >= 30 {
				fmt.Fprintf(&b, "  …and %d more\n", len(lines)-30)
				break
			}
			fmt.Fprintf(&b, "  %s\n", ln)
		}
		b.WriteString("\n")
	}

	// Deterministic stack facts (parsed from manifests, not inferred) — the Stack/Parts
	// lines must render from THIS, never guessed from commit text or subsystem names.
	if facts, err := (stack.LocalStackDetector{}).Detect(ctx, root); err == nil {
		if fs := stack.FactSheet(facts); fs != "" {
			b.WriteString(fs + "\n")
		}
	}

	if commits := gitlog.Recent(ctx, root, ".", 20); len(commits) > 0 {
		b.WriteString("Recent commits (newest first — this is where work IS now):\n")
		for _, c := range commits {
			fmt.Fprintf(&b, "- %s\n", strings.TrimSpace(c.Subject))
		}
	}

	if objs, err := objectives.New(st).Current(ctx); err == nil && len(objs) > 0 {
		b.WriteString("\nActive objectives (human-authored goals):\n")
		for _, o := range objs {
			fmt.Fprintf(&b, "- [%s] %s\n", o.Status, o.Title)
		}
	}

	if claims, err := st.ListClaims(ctx); err == nil {
		var cur []store.Claim
		for _, c := range claims {
			if c.Status == "current" {
				cur = append(cur, c)
			}
		}
		if len(cur) > 0 {
			b.WriteString("\nCurrent claims (adjudicated doctrine):\n")
			for _, c := range cur {
				fmt.Fprintf(&b, "- [%s] %s\n", c.Type, c.Text)
			}
		}
	}

	if topo, err := structure.Load(ctx, st); err == nil && len(topo.Subsystems) > 0 {
		subs := append([]structure.Subsystem(nil), topo.Subsystems...)
		sort.Slice(subs, func(i, j int) bool {
			if subs[i].Recent != subs[j].Recent {
				return subs[i].Recent > subs[j].Recent
			}
			return subs[i].Commits > subs[j].Commits
		})
		b.WriteString("\nSubsystems (most recently active first):\n")
		for i, s := range subs {
			if i >= 8 {
				break
			}
			act := ""
			if s.Recent > 0 {
				act = fmt.Sprintf(" — %d recent commits", s.Recent)
			}
			fmt.Fprintf(&b, "- %s (%s)%s\n", s.Key, s.Ecosystem, act)
		}
	}

	if evs, err := st.ListEvents(ctx, store.EventFilter{Kinds: []string{"decision", "assertion", "frustration"}}); err == nil && len(evs) > 0 {
		if len(evs) > 6 {
			evs = evs[len(evs)-6:]
		}
		b.WriteString("\nRecent human decisions/notes:\n")
		for _, e := range evs {
			if t := eventText(e.Payload); t != "" {
				fmt.Fprintf(&b, "- %s\n", t)
			}
		}
	}

	if b.Len() == 0 {
		return "No signals available — describe the repo from its layout only, and say it hasn't been indexed/learned yet."
	}
	return b.String()
}

func eventText(p json.RawMessage) string {
	var m map[string]any
	if len(p) == 0 || json.Unmarshal(p, &m) != nil {
		return ""
	}
	for _, k := range []string{"text", "note", "message", "decision", "title"} {
		if v, ok := m[k].(string); ok && strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func gitHead(ctx context.Context, root string) string {
	out, err := exec.CommandContext(ctx, "git", "-C", root, "rev-parse", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func gitBranch(ctx context.Context, root string) string {
	out, err := exec.CommandContext(ctx, "git", "-C", root, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// gitPorcelain returns `git status --porcelain` — the uncommitted working tree.
// Empty means a clean tree (everything committed).
func gitPorcelain(ctx context.Context, root string) string {
	out, err := exec.CommandContext(ctx, "git", "-C", root, "status", "--porcelain").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// snapshot captures git truth at this instant: branch, committed HEAD, and a hash
// of the dirty working tree (empty hash = clean). This is the freshness boundary
// for every derived summary — the canonical "where the repo actually is".
func snapshot(ctx context.Context, root string) repoSnapshot {
	porc := gitPorcelain(ctx, root)
	dh := ""
	if porc != "" {
		h := fnv.New64a()
		_, _ = h.Write([]byte(porc))
		dh = fmt.Sprintf("%x", h.Sum64())
	}
	return repoSnapshot{Branch: gitBranch(ctx, root), HeadSHA: gitHead(ctx, root), DirtyHash: dh}
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

func orRepo(branch string) string {
	if branch == "" {
		return "(unknown)"
	}
	return branch
}
