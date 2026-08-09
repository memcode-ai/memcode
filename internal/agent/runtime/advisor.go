package runtime

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/memcode-ai/memcode/internal/provider"
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
