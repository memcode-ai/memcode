package runtime

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/memcode-ai/memcode/internal/agent/tools"
	"github.com/memcode-ai/memcode/internal/structure"
)

// repoMapTool renders the ranked symbol map (structure.BuildSymbolMap):
// personalized PageRank over the symbol reference graph — Go parsed natively,
// TS/JS/Python via the session's resident language servers (documentSymbol),
// content-hash-cached in the store so only changed files re-query. Read-only
// and parallel-safe.
func (s *Session) repoMapTool(ctx context.Context, input json.RawMessage) toolResult {
	var in tools.RepoMapInput
	if err := json.Unmarshal(input, &in); err != nil {
		return errResult(err.Error())
	}
	opts := structure.MapOptions{
		Budget: in.BudgetTokens,
		Store:  s.store,
		Extern: func(ctx context.Context, abs string) ([]structure.ExternSymbol, bool) {
			fs, ok, err := s.lsp().FileSymbols(ctx, abs)
			if !ok || err != nil {
				return nil, ok && err == nil
			}
			out := make([]structure.ExternSymbol, 0, len(fs))
			for _, f := range fs {
				out = append(out, structure.ExternSymbol{
					Name: f.Name, Kind: f.Kind, Line: f.Line, EndLine: f.EndLine,
					SelLine: f.SelLine, Depth: f.Depth,
				})
			}
			return out, true
		},
	}
	if f := strings.TrimSpace(in.Focus); f != "" {
		opts.Focus = strings.Fields(f)
	}
	digest, err := structure.BuildSymbolMap(ctx, s.root, opts)
	if err != nil {
		return errResult("repo_map failed: " + err.Error())
	}
	// Hidden tier: internal machinery, no marker at all (see toolLineStat's tier contract).
	return textResult(truncate(digest, maxToolOutput))
}
