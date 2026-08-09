// Package runtime: this file is the thin seam to the read-only intelligence
// Engine (internal/agent/introspect). The commands themselves were carved off the
// Session god-object into their own leaf package; Session supplies the Engine its
// state plus the few runtime-internal callbacks it needs and otherwise just delegates.
package runtime

import (
	"context"
	"encoding/json"
	"time"

	"github.com/memcode-ai/memcode/internal/agent/introspect"
	"github.com/memcode-ai/memcode/internal/wire"
)

// introspect builds the intelligence Engine over this Session's current state. The
// Engine holds no state of its own, so a fresh one per call is cheap and always
// reflects the latest sessionID / personality / lastUserText.
func (s *Session) introspect() *introspect.Engine {
	return introspect.New(introspect.Deps{
		Root:         s.root,
		Store:        s.store,
		Runner:       s.runner,
		Prov:         s.prov,
		Redactor:     s.redactor,
		SessionID:    s.sessionID,
		Model:        s.model,
		Personality:  s.personality,
		LastUserText: s.lastUserText,
		ToolLine:     s.toolLine,
		Complete:     s.sideComplete,     // side-channel calls ride the instrumented plumbing (wire trace)
		ExtraChecks:  s.classifierChecks, // /doctor shows this session's classifier ok/timeout/err traffic
		ChatRequest:  func(r wire.Request) wire.Request { return s.chatSpec("").request(r) },
		Jobs:         s.introspectJobs, // shell management stays on Session (bgjobs.go)
	})
}

// memcodeTool dispatches the single `memcode` agent tool (see introspect.Engine.MemcodeTool).
func (s *Session) memcodeTool(ctx context.Context, input json.RawMessage) toolResult {
	text, isErr := s.introspect().MemcodeTool(ctx, input)
	return toolResult{blocks: []wire.Block{wire.TextBlock(text)}, isError: isErr}
}

// Intelligence runs a read-only memcode command for the TUI's "orient me" shortcuts.
func (s *Session) Intelligence(ctx context.Context, command, arg string) (string, bool) {
	return s.introspect().Intelligence(ctx, command, arg)
}

// PersonalityBlurb returns a one-line greeting in the currently-set voice (best-effort).
func (s *Session) PersonalityBlurb(ctx context.Context) string {
	return s.introspect().PersonalityBlurb(ctx)
}

// ArchDoc renders the repo's architecture diagrams verbatim from its docs (deterministic).
func (s *Session) ArchDoc() string { return s.introspect().ArchDoc() }

// planIntentTimeout keeps the ambiguity resolver tight — it sits on the interactive
// composer path, and its fail-open answer (false) just runs an ordinary turn.
const planIntentTimeout = 8 * time.Second

// recordPlanIntentTool forces the yes/no as schema-constrained tool_use input — replaces
// the old strings.Contains scrape of prose JSON.
var recordPlanIntentTool = wire.ToolDef{
	Name:        "record_plan_intent",
	Description: "Record whether the user is asking for an implementation PLAN to be drafted before any work. Call exactly once.",
	InputSchema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"plan": map[string]any{"type": "boolean"},
		},
		"required": []string{"plan"},
	},
}

// ClassifyPlanIntent resolves the AMBIGUOUS middle of plan-request detection — the
// front-end's deterministic heuristic handles clear yes/no and only escalates here when
// "plan" is present but the intent is unclear. Lives on Session (not the introspect
// Engine) so it rides the shared judge plumbing (traced, failure-counted). Best-effort:
// any error/timeout/parse miss → false, so ambiguity never hijacks an ordinary turn
// into plan mode.
func (s *Session) ClassifyPlanIntent(ctx context.Context, text string) bool {
	var out struct {
		Plan bool `json:"plan"`
	}
	if s.classifyToolCall(ctx, "plan_intent", recordPlanIntentTool, s.redactor.Redact(text), planIntentTimeout, &out) != nil {
		return false
	}
	return out.Plan
}
