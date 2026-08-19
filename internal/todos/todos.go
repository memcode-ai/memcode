// Package todos is the agent's operational work tracker — a lightweight
// checklist the agent maintains itself so it doesn't lose its place when a
// request has several moving parts. It is NOT the deliberate, research-heavy
// `/plan` mode (that decides WHAT to do and asks the human to confirm); todos
// are just for NOT LOSING TRACK during execution.
//
// The authoritative list lives in the Session's memory (the agent's scratchpad
// for this run). Each change is also snapshotted into the event log as a
// `todos_updated` event — purely for provenance/replay, never as the source of
// truth. Todos deliberately stay OUT of the human-authored objectives table:
// agent-inferred work must not masquerade as the user's durable goals. A todo
// only becomes durable project intent if the user explicitly promotes it to an
// objective.
package todos

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/memcode-ai/memcode/internal/store"
)

// Status values for a todo item. They map onto the checklist legend:
// ✓ done · ▸ active · ○ pending · ! blocked.
const (
	StatusPending = "pending"
	StatusActive  = "active"
	StatusDone    = "done"
	StatusBlocked = "blocked"
	StatusSkipped = "skipped" // intentionally not doing this item
)

func validStatus(s string) bool {
	switch s {
	case StatusPending, StatusActive, StatusDone, StatusBlocked, StatusSkipped:
		return true
	}
	return false
}

// EventKind is the canonical event kind for a todo-list snapshot. (Mirrored as
// events.KindTodosUpdated; kept here too so this package needn't import events.)
const EventKind = "todos_updated"

// Item is one unit of work the agent is tracking. Detail is per-item context
// (what the step actually entails) so the agent can pick the work back up later.
// Owner is who runs it — "main" today; "reader"/"background" once parallel
// fan-out (a later slice) can dispatch items to sub-agents.
type Item struct {
	Title  string `json:"title"`
	Detail string `json:"detail,omitempty"`
	Status string `json:"status"`
	Owner  string `json:"owner,omitempty"`
}

// List is an ordered set of todo items. Slice 1 executes them serially: at most
// one item is StatusActive at a time. (Parallel fan-out is a later slice, built
// on the explore/jobs substrate — not here.)
type List []Item

// snapshot is the wire shape persisted in a todos_updated event payload.
type snapshot struct {
	Items List `json:"items"`
}

// Payload returns the event payload for persisting l (used by the runtime's
// session-tagged emitter so todo changes are attributed like any other event).
func (l List) Payload() map[string]any { return map[string]any{"items": l} }

// Normalize cleans a model-supplied list: it drops empty titles, defaults blank
// or unknown statuses to pending, collapses multiple actives down to the first,
// and — if nothing is active yet but work remains — promotes the first pending
// item to active so there is always a clear "current" step.
func Normalize(l List) List {
	out := make(List, 0, len(l))
	activeSeen := false
	for _, it := range l {
		it.Title = strings.TrimSpace(it.Title)
		if it.Title == "" {
			continue
		}
		if !validStatus(it.Status) {
			it.Status = StatusPending
		}
		if it.Status == StatusActive {
			if activeSeen {
				it.Status = StatusPending // serial execution: only one active
			}
			activeSeen = true
		}
		out = append(out, it)
	}
	if !activeSeen {
		promoteNextPending(out)
	}
	return out
}

// FromTitles builds a fresh list from plain titles (the `create` action), with
// the first item active and the rest pending.
func FromTitles(titles []string) List {
	l := make(List, 0, len(titles))
	for _, t := range titles {
		if t = strings.TrimSpace(t); t != "" {
			l = append(l, Item{Title: t, Status: StatusPending})
		}
	}
	promoteNextPending(l)
	return l
}

// Append pushes newly-discovered work onto the end of an existing list (the
// "push" path) without rewriting it — important for long-running sessions that
// accumulate many items. It keeps existing items untouched and promotes an
// active item if none remains.
func Append(l List, extra List) List {
	for _, it := range extra {
		it.Title = strings.TrimSpace(it.Title)
		if it.Title == "" {
			continue
		}
		if !validStatus(it.Status) {
			it.Status = StatusPending
		}
		if it.Status == StatusActive && ActiveIndex(l) >= 0 {
			it.Status = StatusPending // serial: keep a single active item
		}
		l = append(l, it)
	}
	promoteNextPending(l)
	return l
}

// Advance marks the current active item done and promotes the next pending item
// to active. Used when the agent finishes the step it was on.
func Advance(l List) List {
	if i := ActiveIndex(l); i >= 0 {
		l[i].Status = StatusDone
	}
	promoteNextPending(l)
	return l
}

// MarkDoneAt marks the item at idx (1-based) done; idx<=0 targets the active
// item. It then ensures something is active if work remains.
func MarkDoneAt(l List, idx int) List { return setStatusAt(l, idx, StatusDone) }

// MarkBlockedAt marks the item at idx (1-based) blocked; idx<=0 targets active.
func MarkBlockedAt(l List, idx int) List { return setStatusAt(l, idx, StatusBlocked) }

// MarkSkippedAt marks the item at idx (1-based) skipped; idx<=0 targets active.
// Skipping is "intentionally not doing this", distinct from done.
func MarkSkippedAt(l List, idx int) List { return setStatusAt(l, idx, StatusSkipped) }

