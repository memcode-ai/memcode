package vxui

import (
	"context"
	"strings"

	"github.com/memcode-ai/memcode/internal/forks/vaxis/ui"

	"github.com/memcode-ai/memcode/internal/config"
)

// openSync opens the /sync target picker. It detects which AI-editor context files exist on
// disk, loads the stored selection from config, and seeds the toggles from it (a target is on
// if it's in the stored list, or if Everything mode was set). The picker is multi-select —
// unlike the other (single-select radio) pickers, Space toggles a row and Enter commits.
func (s *appState) openSync() {
	if s.busy() {
		s.sysln("can't open /sync mid-turn — wait for the current task to finish")
		return
	}
	detected := s.w.sess.SyncDetect()
	all := config.SyncTargetAll
	toggles := make([]bool, len(all))
	if cfg, err := config.Load(s.w.sess.Root()); err == nil {
		if cfg.Sync.Everything {
			for i := range toggles {
				toggles[i] = true
			}
		} else {
			active := map[string]bool{}
			for _, t := range cfg.Sync.Targets {
				active[strings.ToLower(string(t))] = true
			}
			for i, t := range all {
				toggles[i] = active[strings.ToLower(t.Name)]
			}
		}
	}
	s.SetState(func() {
		s.syncDetected = detected
		s.syncToggles = toggles
		s.syncSel = 0
		s.syncChoosing = true
	})
}

// handleSyncKey drives the multi-select target picker while it's open: ↑↓ move the cursor,
// Space toggles the highlighted row, a toggles all on/off, Enter persists the selection to
// config and kicks off an async sync, Esc cancels without saving. Owns the keyboard (every key
// is consumed) — the same contract as the other pickers.
func (s *appState) handleSyncKey(k string) ui.EventResult {
	n := len(config.SyncTargetAll)
	switch k {
	case "Up":
		if s.syncSel > 0 {
			s.SetState(func() { s.syncSel-- })
		}
	case "Down", "Tab":
		if s.syncSel < n-1 {
			s.SetState(func() { s.syncSel++ })
		}
	case " ", "Space", "space": // Space toggles the row under the cursor (multi-select)
		if s.syncSel >= 0 && s.syncSel < len(s.syncToggles) {
			s.SetState(func() { s.syncToggles[s.syncSel] = !s.syncToggles[s.syncSel] })
		}
	case "a": // toggle all on/off
		allOn := true
		for _, on := range s.syncToggles {
			if !on {
				allOn = false
				break
			}
		}
		s.SetState(func() {
			for i := range s.syncToggles {
				s.syncToggles[i] = !allOn
			}
		})
	case "Enter":
		// Build the target list from the toggles, persist, close, and sync async.
		var targets []config.SyncTarget
		for i, on := range s.syncToggles {
			if on {
				targets = append(targets, config.SyncTarget(strings.ToLower(config.SyncTargetAll[i].Name)))
			}
		}
		s.SetState(func() { s.syncChoosing = false })
		if len(targets) == 0 {
			s.sysln("no targets selected — nothing to sync.")
			return ui.EventHandled
		}
		// Persist the selection so it survives across sessions (best-effort).
		if cfg, err := config.Load(s.w.sess.Root()); err == nil {
			cfg.Sync.Targets = targets
			cfg.Sync.Everything = false
			_ = cfg.Save()
		}
		s.runAsync(func(ctx context.Context) string {
			out, err := s.w.sess.Sync(ctx, targets)
			if err != nil {
				return "sync failed: " + err.Error()
			}
			return out
		})
	case "Escape":
		s.SetState(func() { s.syncChoosing = false })
		s.sysln("sync cancelled")
	}
	return ui.EventHandled
}

// syncPickerView is the modal multi-select target picker. Each row shows a ☑/☐ checkbox, the
// editor name, its path, and a disk-status tag (✓ exists · ● managed · — not found). The cursor
// row (❯) is the one Space toggles.
func (s *appState) syncPickerView() ui.Widget {
	rows := []ui.Widget{
		ui.RichText{Spans: []ui.TextSpan{{Text: "Select sync targets", Style: s.sty.brand}}},
		ui.RichText{Spans: []ui.TextSpan{{Text: "Keep AI-editor context files (CLAUDE.md, AGENTS.md, …) in sync with memcode.", Style: s.sty.muted}}, SoftWrap: true, MaxLines: 2},
		ui.SizedBox{Height: 1},
	}
	all := config.SyncTargetAll
	for i, t := range all {
		marker, nameStyle := "  ", s.sty.muted
		if i == s.syncSel {
			marker, nameStyle = "❯ ", s.sty.emph
		}
		box := "☐"
		if i < len(s.syncToggles) && s.syncToggles[i] {
			box = "☑"
		}
		// Disk status tag: ✓ exists (and ● managed if memcode already owns the header).
		status := "— not found"
		statusStyle := s.sty.dim
		if i < len(s.syncDetected) && s.syncDetected[i].Exists {
			if s.syncDetected[i].Managed {
				status = "● managed"
				statusStyle = s.sty.info
			} else {
				status = "✓ exists"
				statusStyle = s.sty.muted
			}
		}
		spans := []ui.TextSpan{
			{Text: marker, Style: s.sty.brand},
			{Text: box + " ", Style: s.sty.brand},
			{Text: t.Name, Style: nameStyle},
			{Text: "  " + t.Path, Style: s.sty.dim},
			{Text: "  " + status, Style: statusStyle},
		}
		rows = append(rows, ui.RichText{Spans: spans, SoftWrap: true, MaxLines: 2})
	}
	rows = append(rows, ui.SizedBox{Height: 1},
		s.hintRow("↑↓ move · Space toggle · a all · Enter sync · Esc cancel"))
	return ui.Flex{Axis: ui.Vertical, MainAxisSize: ui.MainAxisSizeMin, CrossAxisAlignment: ui.CrossAxisStart, Children: rows}
}
