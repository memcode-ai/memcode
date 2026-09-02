package vxui

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/memcode-ai/memcode/internal/forks/vaxis"
	"github.com/memcode-ai/memcode/internal/forks/vaxis/ui"

	"github.com/memcode-ai/memcode/internal/config"
	"github.com/memcode-ai/memcode/internal/provider"
)

// vendorLabels maps a vendor id to its display label (capitalized, human-readable).
var vendorLabels = map[string]string{
	"openai":    "OpenAI",
	"anthropic": "Anthropic",
	"gemini":    "Gemini",
	"grok":      "Grok",
}

// vendorLabel returns the display label for a vendor id, falling back to the id itself.
func vendorLabel(v string) string {
	if l, ok := vendorLabels[v]; ok {
		return l
	}
	return v
}

// modelEntry is one row of the /model picker: a concrete pinnable
// model, or (endpoint mode) the free-text entry row.
type modelEntry struct {
	label    string // the model label/id (the pin value)
	name     string // friendly display name ("Sonnet 5"); falls back to label
	desc     string // one-line description ("1M context · Efficient for routine tasks")
	window   int
	byok     bool // served by a vendor the user brought their own API key for
	freeText bool // the "type a model id" row (endpoint mode's fallback entry)
}

// modelSlash handles the /model command. Endpoint mode routes to the endpoint
// picker (the endpoint serves exactly the model you name). Hosted: with no arg
// it opens the flat model picker (every model the gateway offers); with an arg
// it pins that label directly.
//
// There is no Automatic row and no vendor switch. Both were ways of NOT
// choosing a model, and the pin is the only model concept left.
func (s *appState) modelSlash(args string) {
	if ep, ok := s.w.sess.Endpoint(); ok {
		s.endpointModelSlash(ep, strings.TrimSpace(args))
		return
	}
	arg := strings.ToLower(strings.TrimSpace(args))
	if arg != "" {
		s.applyModelChoice(arg, "", 0)
		return
	}
	// No arg → open the picker. Fetch the pinnable list from the gateway (async,
	// bounded), then open the modal on the UI thread.
	cur := s.w.sess.Pin()
	s.runAsync(func(ctx context.Context) string {
		pins := provider.AvailablePins(ctx)
		entries := make([]modelEntry, 0, len(pins))
		for _, p := range pins {
			name := p.Name
			if name == "" {
				name = p.Label
			}
			entries = append(entries, modelEntry{label: p.Label, name: name, desc: p.Desc, window: p.Window, byok: p.Byok})
		}
		s.openModelPicker(entries, cur)
		return "" // the picker is the output; nothing to print
	})
}

// endpointModelSlash is /model against a custom endpoint. There is no
// vendor switch — the endpoint is the serving authority and
// the CLI names one concrete model per session. A typed arg pins it verbatim
// (ids are case-significant); no arg opens the picker on the endpoint's model
// list — the config-curated allowlist when one is set, else GET {base}/models
// — plus the free-text row for endpoints that list nothing.
func (s *appState) endpointModelSlash(ep provider.Endpoint, arg string) {
	switch strings.ToLower(arg) {
	case "":
	case "auto", "automatic":
		s.sysln("a custom endpoint serves exactly the model you name (/model <id>, or /model to pick)")
		return
	default:
		s.applyEndpointModel(ep, arg)
		return
	}
	cur := s.w.sess.Pin()
	s.runAsync(func(ctx context.Context) string {
		ids := ep.Models // the user's curated list is authoritative when present
		if len(ids) == 0 {
			ids = provider.EndpointModels(ctx, ep)
		}
		entries := make([]modelEntry, 0, len(ids)+1)
		for _, id := range ids {
			// Window from the embedded catalog when the id is known, blank
			// otherwise — never a made-up number for a local model.
			entries = append(entries, modelEntry{label: id, name: id, window: provider.CatalogWindow(id)})
		}
		entries = append(entries, modelEntry{freeText: true})
		s.openModelPicker(entries, cur)
		return ""
	})
}

// openModelPicker opens the modal over the given rows on the UI thread,
// preselecting the current choice.
func (s *appState) openModelPicker(entries []modelEntry, cur string) {
	s.rt.Dispatch(func() {
		s.SetState(func() {
			s.modelEntries = entries
			s.modelOrig = cur
			s.modelSel = 0
			for i, e := range entries {
				if e.label != "" && e.label == cur {
					s.modelSel = i
				}
			}
			s.modelTyping = false
			s.modelInput = nil
			s.modelPicking = true
		})
	})
}

// applyEndpointModel pins a concrete endpoint model for the whole session and
// remembers it per-endpoint in config (endpoints never touch the hosted
// pinned_model — the two namespaces must not mix). Window from the embedded
// catalog when known; the serve teaches it otherwise.
func (s *appState) applyEndpointModel(ep provider.Endpoint, model string) {
	win := provider.CatalogWindow(model)
	s.w.sess.SetPin(model, win)
	s.persistModel(func(cfg *config.Config) { cfg.RememberEndpointModel(ep, model) })
	msg := "model → " + model
	if w := fmtWindow(win); w != "" {
		msg += " · " + w + " context"
	}
	s.sysln(msg + " · endpoint " + ep.Name)
}

