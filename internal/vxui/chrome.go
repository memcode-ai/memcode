package vxui

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/memcode-ai/memcode/internal/forks/vaxis/ui"

	"github.com/memcode-ai/memcode/internal/agent/runtime"
	"github.com/memcode-ai/memcode/internal/buildinfo"
	"github.com/memcode-ai/memcode/internal/provider"
)

var spinFrames = []rune("⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏")

// statusStripVisible reports whether the live status strip (spinner/clock + todo panel) should
// render. It's hidden while a question or approval card awaits the user: the turn is blocked on
// input, so a running "Thinking…" clock would mislead (it'd look like the model is computing when
// it's just waiting for an answer).
func (s *appState) statusStripVisible() bool {
	return s.pending == nil && s.askReq == nil
}

// modalUp reports whether a decision surface is awaiting the user: an approval card,
// an ask card, or the plan-ready selector. Background report-backs must not inject a
// turn while one is up — it would clobber the selector's pending state or race a card.
func (s *appState) modalUp() bool {
	return s.pending != nil || s.askReq != nil || s.planChoosing
}

// statusRow is the top line of the live region: a spinner + per-turn clock/tokens while busy,
// otherwise an idle marker (with a queued-count badge when input is waiting).
func (s *appState) statusRow() ui.Widget {
	if s.busy() {
		frame := string(spinFrames[s.spin%len(spinFrames)])
		meta := fmt.Sprintf("(%s", s.turnElapsed().Round(time.Second)) // LLM time, NOT time spent waiting on a HITL card
		// Per-turn tokens: the delta since this turn began (the footer carries the session total).
		if in, out := s.w.sess.Tokens(); in-s.turnIn0 > 0 || out-s.turnOut0 > 0 {
			meta += fmt.Sprintf(" · ↑%s ↓%s", fmtTokens(in-s.turnIn0), fmtTokens(out-s.turnOut0))
		}
		// Surface the reasoning depth the SERVING model is ACTUALLY using (honest + lane-aware):
		// GLM reasons at high/max, an Anthropic model shows its effort tier, a non-reasoning model
		// shows nothing — instead of the old always-"effort: off" that lied on the cheap lane.
		if tier := s.w.sess.ReasoningDisplay(); tier != "" {
			meta += " · " + tier + " effort"
		}
		meta += " · esc to interrupt)"
		return ui.RichText{Spans: []ui.TextSpan{
			{Text: frame + " Thinking… ", Style: s.sty.brand},
			{Text: meta, Style: s.sty.muted},
		}}
	}
	spans := []ui.TextSpan{{Text: "○ idle", Style: s.sty.dim}}
	if n := len(s.queued); n > 0 {
		spans = append(spans, ui.TextSpan{Text: fmt.Sprintf("  · %d queued", n), Style: s.sty.muted})
	}
	// Running background shells surface in the FOOTER only ("N shells") — one
	// quiet cockpit segment, not a status-row banner. /jobs has the detail.
	return ui.RichText{Spans: spans}
}

// composerRow draws the input line with a self-rendered block cursor (no hardware cursor).
func (s *appState) composerRow() ui.Widget {
	spans := []ui.TextSpan{{Text: composerPrompt, Style: s.sty.brand}}
	runes := []rune(s.composer)
	cur := s.cursor
	if cur < 0 {
		cur = 0
	}
	if cur > len(runes) {
		cur = len(runes)
	}
	cursorCh, after := " ", ""
	if cur < len(runes) {
		if runes[cur] == '\n' {
			// Cursor on a line break: draw a reverse-video space and keep the newline
			// in the trailing text, so the row still breaks AFTER the block cursor.
			cursorCh, after = " ", string(runes[cur:])
		} else {
			cursorCh = string(runes[cur])
			after = string(runes[cur+1:])
		}
	}
	// Always draw a block cursor (reverse video) — there is no hardware cursor (see HandleEvent).
	spans = append(spans,
		ui.TextSpan{Text: string(runes[:cur]), Style: s.sty.emph},
		ui.TextSpan{Text: cursorCh, Style: ui.Style{Attribute: ui.AttrReverse}},
		ui.TextSpan{Text: after, Style: s.sty.emph},
	)
	if len(runes) == 0 {
		spans = append(spans, ui.TextSpan{Text: composerPlaceholder, Style: s.sty.muted})
	}
	// SoftWrap so a long line wraps (and reflows on resize) instead of being stuck on
	// one row; the layout also breaks on embedded "\n", so multi-line input (Shift+Enter)
	// renders across rows with the block cursor placed correctly by the layout engine.
	return ui.RichText{Spans: spans, SoftWrap: true}
}

