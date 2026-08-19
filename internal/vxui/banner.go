package vxui

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"time"

	"github.com/memcode-ai/memcode/internal/forks/vaxis/ui"

	"github.com/memcode-ai/memcode/internal/agent/runtime"
	"github.com/memcode-ai/memcode/internal/banner"
	"github.com/memcode-ai/memcode/internal/provider"
	"github.com/memcode-ai/memcode/internal/theme"
)

// bigWordmark is the startup "MEMCODE" wordmark (ported from the old TUI banner).
var bigWordmark = []string{
	"██     ██   ████████   ██     ██    ██████     ██████    ███████    ████████",
	"███   ███   ██         ███   ███   ██    ██   ██    ██   ██    ██   ██      ",
	"██ █ █ ██   ██         ██ █ █ ██   ██         ██    ██   ██    ██   ██      ",
	"██  █  ██   ██████     ██  █  ██   ██         ██    ██   ██    ██   ██████  ",
	"██     ██   ██         ██     ██   ██         ██    ██   ██    ██   ██      ",
	"██     ██   ██         ██     ██   ██    ██   ██    ██   ██    ██   ██      ",
	"██     ██   ████████   ██     ██    ██████     ██████    ███████    ████████",
}

// matrixGlyphs are the katakana/symbol set used for the matrix theme's digital-rain banner.
var matrixGlyphs = []rune("ﾊﾐﾋｰｳｼﾅﾓﾆｻﾜｱｲｳｴｵｶｷｸｹｺｱｦﾝﾘﾂﾃﾅﾆｾﾈ0123456789Zｱ:.=*+<>|╱╲")

// printBanner prints the settled matrix-glyph wordmark (the MEMCODE drawn in bright matrix
// glyphs over sparse rain — bannerString, NOT the boxed animation-frame renderer) to scrollback
// before the inline TUI starts. Used for non-matrix themes; the matrix theme animates it instead.
// When raw is true the terminal is in raw mode (we capture startup keystrokes there), so newlines
// need an explicit carriage return — the cooked-mode NL→CRNL translation is off.
func printBanner(ctx context.Context, sess *runtime.Session, raw bool) {
	s := bannerString(ctx, sess, theme.Active().Palette)
	// A selected subscription that failed to resolve must be LOUD: turns are
	// about to serve (and bill) somewhere the user did not choose.
	for _, src := range provider.SelectedSourcesUnresolved() {
		s += fmt.Sprintf("⚠ %s subscription attached but not signed in — its models serve elsewhere; run `memcode auth %s` to fix\n", src, src)
	}
	if raw {
		s = strings.ReplaceAll(s, "\n", "\r\n")
	}
	fmt.Fprint(os.Stdout, s)
}

// sessionStatusChips returns the basic active-state chips for the ready line — only the
// ones actually set, so a default session shows nothing extra. A "random" theme/voice
// choice shows what the roll SELECTED (the concrete name), not the word "random".
func sessionStatusChips(sess *runtime.Session) []string {
	var chips []string
	if !sess.Connected() {
		chips = append(chips, "signed out")
	}
	if ep, ok := sess.Endpoint(); ok {
		// Exclusive endpoint mode: name where inference actually goes — the
		// one signal that matters when there's no memcode account in the loop.
		chips = append(chips, "endpoint "+ep.Name)
	}
	// Family lanes: which subscriptions/keys serve their vendors' models.
	var subs, keys []string
	for _, ln := range sess.Lanes() {
		if ln.Kind == "sub" {
			subs = append(subs, provider.ServingLabel(ln.Name))
		} else {
			keys = append(keys, ln.Vendor)
		}
	}
	if len(subs) > 0 {
		chips = append(chips, "subs "+strings.Join(subs, "+"))
	}
	if len(keys) > 0 {
		chips = append(chips, "key "+strings.Join(keys, "+"))
	}
	if t := theme.Chosen(); t != "" && t != "aurora" {
		if t == "random" {
			t = theme.Active().Name
		}
		chips = append(chips, t)
	}
	if p := sess.PersonalityResolved(); p != "" {
		chips = append(chips, p)
	}
	if sess.ExtraMile() {
		chips = append(chips, "extra mile")
	}
	return chips
}

