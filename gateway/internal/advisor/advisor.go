// Package advisor is memcode's "second opinion" side-channel: it asks a frontier model
// from a DIFFERENT vendor (Claude Opus, adaptive thinking on) to advise the best path
// forward on a situation or a plan. This is NOT a coding-inference backend (the two-backend
// doctrine — OpenAI + Fireworks — still governs that); it's an advisory tool the user
// invokes deliberately (/advisor, or "Ask an advisor" in plan mode), automating the manual
// "paste it into Claude" loop. Using the cross-vendor frontier model is the point — a
// second opinion from the SAME vendor that serves inference would be an echo chamber.
package advisor

import (
	"context"
	"fmt"
	"strings"

	"github.com/memcode-ai/memcode/catalog"
	"github.com/memcode-ai/memcode/internal/providers/anthropic"
	"github.com/memcode-ai/memcode/internal/wire"
)

const systemPrompt = `You are a senior staff engineer acting as a second-opinion advisor on the memcode project (a Go CLI/TUI coding agent whose CLI owns model selection across vendor backends; a hosted gateway serves metered inference over one OpenAI-compatible wire).

Given the situation or plan below, give concise, decisive advice on the single best path forward. Lead with your recommendation in one line. Then: the highest-leverage next step, the biggest risk, and anything the plan is missing or over-building. Be direct and specific; skip preamble and flattery. Prefer a few strong points over an exhaustive list.`

// Advisor asks the cross-vendor frontier model via the SHARED Messages
// adapter (providers/anthropic) — no wire code of its own.
type Advisor struct {
	key   string
	model string
	prov  *anthropic.Anthropic
}

// New returns an advisor. model defaults to Claude Sonnet (catalog.ModelSonnet); override with
// ANTHROPIC_ADVISOR_MODEL. An empty key yields an advisor whose Advise returns a clear "not
// configured" error (never panics).
func New(key, model string) *Advisor {
	if model == "" {
		model = catalog.ModelSonnet
	}
	return &Advisor{key: key, model: model, prov: anthropic.NewAnthropic(key)}
}

// Available reports whether the advisor has a key (so callers can hide the option).
func (a *Advisor) Available() bool { return a.key != "" }

// setBaseURL points the underlying adapter at a test server.
func (a *Advisor) setBaseURL(u string) { a.prov.SetBaseURL(u) }

// mapEffort translates the advisor's effort vocabulary (none|low|medium|high|xhigh —
// the OpenAI reasoning_effort names) onto Anthropic's output_config.effort
// (low|medium|high). The full ladder is honored so a caller asking for "medium" gets
// medium, not high (the old map collapsed everything except "low" to high, and even
// inverted "none" — a no-reasoning request — into maximum effort). "" defaults to high
// (the advisor's job is a hard second opinion), but an explicit none/low is respected.
func mapEffort(effort string) string {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "none", "low":
		return "low"
	case "medium":
		return "medium"
	case "high", "xhigh":
		return "high"
	default: // "" or unknown → the advisor's default
		return "high"
	}
}

// Advise asks the model to advise on question. effort is the reasoning depth
// (none|low|medium|high|xhigh); "" defaults to high. Returns the advice text.
func (a *Advisor) Advise(ctx context.Context, question, effort string) (string, error) {
	text, _, err := a.AdviseMetered(ctx, question, effort)
	return text, err
}

// AdviseMetered is Advise plus the usage the call billed, so the gateway can record this
// side-channel into the shared Ledger — the advisor is the most expensive model in the system
// and was previously unmetered (invisible spend).
func (a *Advisor) AdviseMetered(ctx context.Context, question, effort string) (string, wire.Response, error) {
	if a.key == "" {
		return "", wire.Response{}, fmt.Errorf("advisor unavailable: ANTHROPIC_API_KEY not configured on the gateway")
	}
	if effort == "" {
		effort = "high"
	}
	// One question through the SHARED Messages adapter — the advisor carries
	// zero wire code of its own (mapEffort folds the OpenAI-style vocabulary
	// onto the abstract Effort the adapter maps per-model).
	resp, err := a.prov.Complete(ctx, wire.Request{
		Model:     a.model,
		System:    systemPrompt,
		MaxTokens: 4096,
		Effort:    wire.Effort(mapEffort(effort)),
		Messages: []wire.Message{{Role: "user", Blocks: []wire.Block{
			wire.TextBlock(question),
		}}},
	})
	if err != nil {
		return "", wire.Response{}, fmt.Errorf("advisor request: %w", err)
	}
	usage := resp
	usage.Model = a.model
	text := resp.Text()
	if text == "" {
		return "", usage, fmt.Errorf("advisor: empty response from %s", a.model)
	}
	return text, usage, nil
}
