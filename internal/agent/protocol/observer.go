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
	data := wire.UsageData{OutputTokens: output}
	if d.sess != nil {
		in, out := d.sess.Tokens()
		cacheRead, cacheWrite := d.sess.CacheStats()
		data.InputTokens = in
		data.TotalOutputTokens = out
		data.CacheReadTokens = cacheRead
		data.CacheWriteTokens = cacheWrite
		data.ContextTokens = d.sess.ContextTokens()
		data.ContextWindow = d.sess.ContextWindow()
		data.Model = d.sess.DisplayModel()
		data.ReasoningEffort = d.sess.ReasoningDisplay()
		data.ServedBy = d.sess.ServedBy()
		data.ServedByok = d.sess.ServedByok()
		data.RunningShells = len(d.sess.RunningShells())
	}
	d.emit(d.currentTurn(), wire.MsgUsage, data)
}

// Raw output (the `$` direct-shell lane) rides as a verbatim assistant delta.
func (d *driver) Raw(text string) {
	d.emit(d.currentTurn(), wire.MsgAssistantDelta, wire.AssistantDeltaData{Text: text})
}

// The remaining observer signals have no protocol counterpart yet — no-ops.
func (d *driver) Routed(input.Route, string) {}
func (d *driver) QueueChanged([]string)      {}
func (d *driver) Mood(mood.Reading)          {}

// Todos forwards the plan/checklist as a `todos` event (a GUI plan panel).
func (d *driver) Todos(list todos.List) {
	items := make([]wire.TodoItem, 0, len(list))
	for _, it := range list {
		items = append(items, wire.TodoItem{Text: it.Title, Status: mapTodoStatus(it.Status)})
	}
	d.emit(d.currentTurn(), wire.MsgTodos, wire.TodosData{Items: items})
}

func mapTodoStatus(s string) string {
	switch s {
	case todos.StatusActive:
		return "in_progress"
	case todos.StatusDone:
		return "done"
	case todos.StatusBlocked:
		return "blocked"
	case todos.StatusSkipped:
		return "skipped"
	default:
		return "pending"
	}
}

// EmitDiff / EmitTool satisfy the runtime's optional diffEmitter / toolEmitter
// interfaces (internal/agent/runtime/emit.go), turning structured file changes and
// tool activity into protocol events a GUI client renders natively.
func (d *driver) EmitDiff(path, language, unified string, added, removed int, newFile bool) {
	d.emit(d.currentTurn(), wire.MsgDiff, wire.DiffData{
		Path: path, Language: language, Unified: unified, Added: added, Removed: removed, NewFile: newFile,
	})
}

func (d *driver) EmitTool(name, target, detail string, failed bool) {
	d.emit(d.currentTurn(), wire.MsgToolCall, wire.ToolCallData{Name: name, Target: target})
	status := "ok"
	if failed {
		status = "failed"
	}
	d.emit(d.currentTurn(), wire.MsgToolResult, wire.ToolResultData{Name: name, Status: status, Detail: detail})
}

func (d *driver) EmitToolOutput(name, output string) {
	d.emit(d.currentTurn(), wire.MsgToolResult, wire.ToolResultData{Name: name, Output: output})
}