// StartAt makes the item at idx (1-based) the active one, demoting any current
// active item back to pending (a deliberate focus switch). idx<=0 starts the
// next pending item. Serial execution keeps exactly one item active.
func StartAt(l List, idx int) List {
	target := idx - 1
	if idx <= 0 {
		target = -1
		for i := range l {
			if l[i].Status == StatusPending {
				target = i
				break
			}
		}
	}
	if target < 0 || target >= len(l) || l[target].Status == StatusDone {
		return l
	}
	if a := ActiveIndex(l); a >= 0 && a != target {
		l[a].Status = StatusPending
	}
	l[target].Status = StatusActive
	return l
}

func setStatusAt(l List, idx int, status string) List {
	target := idx - 1 // 1-based → 0-based
	if idx <= 0 {
		target = ActiveIndex(l)
	}
	if target < 0 || target >= len(l) {
		return l
	}
	l[target].Status = status
	if ActiveIndex(l) < 0 {
		promoteNextPending(l)
	}
	return l
}

// promoteNextPending sets the first pending item to active iff nothing is
// currently active (serial execution invariant).
func promoteNextPending(l List) {
	if ActiveIndex(l) >= 0 {
		return
	}
	for i := range l {
		if l[i].Status == StatusPending {
			l[i].Status = StatusActive
			return
		}
	}
}

// ActiveIndex returns the index of the active item, or -1 if none.
func ActiveIndex(l List) int {
	for i := range l {
		if l[i].Status == StatusActive {
			return i
		}
	}
	return -1
}

// AllSettled reports whether every item has reached a terminal state (done or
// deliberately skipped) — i.e. there's no work left pending/active/blocked.
func (l List) AllSettled() bool {
	if len(l) == 0 {
		return false
	}
	for _, it := range l {
		if it.Status != StatusDone && it.Status != StatusSkipped {
			return false
		}
	}
	return true
}

// Marker returns the checklist glyph for a status.
func Marker(status string) string {
	switch status {
	case StatusDone:
		return "✓"
	case StatusActive:
		return "●"
	case StatusBlocked:
		return "!"
	case StatusSkipped:
		return "⊘"
	default:
		return "○"
	}
}

// Summary is a plain-language progress line: "2/6 done" (skipped items count as
// resolved), with "· N blocked" appended when anything is stuck.
func (l List) Summary() string {
	done, blocked, total := 0, 0, len(l)
	for _, it := range l {
		switch it.Status {
		case StatusDone, StatusSkipped:
			done++
		case StatusBlocked:
			blocked++
		}
	}
	s := fmt.Sprintf("%d/%d done", done, total)
	if blocked > 0 {
		s += fmt.Sprintf(" · %d blocked", blocked)
	}
	return s
}

// Render returns the multi-line checklist, each line prefixed with indent, e.g.
//
//	✓ 1. inspect current loader
//	▸ 2. add upsert path
//	○ 3. update tests
func (l List) Render(indent string) string {
	var b strings.Builder
	for i, it := range l {
		fmt.Fprintf(&b, "%s%s %d. %s\n", indent, Marker(it.Status), i+1, it.Title)
	}
	return strings.TrimRight(b.String(), "\n")
}

// RenderWindow renders at most max item lines, centered on the active item, so
// a long list (a session can accumulate dozens) stays compact in the live TUI.
// Hidden items above/below are summarized as "…N above/below". The full list is
// always available via `todo show` / `memcode todos`.
func (l List) RenderWindow(indent string, max int) string {
	if max <= 0 || len(l) <= max {
		return l.Render(indent)
	}
	center := ActiveIndex(l)
	if center < 0 {
		center = len(l) - 1 // no active item: anchor on the end
	}
	start := center - max/2
	if start < 0 {
		start = 0
	}
	end := start + max
	if end > len(l) {
		end = len(l)
		start = end - max
	}
	var b strings.Builder
	if start > 0 {
		fmt.Fprintf(&b, "%s  ⋯ %d above\n", indent, start)
	}
	for i := start; i < end; i++ {
		fmt.Fprintf(&b, "%s%s %d. %s\n", indent, Marker(l[i].Status), i+1, l[i].Title)
	}
	if end < len(l) {
		fmt.Fprintf(&b, "%s  ⋯ %d below\n", indent, len(l)-end)
	}
	return strings.TrimRight(b.String(), "\n")
}

// Current reads the most recent todo-list snapshot from the event log. An empty
// list (no error) means no todos have been recorded yet.
func Current(ctx context.Context, st store.Store) (List, error) {
	// Each event is a FULL snapshot, so only the newest one matters — Limit: 1
	// fetches exactly it instead of scanning every snapshot ever written.
	evs, err := st.ListEvents(ctx, store.EventFilter{Kinds: []string{EventKind}, Limit: 1})
	if err != nil {
		return nil, err
	}
	if len(evs) == 0 {
		return nil, nil
	}
	ev := evs[len(evs)-1]
	var snap snapshot
	if err := json.Unmarshal(ev.Payload, &snap); err != nil {
		// Surface corruption instead of silently reporting "no todos" — the
		// caller can't tell an empty list from a broken one otherwise.
		return nil, fmt.Errorf("todos: malformed snapshot (event %d): %w", ev.ID, err)
	}
	return snap.Items, nil
}