// footerRow is the bottom cockpit: build · branch · git stat · session tokens · model · mode.
func (s *appState) footerRow() ui.Widget {
	model := s.w.sess.DisplayModel()
	if model == "" {
		model = s.w.sess.Model()
	}
	spans := []ui.TextSpan{{Text: "memcode", Style: s.sty.brand}}
	sep := ui.TextSpan{Text: " · ", Style: s.sty.muted}
	add := func(text string, st ui.Style) {
		if text == "" {
			return
		}
		spans = append(spans, sep, ui.TextSpan{Text: text, Style: st})
	}
	add(buildinfo.Compact(), s.sty.muted)
	add(s.branch, s.sty.muted)
	add(gitStatText(s.gstat), s.sty.muted)
	if in, out := s.w.sess.Tokens(); in > 0 || out > 0 {
		add(fmt.Sprintf("↑%s ↓%s", fmtTokens(in), fmtTokens(out)), s.sty.muted)
		// Cache-hit pulse: glyph + % of prompt tokens served from cache. Conditional (only when
		// caching is actually happening) and muted, so it's a quiet signal, not a fixture.
		if cr, _ := s.w.sess.CacheStats(); cr > 0 {
			add(fmt.Sprintf("%s %d%%", cacheGlyph, cacheHitRate(in, cr)), s.sty.muted)
		}
	}
	// Context-window fill of the latest MAIN call. Hidden until the first main call
	// reports (and while the serving window is unknown), muted like the other
	// instruments; warns at ≥80%. Deliberately unclamped — >100% is overflow about to
	// happen, the honest reading. Precision lives in /debug (context N/N), not here.
	if ct, win := s.w.sess.ContextTokens(), s.w.sess.ContextWindow(); ct > 0 && win > 0 {
		pct := ctxPercent(ct, win)
		st := s.sty.muted
		if pct >= 80 {
			st = s.sty.warn
		}
		add(fmt.Sprintf("ctx %d%%", pct), st)
	}
	add(provider.ShortModel(model), s.sty.muted)
	// The last turn served on the user's OWN provider key (zero credit debit).
	// Strictly per-turn: the session clears the flag on every non-BYOK response.
	if s.w.sess.ServedByok() {
		add("byok", s.sty.muted)
	}
	// Live dispatched sub-agent count (from /dispatch or the dispatch tool). Gated on
	// >0 so idle stays clean — like the cache % and token segments. Distinct from the
	// /jobs shell count (these are detached agent processes, not in-session shells).
	if n := s.agents; n > 0 {
		add(fmt.Sprintf("%d agent%s", n, pluralS(n)), s.sty.info)
	}
	// Live in-session background shells (bash background:true, `$ cmd &`, promoted
	// foreground commands). Same gating as agents: idle stays clean. The segment
	// carries a live spinner (animated by the shell ticker even while idle) so a
	// running shell reads as WORKING, not as a stale count.
	if sh := s.w.sess.RunningShells(); len(sh) > 0 {
		frame := string(spinFrames[s.spin%len(spinFrames)])
		add(fmt.Sprintf("%s %d shell%s", frame, len(sh), pluralS(len(sh))), s.sty.info)
	}
	// Plan mode is a distinct state from the permission mode (auto/allow-all/ask) and overrides the
	// segment when active — otherwise the footer reads "auto" while you're actually planning.
	mode := string(s.w.sess.Mode())
	if s.w.sess.Planning() {
		mode = "plan"
		if s.w.sess.PlanYolo() {
			mode = "plan+auto"
		}
	}
	add(mode, s.sty.modeStyle(mode))
	return ui.RichText{Spans: spans}
}

// cacheGlyph marks cached tokens — "latent/cached" (U+25CC). Cache is the ONLY footer/status
// metric that carries a glyph (↑↓ stay glyphless). If a target font tofu-boxes it, swap to "~".
const cacheGlyph = "◌"

// pluralS returns "s" when n != 1, "" otherwise — for compact pluralization in the footer.
func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// cacheHitRate is the percent of prompt tokens served from cache: read/(input+read). The input
// here is the UNCACHED prompt (the gateway splits cached out), so input+read is the full prompt.
// 0 when nothing's been cached — callers gate the display on read>0.
func cacheHitRate(input, read int) int {
	if input+read == 0 {
		return 0
	}
	return read * 100 / (input + read)
}

// ctxPercent is the context-window fill percent — tokens*100/window, div-by-zero-safe.
// Unclamped: >100 means the next call will overflow, which is exactly when to say so.
func ctxPercent(tokens, window int) int {
	if window <= 0 {
		return 0
	}
	return tokens * 100 / window
}

// fmtTokens renders a token count compactly (1234 → "1.2k", 1_200_000 → "1.2M").
func fmtTokens(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	return strconv.Itoa(n)
}

// gitStatText renders the working-tree cockpit instrument: "clean", or
// "git N✎ +A/-R staged N✎ +A/-R" — matching the old TUI's gitStatBadge text.
func gitStatText(g runtime.GitStat) string {
	if g.Clean() {
		return "clean"
	}
	var parts []string
	if g.Files > 0 {
		parts = append(parts, fmt.Sprintf("git %d✎ +%s/-%s", g.Files, fmtTokens(g.Added), fmtTokens(g.Removed)))
	}
	if g.StagedFiles > 0 {
		parts = append(parts, fmt.Sprintf("staged %d✎ +%s/-%s", g.StagedFiles, fmtTokens(g.StagedAdded), fmtTokens(g.StagedRemoved)))
	}
	return strings.Join(parts, " ")
}
