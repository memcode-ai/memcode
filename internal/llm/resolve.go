package llm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/memcode-ai/memcode/catalog"
	"github.com/memcode-ai/memcode/internal/provider"
	"github.com/memcode-ai/memcode/internal/wire"
)

// resolve.go — the ONE non-pin decision and the capability gate.
//
// There is exactly one model per session and the pin resolver already settled
// it (session -> workspace -> user -> default_model). What remains here is
// routing internal plumbing to the catalog's utility_model, and refusing a turn
// the pinned model physically cannot serve.
//
// The ladder this file used to hold — role/tier verdicts, BYOK steering, the $0
// fundability remap, capability SUBSTITUTION — is deleted (v0.29.0). See
// resolveHosted and capabilityCheck for why each one had to go. The gateway
// cannot reroute either: what the pin names is what serves, or a typed error
// comes back.

// modelsTTL bounds how stale the control-plane snapshot may get before a
// refresh; invalidation (login, /apikeys, 402s) cuts it short.
const modelsTTL = 5 * time.Minute

// selection owns the control-plane snapshot and the resolution policy. One per
// Runner tree (forks share it, like the ledger).
type selection struct {
	mu      sync.Mutex
	info    provider.ModelsInfo
	at      time.Time
	haveNet bool // true when info came from the gateway (vs the catalog fallback)

	// fetch is the control-plane call, swappable in tests.
	fetch func(ctx context.Context) (provider.ModelsInfo, error)
}

func newSelection() *selection {
	return &selection{fetch: provider.FetchModels}
}

// Invalidate drops the snapshot so the next call refetches — hooked to /login,
// /apikeys mutations, and 402-class errors (credits state just changed).
func (s *selection) Invalidate() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.at = time.Time{}
}

// models returns the current control-plane snapshot, refreshing over the
// network when stale. A failed fetch degrades to the last snapshot, or to a
// catalog-derived default (labels + capabilities only — no byok/credits
// facts), so a gateway hiccup never blocks selection: the gateway remains the
// enforcement backstop for anything the degraded snapshot got wrong.
func (s *selection) models(ctx context.Context) provider.ModelsInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.at.IsZero() && time.Since(s.at) < modelsTTL {
		return s.info
	}
	fctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	info, err := s.fetch(fctx)
	if err == nil && len(info.Models) > 0 {
		s.info, s.at, s.haveNet = info, time.Now(), true
		return info
	}
	if s.haveNet { // keep serving the stale-but-real snapshot
		s.at = time.Now() // don't hammer a down gateway every call
		return s.info
	}
	s.info, s.at = catalogInfo(), time.Now()
	return s.info
}

// catalogInfo builds the degraded control-plane snapshot from the embedded
// catalog: every chat-capable label, no byok/credits/role facts. Selection
// still works (tier fallbacks resolve); the gateway enforces the rest.
func catalogInfo() provider.ModelsInfo {
	var info provider.ModelsInfo
	for _, m := range catalog.CatalogModels() {
		if m.Window <= 0 {
			continue
		}
		info.Models = append(info.Models, provider.ModelFact{
			Label: m.Label, Name: m.Name, Desc: m.Desc, Group: m.Group, Vendor: m.Vendor,
			Window: m.Window, Vision: m.Vision, PDF: m.PDF, Reasoning: m.Reasoning, Pinnable: m.Pinnable,
		})
	}
	return info
}

// resolved is one selection verdict.
type resolved struct {
	label  string // the concrete model label to request ("" only on err)
	pinned bool   // the user's explicit /model choice served as-is
	reason string // client-side absorb reason ("vision" | "document" | "context_overflow") for the ⇄ line
	err    error  // turn-fatal capability refusal (no capable lane reachable)
}

// utilityPurposes are the internal-plumbing purposes that never ride the pin:
// the structured classifiers (classify — authorize rides it, it has no purpose
// of its own), compaction, and shrinkwrap. They run on the catalog's
// utility_model so background jobs never spend the user's chosen model.
//
// The rule that governs this set: utility inference may classify, authorize,
// compact, shrinkwrap, or otherwise support execution, but it may NEVER select,
// substitute, escalate, downgrade, or steer the pinned model. Anything that
// influenced which model runs was routing, and routing is gone.
var utilityPurposes = map[string]bool{"classify": true, "compact": true, "shrinkwrap": true}

