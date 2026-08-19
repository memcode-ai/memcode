package vxui

// /apikeys — bring your own provider API keys (BYOK). The picker lists every
// vendor the GATEWAY enumerates (registry-derived — nothing hardcoded here)
// with the user's masked key state; Enter opens a MASKED entry stage (the
// composer is never used for secrets — it echoes to scrollback), `d` confirms
// a delete, `v` live-validates the stored key.
//
// Key hygiene: the typed/pasted key exists only in apikeysInput, is registered
// with the session redactor before leaving this file, and the buffer is zeroed
// after submit/cancel. The gateway stores it in Secret Manager; nothing is
// written locally.

import (
	"context"
	"fmt"
	"strings"

	"github.com/memcode-ai/memcode/internal/forks/vaxis"
	"github.com/memcode-ai/memcode/internal/forks/vaxis/ui"

	"github.com/memcode-ai/memcode/internal/provider"
)

// apikeyRow is one provider line in the picker.
type apikeyRow struct {
	provider string // gateway id ("openai", …)
	tail     string // last-4 of the stored key ("" = not set)
	status   string // "active" | "invalid" | ""
}

// providerDisplay renders a provider id for the picker ("openai" → "OpenAI").
// Unknown ids (the roster is server-enumerated and may grow) just capitalize —
// the CLI never hardcodes vendor names (see the provider guard test).
func providerDisplay(id string) string {
	if l, ok := vendorLabels[id]; ok {
		return l
	}
	if id == "" {
		return id
	}
	return strings.ToUpper(id[:1]) + id[1:]
}

// apikeysSlash opens the picker (async roster fetch first).
func (s *appState) apikeysSlash() {
	s.runAsync(func(ctx context.Context) string {
		list, err := provider.ByokList(ctx)
		if err != nil {
			return "✗ couldn't load key settings: " + err.Error()
		}
		byProv := map[string]apikeyRow{}
		for _, k := range list.Keys {
			byProv[k.Provider] = apikeyRow{provider: k.Provider, tail: k.Tail, status: k.Status}
		}
		rows := make([]apikeyRow, 0, len(list.Providers))
		for _, p := range list.Providers {
			if r, ok := byProv[p]; ok {
				rows = append(rows, r)
			} else {
				rows = append(rows, apikeyRow{provider: p})
			}
		}
		s.rt.Dispatch(func() {
			s.SetState(func() {
				s.apikeysRows = rows
				s.apikeysSel = 0
				s.apikeysEntering = false
				s.apikeysConfirmDel = false
				s.apikeysInput = nil
				s.apikeysPicking = true
			})
		})
		if list.Warning != "" {
			return "⚠ " + list.Warning
		}
		return ""
	})
}

// zeroApikeysInput wipes the masked buffer (best-effort memory hygiene).
func (s *appState) zeroApikeysInput() {
	for i := range s.apikeysInput {
		s.apikeysInput[i] = 0
	}
	s.apikeysInput = nil
}