// bannerString builds the wordmark + ready line. vaxis's primary screen is meant to preserve
// this as scrollback above the live region. The matrix theme gets a digital-rain variant.
func bannerString(ctx context.Context, sess *runtime.Session, p theme.Palette) string {
	reset := "\x1b[0m"
	model := sess.DisplayModel()
	if model == "" {
		model = sess.Model()
	}
	meta := provider.ShortModel(model) + " · " + string(sess.Mode())
	if o := sess.Orientation(ctx); o.Branch != "" {
		meta = o.Branch + " · " + meta
	}
	// A few basic active-state chips, only when set (idle stays clean): a non-default
	// theme, a chosen personality, and extra-mile mode — so a launched session shows its
	// posture at a glance without a wall of settings.
	for _, chip := range sessionStatusChips(sess) {
		meta += " · " + chip
	}

	// The settled wordmark — MEMCODE drawn in matrix glyphs (bright phosphor over its strokes, a
	// negative-space halo around the letters, sparse dim rain in the field). EVERY theme settles
	// on this glyph banner; the matrix theme additionally animates into it. Colored from the
	// active palette (green under the matrix theme, the theme's brand/dim elsewhere).
	bright, dim := ansiSGR(p.Brand, true), ansiSGR(p.Dim, false)
	glyph := func() string { return string(matrixGlyphs[rand.Intn(len(matrixGlyphs))]) }
	rows := make([][]rune, len(bigWordmark))
	width := 0
	for i, r := range bigWordmark {
		rows[i] = []rune(r)
		if len(rows[i]) > width {
			width = len(rows[i])
		}
	}
	fill := func(y, x int) bool {
		return y >= 0 && y < len(rows) && x >= 0 && x < len(rows[y]) && rows[y][x] == '█'
	}
	halo := func(y, x int) bool {
		for dy := -1; dy <= 1; dy++ {
			for dx := -2; dx <= 2; dx++ {
				if fill(y+dy, x+dx) {
					return true
				}
			}
		}
		return false
	}

	var b strings.Builder
	b.WriteString("\n")
	rainRow := func() {
		for x := 0; x < width; x++ {
			if rand.Intn(4) == 0 {
				b.WriteString(dim + glyph() + reset)
			} else {
				b.WriteByte(' ')
			}
		}
		b.WriteByte('\n')
	}
	rainRow()
	for y := range rows {
		for x := 0; x < width; x++ {
			switch {
			case fill(y, x):
				b.WriteString(bright + glyph() + reset)
			case halo(y, x):
				b.WriteByte(' ') // negative space so MEMCODE reads cleanly
			case rand.Intn(4) == 0:
				b.WriteString(dim + glyph() + reset)
			default:
				b.WriteByte(' ')
			}
		}
		b.WriteByte('\n')
	}
	rainRow()
	if sess.Connected() {
		b.WriteString(bright + "↺ ready" + reset + ansiSGR(p.Muted, false) + "  " + meta + reset + "\n\n")
	} else {
		// Mandatory login: the boot notice. Local commands still work; anything
		// model-backed prompts for /login.
		b.WriteString(bright + "○ signed out" + reset + ansiSGR(p.Muted, false) + "  " + meta +
			reset + "\n" + ansiSGR(p.Muted, false) + "  run " + reset + bright + "/login" + reset +
			ansiSGR(p.Muted, false) + " to connect to memcode.ai" + reset + "\n\n")
	}
	return b.String()
}

// runIntro advances the matrix intro one frame per ~45ms on the UI thread, until the animation
// + a short hold complete (or a keystroke ends it). The matrix card is drawn as widgets by Build.
func (s *appState) runIntro() {
	total := banner.MatrixFrames() + 16 // animation + ~700ms landed hold
	t := time.NewTicker(45 * time.Millisecond)
	defer t.Stop()
	for range t.C {
		cont := make(chan bool, 1)
		s.rt.Dispatch(func() {
			if s.intro {
				s.introFrame++
				if s.introFrame >= total {
					s.settleIntro() // animation done → rest on the settled glyph wordmark
				}
				s.SetState(func() {})
			}
			cont <- s.intro
		})
		if !<-cont {
			return
		}
	}
}

// settleIntro ends the matrix animation and leaves the settled glyph wordmark in scrollback —
// EVERY theme rests on this banner (the matrix theme just animates into it first). Called both
// when the animation finishes and when a keystroke skips it.
func (s *appState) settleIntro() {
	s.intro = false
	s.ectx.AppendString(bannerString(s.w.ctx, s.w.sess, theme.Active().Palette))
	s.lastBlank = true
}

// matrixIntroView renders the digital-rain card as widget rows — the framework lays them out
// (clipped to the terminal width), so there's no raw-output overflow/desync.
func (s *appState) matrixIntroView() ui.Widget {
	w := s.width
	if w < 80 {
		w = 80
	}
	// Cap the rain card: on a wide terminal a full-bleed card reads as noise;
	// ~100 cols frames the 77-col wordmark with margin and stops there.
	if w > 100 {
		w = 100
	}
	var rows []ui.Widget
	for _, line := range strings.Split(banner.Matrix(w, s.introFrame, s.introRecall), "\n") {
		rows = append(rows, ui.RichText{Spans: parseANSISpans(line)})
	}
	return ui.Flex{Axis: ui.Vertical, MainAxisSize: ui.MainAxisSizeMin, CrossAxisAlignment: ui.CrossAxisStart, Children: rows}
}

// matrixRecall mirrors the old intro's recall line from the session's orientation.
func matrixRecall(ctx context.Context, sess *runtime.Session) string {
	o := sess.Orientation(ctx)
	if o.Subsystems == 0 && o.ClaimsCurrent == 0 {
		return "↺ first run — getting to know " + o.Repo
	}
	var parts []string
	if o.Subsystems > 0 {
		parts = append(parts, fmt.Sprintf("%d subsystems", o.Subsystems))
	}
	if o.ClaimsCurrent > 0 {
		parts = append(parts, fmt.Sprintf("%d memories", o.ClaimsCurrent))
	}
	return "↺ recalled  " + strings.Join(parts, " · ")
}
