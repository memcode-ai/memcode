package runtime

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/memcode-ai/memcode/internal/provider"
	"github.com/memcode-ai/memcode/internal/wire"
)

// AskAdvisor sends a question (plus light session context) to the gateway's
// second-opinion advisor — a DIFFERENT vendor from the coding lanes; the
// gateway owns which model serves it — and returns its advice. effort is the reasoning depth (low|medium|high; "" →
// high). This is a deliberate user action (/advisor, or "Ask an advisor" in plan
// mode), not part of the coding-inference path. Returns (text, ok).
func (s *Session) AskAdvisor(ctx context.Context, question, effort string) (string, bool) {
	adv, ok := s.prov.(provider.Advisor)
	if !ok {
		return "advisor unavailable on this backend (needs the memcode gateway)", false
	}
	q := strings.TrimSpace(question)
	if q == "" {
		q = "Given the current memcode session, advise the single best path forward right now. Be specific and decisive."
	}
	prompt := fmt.Sprintf("Repo: %s\n\n%s", filepath.Base(s.Root()), q)
	advice, err := adv.Advise(ctx, prompt, effort)
	if err != nil {
		return "advisor error: " + err.Error(), false
	}
	return advice, true
}

// ReviewPlanWith runs a ONE-SHOT plan critique on a model the USER named on the
// approval card ("Review with another model").
//
// This is deliberately its own category: not utility inference (it is not
// plumbing, and the user chose it), and not routing (nothing selected it on the
// user's behalf). It is an explicit, user-directed second opinion, scoped to
// this one operation.
//
// The chosen model runs on an ephemeral fork. The session pin, the workspace
// store, and the user store are untouched, the footer keeps showing the session
// model, and any revision the critique prompts runs back on the pinned model.
// Returns the critique prose and the model that produced it.
func (s *Session) ReviewPlanWith(ctx context.Context, plan, model string) (string, string) {
	plan = strings.TrimSpace(plan)
	if plan == "" {
		return "", ""
	}
	msgs := []wire.Message{{Role: "user", Blocks: []wire.Block{{Type: "text", Text: "=== PLAN ===\n" + plan}}}}
	v, servedBy := s.reviewWithTools(ctx, msgs, model)

	var b strings.Builder
	if sum := strings.TrimSpace(v.Summary); sum != "" {
		b.WriteString(sum)
	}
	for _, is := range v.Issues {
		if d := strings.TrimSpace(is.Detail); d != "" {
			b.WriteString("\n- " + d)
		}
	}
	if fb := strings.TrimSpace(v.Feedback); fb != "" {
		b.WriteString("\n\n" + fb)
	}
	out := strings.TrimSpace(b.String())
	if out == "" {
		out = "No issues raised."
	}
	return out, servedBy
}