// resolveEndpointModel is the endpoint-mode boot resolver: when no model is
// configured anywhere (no remembered choice, no MEMCODE_ENDPOINT_MODEL, no
// curated list), ask the endpoint what it serves and adopt the first listed
// model, persisting it as the endpoint's last-model so the next launch skips
// the fetch. Quiet no-op when a model is already set; a listing-less endpoint
// gets a one-line nudge to /model instead of a doomed first turn.
func (s *appState) resolveEndpointModel() {
	ep, ok := s.w.sess.Endpoint()
	if !ok || s.w.sess.Pin() != "" {
		return
	}
	fctx, cancel := context.WithTimeout(s.w.ctx, 5*time.Second)
	defer cancel()
	ids := provider.EndpointModels(fctx, ep)
	if len(ids) == 0 {
		s.rt.Dispatch(func() {
			s.sysln("○ endpoint " + ep.Name + " listed no models — name one with /model <id> (or set " + provider.EnvEndpointModel + ")")
		})
		return
	}
	model := ids[0]
	s.w.sess.SetPin(model, provider.CatalogWindow(model))
	s.persistModel(func(cfg *config.Config) { cfg.RememberEndpointModel(ep, model) })
	s.rt.Dispatch(func() {
		s.SetState(func() {})
		s.sysln("model → " + model + " (first listed by endpoint " + ep.Name + " — change with /model)")
	})
}

// applyModelChoice applies a /model selection: any label pins that model for the
// session, and is persisted so the next session starts on it. window is the pin's
// context window when known (picker rows carry it; typed args pass 0). display is
// the picker's friendly name ("Sonnet 5") when the choice came from the picker —
// empty for a typed arg, so the confirmation echoes back exactly what was typed.
func (s *appState) applyModelChoice(choice, display string, window int) {
	if choice == "" || choice == "auto" || choice == "automatic" {
		// Automatic is gone. Say so plainly rather than silently doing nothing,
		// because muscle memory and old docs will keep sending people here.
		s.sysln("Automatic routing was removed — /model picks one model for the session")
		return
	}
	// A concrete model label. A typed pin works even when the picker list
	// couldn't be fetched; selection validates it for real.
	s.w.sess.SetPin(choice, window)
	// Persist to BOTH stores: the workspace remembers what this repo runs on,
	// and the user level seeds the next NEW repo — "I use Opus" shouldn't have
	// to be re-said per checkout.
	s.persistModel(func(cfg *config.Config) { cfg.PinnedModel, cfg.PinnedWindow = choice, window })
	config.SaveUserPin(choice, window)
	// Picker selections carry a friendly name + context window — echo THAT (what the
	// user actually picked), not the bare gateway label, so the confirmation reads as
	// well as the picker row did. A typed arg has no display name; echo it verbatim.
	label := display
	if label == "" {
		label = choice
	}
	msg := "model → " + label
	if w := fmtWindow(window); w != "" {
		msg += " · " + w + " context"
	}
	s.sysln(msg)
}

// fmtWindow renders a context window compactly for the picker column: 1M, 500K.
// "" for unknown (the Automatic row).
func fmtWindow(w int) string {
	switch {
	case w >= 1_000_000:
		return fmt.Sprintf("%dM", w/1_000_000)
	case w > 0:
		return fmt.Sprintf("%dK", w/1_000)
	}
	return ""
}

// persistModel applies a mutation to the on-disk config so /model choices survive
// across sessions. Serialized through updateConfig — resolveEndpointModel calls
// this off the UI thread.
func (s *appState) persistModel(mut func(*config.Config)) {
	s.updateConfig(mut)
}

// handleModelKey handles the model picker's keyboard: ↑↓ move, Enter apply +
// close, Esc cancel. Selecting the free-text row (endpoint mode) opens an
// inline id-entry stage (the apikeys entry idiom — the composer is never
// borrowed by a modal): type + Backspace edit, Enter applies, Esc returns to
// the list.
func (s *appState) handleModelKey(key vaxis.Key) ui.EventResult {
	n := len(s.modelEntries)
	if s.modelTyping {
		switch key.String() {
		case "Enter":
			id := strings.TrimSpace(string(s.modelInput))
			s.SetState(func() {
				s.modelTyping = false
				s.modelInput = nil
				s.modelPicking = id == "" // empty submit stays in the picker
			})
			if id != "" {
				s.applyPickedModel(id, id, provider.CatalogWindow(id))
			}
			return ui.EventHandled
		case "Escape":
			s.SetState(func() {
				s.modelTyping = false
				s.modelInput = nil
			})
			return ui.EventHandled
		case "Backspace", "BackSpace": // vaxis names it with a capital S; accept both
			s.SetState(func() {
				if n := len(s.modelInput); n > 0 {
					s.modelInput = s.modelInput[:n-1]
				}
			})
			return ui.EventHandled
		}
		if key.Text != "" && key.Modifiers&(vaxis.ModCtrl|vaxis.ModAlt) == 0 {
			s.SetState(func() { s.modelInput = append(s.modelInput, []rune(key.Text)...) })
		}
		return ui.EventHandled // modal: swallow everything else
	}
	switch key.String() {
	case "Up":
		if s.modelSel > 0 {
			s.SetState(func() { s.modelSel-- })
		}
		return ui.EventHandled
	case "Down":
		if s.modelSel < n-1 {
			s.SetState(func() { s.modelSel++ })
		}
		return ui.EventHandled
	case "Enter":
		if n > 0 {
			e := s.modelEntries[s.modelSel]
			if e.freeText {
				s.SetState(func() {
					s.modelTyping = true
					s.modelInput = nil
				})
				return ui.EventHandled
			}
			s.SetState(func() { s.modelPicking = false })
			if e.label == "" {
				s.applyModelChoice("auto", "", 0)
			} else {
				s.applyPickedModel(e.label, e.name, e.window)
			}
		}
		return ui.EventHandled
	case "Escape":
		s.SetState(func() { s.modelPicking = false })
		return ui.EventHandled
	}
	return ui.EventIgnored
}