// handleApikeysKey drives the picker's three stages.
func (s *appState) handleApikeysKey(key vaxis.Key) ui.EventResult {
	if len(s.apikeysRows) == 0 {
		s.SetState(func() { s.apikeysPicking = false })
		return ui.EventHandled
	}
	row := s.apikeysRows[s.apikeysSel]

	// Stage: masked key entry.
	if s.apikeysEntering {
		switch key.String() {
		case "Enter":
			keyVal := strings.TrimSpace(string(s.apikeysInput))
			s.SetState(func() {
				s.apikeysEntering = false
				s.apikeysPicking = false
				s.zeroApikeysInput()
			})
			if keyVal == "" {
				s.sysln("○ no key entered")
				return ui.EventHandled
			}
			// Never let the key surface anywhere downstream.
			s.w.sess.AddRedactSecrets(keyVal)
			prov := row.provider
			s.runAsync(func(ctx context.Context) string {
				res, err := provider.ByokPut(ctx, prov, keyVal)
				if err != nil {
					return "✗ " + providerDisplay(prov) + ": " + err.Error()
				}
				s.w.sess.InvalidateModels() // selection reads byok coverage — refetch
				out := fmt.Sprintf("✓ %s key saved (…%s) — your key is now used first for %s turns",
					providerDisplay(prov), res.Tail, providerDisplay(prov))
				if res.Warning != "" {
					out += "\n  ⚠ " + res.Warning
				}
				return out
			})
			return ui.EventHandled
		case "Escape":
			s.SetState(func() {
				s.apikeysEntering = false
				s.zeroApikeysInput()
			})
			return ui.EventHandled
		case "Backspace", "BackSpace": // vaxis names it with a capital S; accept both
			s.SetState(func() {
				if n := len(s.apikeysInput); n > 0 {
					s.apikeysInput = s.apikeysInput[:n-1]
				}
			})
			return ui.EventHandled
		}
		if key.Text != "" && key.Modifiers&(vaxis.ModCtrl|vaxis.ModAlt) == 0 {
			s.SetState(func() { s.apikeysInput = append(s.apikeysInput, []rune(key.Text)...) })
			return ui.EventHandled
		}
		return ui.EventHandled // swallow everything else — modal secret entry
	}

	// Stage: confirm delete.
	if s.apikeysConfirmDel {
		switch key.String() {
		case "Enter":
			prov := row.provider
			s.SetState(func() {
				s.apikeysConfirmDel = false
				s.apikeysPicking = false
			})
			s.runAsync(func(ctx context.Context) string {
				if err := provider.ByokDelete(ctx, prov); err != nil {
					return "✗ " + providerDisplay(prov) + ": " + err.Error()
				}
				s.w.sess.InvalidateModels() // selection reads byok coverage — refetch
				return "✓ " + providerDisplay(prov) + " key removed — memcode's keys serve those turns again"
			})
			return ui.EventHandled
		case "Escape":
			s.SetState(func() { s.apikeysConfirmDel = false })
			return ui.EventHandled
		}
		return ui.EventHandled
	}

	// Stage: provider list.
	switch key.String() {
	case "Up":
		if s.apikeysSel > 0 {
			s.SetState(func() { s.apikeysSel-- })
		}
	case "Down":
		if s.apikeysSel < len(s.apikeysRows)-1 {
			s.SetState(func() { s.apikeysSel++ })
		}
	case "Enter":
		s.SetState(func() {
			s.apikeysEntering = true
			s.apikeysInput = nil
		})
	case "d":
		if row.tail != "" {
			s.SetState(func() { s.apikeysConfirmDel = true })
		}
	case "v":
		if row.tail != "" {
			prov := row.provider
			s.SetState(func() { s.apikeysPicking = false })
			s.runAsync(func(ctx context.Context) string {
				ok, msg, err := provider.ByokValidate(ctx, prov)
				if err != nil {
					return "✗ " + providerDisplay(prov) + ": " + err.Error()
				}
				if !ok {
					return "✗ " + providerDisplay(prov) + " key failed validation: " + msg
				}
				return "✓ " + providerDisplay(prov) + " key is valid"
			})
		}
	case "Escape":
		s.SetState(func() { s.apikeysPicking = false })
	}
	return ui.EventHandled
}

// apikeysPickerView renders the picker (all three stages).
func (s *appState) apikeysPickerView() ui.Widget {
	var rows []ui.Widget

	if s.apikeysEntering && s.apikeysSel < len(s.apikeysRows) {
		row := s.apikeysRows[s.apikeysSel]
		rows = append(rows, ui.RichText{Spans: []ui.TextSpan{
			{Text: "Enter your " + providerDisplay(row.provider) + " API key", Style: s.sty.brand},
		}})
		masked := strings.Repeat("•", len(s.apikeysInput))
		if masked == "" {
			masked = "(paste or type — input is hidden)"
		}
		rows = append(rows, ui.RichText{Spans: []ui.TextSpan{{Text: "  " + masked, Style: s.sty.user}}})
		rows = append(rows, s.hintRow("Enter save · Esc back — the key goes straight to secure storage, nothing is kept on this machine"))
		return s.card(rows...)
	}

	if s.apikeysConfirmDel && s.apikeysSel < len(s.apikeysRows) {
		row := s.apikeysRows[s.apikeysSel]
		rows = append(rows, ui.RichText{Spans: []ui.TextSpan{
			{Text: "Remove your " + providerDisplay(row.provider) + " key (…" + row.tail + ")?", Style: s.sty.brand},
		}})
		rows = append(rows, ui.RichText{Spans: []ui.TextSpan{
			{Text: "  " + providerDisplay(row.provider) + " turns go back to memcode's keys (billed as credits).", Style: s.sty.muted},
		}})
		rows = append(rows, s.hintRow("Enter remove · Esc back"))
		return s.card(rows...)
	}

	rows = append(rows, ui.RichText{Spans: []ui.TextSpan{
		{Text: "API keys — bring your own", Style: s.sty.brand},
	}})
	nameW := 0
	for _, r := range s.apikeysRows {
		if n := len(providerDisplay(r.provider)); n > nameW {
			nameW = n
		}
	}
	opts := make([]choice, 0, len(s.apikeysRows))
	for _, r := range s.apikeysRows {
		name := providerDisplay(r.provider)
		if pad := nameW - len([]rune(name)); pad > 0 {
			name += strings.Repeat(" ", pad)
		}
		state := "not set"
		switch {
		case r.tail != "" && r.status == "invalid":
			state = "…" + r.tail + " · ✗ invalid — replace it"
		case r.tail != "":
			state = "…" + r.tail + " · active"
		}
		opts = append(opts, choice{label: name + "  " + state})
	}
	rows = append(rows, s.optionList(opts, s.apikeysSel, false)...)
	rows = append(rows, s.hintRow("↑↓ select · Enter set/replace · d delete · v validate · Esc close"))
	return s.card(rows...)
}
