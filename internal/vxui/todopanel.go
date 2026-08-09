package vxui

import (
	"fmt"
	"strings"

	"github.com/memcode-ai/memcode/internal/forks/vaxis/ui"

	"github.com/memcode-ai/memcode/internal/agent/input"
	"github.com/memcode-ai/memcode/internal/agent/mood"
	"github.com/memcode-ai/memcode/internal/agent/room"
	"github.com/memcode-ai/memcode/internal/todos"
)

// vxObserver is vxui's UIObserver. vxui reads most live state directly (tokens/mode/busy via
// the session accessors), so only Todos is wired — it feeds the live task panel. Crucially,
// setting ANY observer also suppresses the engine's fallback of dumping the full checklist into
// scrollback: todoTool prints the list inline only when observer == nil, and pushes it here
// otherwise. The other methods are deliberate no-ops (vxui already surfaces that state).
type vxObserver struct{ s *appState }

// Todos arrives on the engine goroutine — marshal onto the UI thread before touching state.
func (o vxObserver) Todos(list todos.List) {
	cp := append(todos.List(nil), list...)
	o.s.rt.Dispatch(func() { o.s.SetState(func() { o.s.todos = cp }) })
}

func (vxObserver) Routed(input.Route, string) {}
func (vxObserver) QueueChanged([]string)      {} // vxui tracks the queue via its scheduler observer
func (vxObserver) Busy(bool)                  {} // vxui owns its own busy/spinner state
func (vxObserver) Mood(mood.Reading)          {}
func (vxObserver) Room(room.State)            {}
func (vxObserver) Tokens(int)                 {} // vxui polls sess.Tokens() each frame

// Raw commits a VERBATIM block to scrollback — the `$` shell lane (the styled prompt plus the
// command's raw output, whitespace/blank lines intact), bypassing the markdown/stream renderer.
// Without this it was a no-op, so `$ <cmd>` ran but printed nothing (the "shell lane is broken"
// report). Arrives on the executor goroutine → marshal onto the UI thread; flush any buffered
// stream text first so ordering is preserved.
func (o vxObserver) Raw(block string) {
	if strings.TrimSpace(stripSGR(block)) == "" {
		return
	}
	o.s.rt.Dispatch(func() {
		o.s.flushAppend()
		o.s.ectx.AppendString(strings.TrimRight(block, "\n") + "\n")
		o.s.lastBlank, o.s.lastTool = false, false
	})
}

// todoPanel renders the agent's work-tracker as a live region BELOW the status bar (Claude-Code
// style) — it updates in place as the agent starts/completes items, instead of each update
// appending a fresh checklist to scrollback. Returns nil when there are no todos.
func (s *appState) todoPanel() []ui.Widget {
	if len(s.todos) == 0 {
		return nil
	}
	done := 0
	for _, it := range s.todos {
		if it.Status == todos.StatusDone {
			done++
		}
	}
	rows := []ui.Widget{ui.RichText{Spans: []ui.TextSpan{
		{Text: fmt.Sprintf("Tasks %d/%d", done, len(s.todos)), Style: s.sty.muted},
	}}}
	for _, it := range s.todos {
		glyph, gStyle, tStyle := s.todoStyle(it.Status)
		rows = append(rows, ui.RichText{Spans: []ui.TextSpan{
			{Text: "  " + glyph + " ", Style: gStyle},
			{Text: it.Title, Style: tStyle},
		}, SoftWrap: true, MaxLines: 2})
	}
	return rows
}

// todoStyle maps a todo status to its marker glyph and styles (glyph, title).
func (s *appState) todoStyle(status string) (glyph string, g, t ui.Style) {
	switch status {
	case todos.StatusDone:
		return "✓", s.sty.brand, s.sty.muted
	case todos.StatusActive:
		return "▸", s.sty.brand, s.sty.emph
	case todos.StatusBlocked:
		return "⊘", s.sty.warn, s.sty.muted
	case todos.StatusSkipped:
		return "⊝", s.sty.muted, s.sty.muted
	default: // pending
		return "○", s.sty.dim, s.sty.muted
	}
}