// resolveHosted maps an Intent to a concrete model label.
//
// There is exactly ONE model per session — the pin — and this function does not
// choose it: the pin resolver settled that at session start from the
// session -> workspace -> user -> default_model chain. All that happens here is
// routing the one non-pin case (internal plumbing) and refusing a turn the
// pinned model physically cannot serve.
//
// The Automatic ladder that used to live here — laneFor's role/tier verdicts,
// BYOK steering, the $0 fundability remap, all ported from the gateway in
// 2026-08 — is DELETED. Per-turn model decision-making is precisely what the
// pin replaced, and keeping a second selection authority alive is how the
// footer and the serving model came to disagree.
func resolveHosted(it wire.Intent, req wire.Request, info provider.ModelsInfo) resolved {
	purpose := strings.TrimSpace(strings.ToLower(it.Purpose))

	if utilityPurposes[purpose] {
		if u := catalog.UtilityModel(); u != "" {
			return resolved{label: u}
		}
		// No utility model declared: fall through to the pin rather than
		// inventing a model. Losing a classifier is better than a silent pick.
	}

	if it.Pin == "" {
		// The pin resolver guarantees a pin (it seeds from default_model when
		// nothing is stored). Reaching here means a caller bypassed it — a bug,
		// and one that must NOT be papered over with a default, or the default
		// quietly becomes the new Automatic.
		return resolved{err: errors.New("no model selected for this session — run /model to choose one")}
	}
	return capabilityCheck(it.Pin, req, info)
}

// capabilityCheck refuses a turn the pinned model cannot physically serve.
//
// This was capabilityAdjust, which SUBSTITUTED a capable model instead. That
// was Automatic routing wearing a different hat: pasting a screenshot silently
// moved the turn onto a model the user never chose and never saw. Model choice
// belongs to the user now, so a capability gap is a visible refusal that names
// the fix.
//
// Provider and TRANSPORT failures are a different thing and still absorb —
// recover.go walks the catalog's declared fallback chain and names the
// substitute on the exchange line. That is infrastructure resilience, not model
// selection.
func capabilityCheck(label string, req wire.Request, info provider.ModelsInfo) resolved {
	f, ok := info.Fact(label)
	if !ok {
		if m, found := catalog.LookupModel(label); found {
			f = provider.ModelFact{
				Label: m.Label, Name: m.Name, Vendor: m.Vendor,
				Window: m.Window, Vision: m.Vision, PDF: m.PDF,
			}
		} else {
			f = provider.ModelFact{Label: label}
		}
	}
	name := label
	if f.Name != "" {
		name = f.Name
	}

	if hasBlock(req, "image") && !f.Vision {
		return resolved{err: fmt.Errorf("%s can't read images. Switch with /model to send this turn", name)}
	}
	if hasBlock(req, "document") && !f.PDF {
		return resolved{err: fmt.Errorf("%s can't read PDFs. Switch with /model to send this turn", name)}
	}
	if est := estimateTokens(req); f.Window > 0 && est > f.Window {
		return resolved{err: fmt.Errorf("this turn is about %d tokens, past %s's %d-token window. Run /compact, or switch with /model", est, name, f.Window)}
	}
	return resolved{label: label, pinned: true}
}

// hasBlock reports whether any message block (top-level or inside a
// tool_result's ContentBlocks) has the given type — the same check the
// gateway's capability gates run.
func hasBlock(req wire.Request, t string) bool {
	for _, m := range req.Messages {
		for _, b := range m.Blocks {
			if b.Type == t {
				return true
			}
			for _, cb := range b.ContentBlocks {
				if cb.Type == t {
					return true
				}
			}
		}
	}
	return false
}

// estimateTokens is the pre-flight size estimate (~4 chars/token with a 1.25
// safety margin — the gateway's exact formula, kept so the client-side
// overflow pre-check fires where the server-side one did).
func estimateTokens(req wire.Request) int {
	n := len(req.System) + len(req.SystemVolatile)
	for _, m := range req.Messages {
		for _, b := range m.Blocks {
			n += len(b.Text) + len(b.Content) + len(b.Input) + len(b.Thinking)
		}
	}
	return n / 4 * 125 / 100
}