// applyPickedModel applies a picker selection on the ACTIVE backend: endpoint
// mode pins + remembers per-endpoint; hosted keeps the existing pin flow.
func (s *appState) applyPickedModel(label, display string, window int) {
	if ep, ok := s.w.sess.Endpoint(); ok {
		s.applyEndpointModel(ep, label)
		return
	}
	s.applyModelChoice(label, display, window)
}

// modelPickerView renders the flat model picker: Automatic first (hosted),
// then the models on offer, the current choice marked. Endpoint mode titles
// the card with the endpoint and swaps to the id-entry stage while typing.
func (s *appState) modelPickerView() ui.Widget {
	title := "Select a model"
	if ep, ok := s.w.sess.Endpoint(); ok {
		title += " — endpoint " + ep.Name
	}
	var rows []ui.Widget
	rows = append(rows, ui.RichText{Spans: []ui.TextSpan{
		{Text: title, Style: s.sty.brand},
	}})
	if s.modelTyping {
		rows = append(rows,
			ui.RichText{Spans: []ui.TextSpan{
				{Text: "  model id: ", Style: s.sty.muted},
				{Text: string(s.modelInput), Style: s.sty.emph},
				{Text: " ", Style: ui.Style{Attribute: ui.AttrReverse}}, // block cursor
			}},
			s.hintRow("Enter apply · Esc back to the list"),
		)
		return s.card(rows...)
	}
	opts := make([]choice, 0, len(s.modelEntries))
	for _, row := range modelPickerRows(s.modelEntries, s.modelOrig) {
		opts = append(opts, choice{label: row})
	}
	rows = append(rows, s.optionList(opts, s.modelSel, false)...)
	rows = append(rows, s.hintRow("↑↓ select · Enter · Esc cancel"))
	return s.card(rows...)
}

// modelPickerRows lays out the picker rows in four aligned columns: friendly
// name (✔ marks the active choice), the description, the context window, and a
// trailing byok tag for models served on the user's own API key. A window or
// tag trailing the variable-length description drifted left and right per row
// without the straight columns.
func modelPickerRows(entries []modelEntry, orig string) []string {
	const autoName, autoDesc = "Automatic (recommended)", "memcode picks the right model per turn"
	const freeTextName, freeTextDesc = "Type a model id…", "anything this endpoint serves"
	// All width math is in RUNES (columns), never bytes — a non-ASCII model name
	// measured in bytes would over-pad and shear every column to its right.
	rlen := utf8.RuneCountInString
	nameW, descW, winW := rlen(autoName), rlen(autoDesc), 0
	for _, e := range entries {
		if n := rlen(e.name); n > nameW {
			nameW = n
		}
		if n := rlen(e.desc); n > descW {
			descW = n
		}
		if n := rlen(fmtWindow(e.window)); n > winW {
			winW = n
		}
	}
	nameW += 2 // room for " ✔"
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		name, desc, current := e.name, e.desc, e.label == orig
		switch {
		case e.freeText: // the endpoint-mode escape hatch — never "current"
			name, desc, current = freeTextName, freeTextDesc, false
		case e.label == "":
			name, desc, current = autoName, autoDesc, orig == ""
		}
		if current {
			name += " ✔"
		}
		// Pad by rune count, not bytes — "✔" is 3 bytes but 1 column, and a byte pad
		// would shear the description column on the current row.
		if pad := nameW - rlen(name); pad > 0 {
			name += strings.Repeat(" ", pad)
		}
		row := name + "  " + desc
		w := fmtWindow(e.window)
		if w != "" || e.byok {
			row += strings.Repeat(" ", descW-rlen(desc)) + "  " + w
		}
		if e.byok {
			row += strings.Repeat(" ", winW-rlen(w)) + "  byok"
		}
		out = append(out, strings.TrimRight(row, " "))
	}
	return out
}
