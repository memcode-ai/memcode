package protocol

import (
	"github.com/memcode-ai/memcode/internal/agent/input"
	"github.com/memcode-ai/memcode/internal/agent/mood"
	"github.com/memcode-ai/memcode/internal/agent/room"
	"github.com/memcode-ai/memcode/internal/todos"

	"github.com/memcode-ai/memcode/internal/wire"
)

// driver implements runtime.UIObserver — the structured tap the TUI also uses. Only
// the signals that map cleanly to protocol events are forwarded; the rest are no-ops.

func (d *driver) Busy(busy bool) {
	d.emit(d.currentTurn(), wire.MsgSessionState, wire.SessionStateData{Busy: busy})
}

func (d *driver) Room(s room.State) {
	d.emit(d.currentTurn(), wire.MsgSessionState, wire.SessionStateData{Busy: true, Mode: string(s.Mode)})
}

func (d *driver) Tokens(output int) {
	d.emit(d.currentTurn(), wire.MsgUsage, wire.UsageData{OutputTokens: output})
}

// Raw output (the `$` direct-shell lane) rides as a verbatim assistant delta.
func (d *driver) Raw(text string) {
	d.emit(d.currentTurn(), wire.MsgAssistantDelta, wire.AssistantDeltaData{Text: text})
}

// The remaining observer signals have no protocol counterpart yet — no-ops.
func (d *driver) Routed(input.Route, string) {}
func (d *driver) QueueChanged([]string)      {}
func (d *driver) Mood(mood.Reading)          {}
func (d *driver) Todos(todos.List)           {}
