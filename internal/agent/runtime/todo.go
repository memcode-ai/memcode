package runtime

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/memcode-ai/memcode/internal/agent/tools"
	"github.com/memcode-ai/memcode/internal/events"
	"github.com/memcode-ai/memcode/internal/todos"
	"github.com/memcode-ai/memcode/internal/wire"
)

// todoTool mutates the agent's in-memory work tracker (s.todos) — its scratchpad
// for the current run. Each mutation is also snapshotted into the event log for
// provenance/replay (NOT as the source of truth), and pushed to the front-end so
// the live checklist updates. The list never enters the objectives table: it's
// agent scratch, not human-authored project intent.
func (s *Session) todoTool(ctx context.Context, input json.RawMessage) toolResult {
	var in tools.TodoInput
	if err := json.Unmarshal(input, &in); err != nil {
		return errResult(err.Error())
	}
	action := strings.ToLower(strings.TrimSpace(in.Action))
	items := wireToTodos(in.Items)

	switch action {
	case "create":
		s.todos = todos.Normalize(items)
	case "add":
		if len(s.todos) == 0 {
			s.todos = todos.Normalize(items) // first push doubles as create
		} else {
			s.todos = todos.Append(s.todos, items)
		}
	case "update":
		s.todos = todos.Normalize(items)
	case "start":
		if len(s.todos) == 0 {
			return errResult("no todo list yet — create one first.")
		}
		s.todos = todos.StartAt(s.todos, in.Index)
	case "done":
		if len(s.todos) == 0 {
			return errResult("no todo list yet — create one first.")
		}
		// Apply to a copy first so we can reject before committing. `indices` marks
		// several at once (a holistic sweep → one call); else `index`/active item.
		next := append(todos.List(nil), s.todos...)
		if len(in.Indices) > 0 {
			for _, idx := range in.Indices {
				next = todos.MarkDoneAt(next, idx)
			}
		} else {
			next = todos.MarkDoneAt(next, in.Index)
		}
		// Acceptance guardrail (enforced, not just advised): the work isn't done
		// until it's verified. If this completes the LAST item but no build/tests
		// passed after the last edit, refuse — don't let the agent claim success.
		if next.AllSettled() && s.metrics.didEdit && s.metrics.lastVerifyOKSeq <= s.metrics.lastEditSeq {
			return errResult("cannot complete the final todo: no passing verification (build/tests) after your last edit. Run a build and/or tests first, then mark it done.")
		}
		s.todos = next
	case "block":
		if len(s.todos) == 0 {
			return errResult("no todo list yet — create one first.")
		}
		s.todos = todos.MarkBlockedAt(s.todos, in.Index)
	case "skip":
		if len(s.todos) == 0 {
			return errResult("no todo list yet — create one first.")
		}
		if len(in.Indices) > 0 {
			for _, idx := range in.Indices {
				s.todos = todos.MarkSkippedAt(s.todos, idx)
			}
		} else {
			s.todos = todos.MarkSkippedAt(s.todos, in.Index)
		}
	case "show":
		// read-only
	default:
		return errResult("unknown todo action: " + action + " (try: " + strings.Join(tools.TodoActions, ", ") + ")")
	}

	if len(s.todos) == 0 {
		s.toolLine(true, "Task", action, "empty", false)
		return textResult("todo list is empty.")
	}

	// Provenance: snapshot the list (with the action that caused it) into the
	// event log. This is for replay/debug, not the authoritative store.
	if action != "show" {
		s.emit(ctx, events.KindTodosUpdated, map[string]any{"action": action, "items": s.todos})
	}

	// One-line marker, consistent with every other tool (⏺ Verb(arg) · status). "Task"
	// reads more human than "Todo"; the live checklist panel carries the full list.
	s.toolLine(true, "Task", action, s.todos.Summary(), false)
	// Without a front-end (plain CLI / single-shot) there's no live region, so
	// print the checklist inline; the TUI renders it in place instead.
	if s.observer == nil {
		s.printf("%s\n", s.todos.Render("  "))
	} else {
		s.observer.Todos(s.todos)
	}

	return textResult("todos (" + s.todos.Summary() + "):\n" + s.todos.Render("  "))
}

func wireToTodos(items []tools.TodoItemWire) todos.List {
	out := make(todos.List, 0, len(items))
	for _, it := range items {
		out = append(out, todos.Item{Title: it.Title, Detail: it.Detail, Status: it.Status, Owner: "main"})
	}
	return out
}

// synthTitle picks the todo-list title for a tracked follow-up: the classifier's
// synthesized title when it is actually title-shaped, else the raw text when THAT already
// reads as a title (a short imperative ask needs no synthesis), else "" — the caller
// substitutes its fixed label. The bounds are what make "synthesized, not verbatim"
// ENFORCED rather than advised: a multi-line or over-long "title" is the raw message
// echoed back, and pasted user prose must never land on the human-scannable list — the
// model still gets the full text via the FYI note / the queued transaction.
func synthTitle(title, raw string) string {
	if t := strings.TrimSpace(title); t != "" && !strings.Contains(t, "\n") && len(t) <= synthTitleMax {
		return t
	}
	if r := strings.TrimSpace(raw); r != "" && !strings.Contains(r, "\n") && len(r) <= rawTitleMax {
		return r
	}
	return ""
}

const (
	synthTitleMax = 100 // longer is not a synthesis — it's the message echoed back
	rawTitleMax   = 60  // a raw message this short already reads as a title

	// followupFallbackTitle labels a follow-up whose synthesized title never arrived and
	// whose raw text is too long to read as one; activeFallbackTitle seeds the placeholder
	// for the in-progress task in the same situation.
	followupFallbackTitle = "Queued follow-up"
	activeFallbackTitle   = "Current task"
)

// noteSeparateRequests tracks genuinely SEPARATE follow-ups (the background classifier's
// "not related" verdict) on the todo list, since a scheduler-driven mutation has no other
// way to become visible: the model didn't call the todo tool itself, so this does the
// append the model would have done, then tells the model it happened via an FYI note (see
// buildSeparateNote) so it can acknowledge in its next reply.
//
// Each item's todo Title goes through synthTitle — the classifier's synthesized title,
// or the raw text only when it is itself title-shaped, or the fixed fallback label.
// The verbatim text is reserved for the FYI note the model sees (buildSeparateNote),
// never the list a human scans.
//
// The first time this fires with an EMPTY todo list, it seeds a placeholder item for the
// CURRENT task (marked active) before appending — without that seed, todos.Append's
// "promote the first pending item to active" would wrongly promote the separate ask ahead
// of the work actually in progress. The placeholder's title is synthTitle over activeTitle
// (the SAME classify call's synthesized title for the active task) and activeText.
func (s *Session) noteSeparateRequests(ctx context.Context, activeText, activeTitle string, items []separateAsk, messages *[]wire.Message) {
	if len(items) == 0 {
		return
	}
	extra := make(todos.List, 0, len(items))
	texts := make([]string, 0, len(items))
	for _, it := range items {
		text := strings.TrimSpace(it.Text)
		if text == "" {
			continue
		}
		title := synthTitle(it.Title, text)
		if title == "" {
			title = followupFallbackTitle
		}
		extra = append(extra, todos.Item{Title: title, Status: todos.StatusPending, Owner: "main"})
		texts = append(texts, text)
	}
	if len(extra) == 0 {
		return
	}
	if len(s.todos) == 0 {
		placeholder := synthTitle(activeTitle, activeText)
		if placeholder == "" {
			placeholder = activeFallbackTitle
		}
		s.todos = todos.Normalize(todos.List{{Title: placeholder, Status: todos.StatusActive, Owner: "main"}})
	}
	s.todos = todos.Append(s.todos, extra)

	// Same provenance snapshot todoTool emits — this mutation was invisible in
	// events.jsonl, which is what made the verbatim-title incident hard to trace.
	s.emit(ctx, events.KindTodosUpdated, map[string]any{"action": "add", "source": "followup_classifier", "items": s.todos})

	if s.observer != nil {
		s.observer.Todos(s.todos)
	}
	// Same one-line marker todoTool itself prints, for scrollback consistency — this
	// mutation didn't come from the model calling the tool, but it looks like it did.
	s.toolLine(true, "Task", "add", s.todos.Summary(), false)

	*messages = append(*messages, wire.Message{Role: "user", Blocks: []wire.Block{{Type: "text", Text: buildSeparateNote(texts)}}})
}
