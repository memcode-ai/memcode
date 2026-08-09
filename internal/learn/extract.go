package learn

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/memcode-ai/memcode/internal/agent/secrets"
	"github.com/memcode-ai/memcode/internal/llm"
	"github.com/memcode-ai/memcode/internal/sources"
	"github.com/memcode-ai/memcode/internal/store"
	"github.com/memcode-ai/memcode/internal/wire"
)

const (
	maxSourcesToExtract = 8
	maxSourceBytes      = 3000
)

// extractClaims asks the model to extract candidate claims from the highest-value
// source documents. All status is "candidate" — adjudication decides currency.
func extractClaims(ctx context.Context, runner *llm.Runner, root string, srcs []sources.Source) ([]store.Claim, error) {
	selected := selectSources(srcs)
	if len(selected) == 0 {
		return nil, nil
	}

	red := secrets.NewFromEnv()
	var b strings.Builder
	for _, s := range selected {
		data, err := os.ReadFile(filepath.Join(root, s.Path))
		if err != nil {
			continue
		}
		content := red.Redact(truncate(string(data), maxSourceBytes))
		b.WriteString("### source: " + s.Path + " (scope: " + s.Scope + ", kind: " + s.Kind + ")\n")
		b.WriteString(content + "\n\n")
	}
	if b.Len() == 0 {
		return nil, nil
	}

	// Forced tool: the learn lane serves reasoning models — prose JSON
	// rambles, a forced call can't. The prose-array parse stays as fallback.
	resp, err := runner.Complete(ctx, llm.Learn, wire.Request{
		Mode:       "extract",
		Messages:   []wire.Message{{Role: "user", Blocks: []wire.Block{{Type: "text", Text: b.String()}}}},
		Tools:      []wire.ToolDef{claimsTool},
		ToolChoice: claimsTool.Name,
	})
	if err != nil {
		return nil, err
	}

	wire := decodeClaims(resp)
	if wire == nil {
		return nil, nil // tolerate a malformed extraction rather than fail the run
	}

	scopeByPath := map[string]string{}
	modByPath := map[string]string{}
	for _, s := range selected {
		scopeByPath[s.Path] = s.Scope
		modByPath[s.Path] = s.GitDate
	}

	var out []store.Claim
	for _, w := range wire {
		if strings.TrimSpace(w.Text) == "" {
			continue
		}
		scope := w.Scope
		if sc, ok := scopeByPath[w.SourcePath]; ok && (scope == "" || scope == ".") {
			scope = sc
		}
		out = append(out, store.Claim{
			Type:             normalizeType(w.Type),
			Text:             strings.TrimSpace(w.Text),
			Scope:            scope,
			Status:           "candidate",
			SourcePath:       w.SourcePath,
			SourceModifiedAt: modByPath[w.SourcePath],
		})
	}
	return out, nil
}

// selectSources prioritizes instruction artifacts over docs, current over stale,
// and caps the count to control tokens.
func selectSources(srcs []sources.Source) []sources.Source {
	rank := func(kind string) int {
		switch kind {
		case "claude", "cursor", "codex/agents", "copilot", "windsurf", "aider":
			return 0
		case "readme":
			return 1
		default:
			return 2
		}
	}
	cp := append([]sources.Source(nil), srcs...)
	sort.SliceStable(cp, func(i, j int) bool {
		if rank(cp[i].Kind) != rank(cp[j].Kind) {
			return rank(cp[i].Kind) < rank(cp[j].Kind)
		}
		if cp[i].Stale != cp[j].Stale {
			return !cp[i].Stale
		}
		return cp[i].Path < cp[j].Path
	})
	if len(cp) > maxSourcesToExtract {
		cp = cp[:maxSourcesToExtract]
	}
	return cp
}

func normalizeType(t string) string {
	t = strings.ToLower(strings.TrimSpace(t))
	switch t {
	case "doctrine", "preference", "command", "decision", "warning":
		return t
	default:
		return "doctrine"
	}
}

// wireClaim is one extracted claim on the model contract.
type wireClaim struct {
	SourcePath string `json:"source_path"`
	Type       string `json:"type"`
	Text       string `json:"text"`
	Scope      string `json:"scope"`
}

// claimsTool is the forced structured-output contract for claim extraction.
var claimsTool = wire.ToolDef{
	Name:        "record_claims",
	Description: "Record the candidate claims extracted from the source documents.",
	InputSchema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"claims": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"source_path": map[string]any{"type": "string"},
						"type":        map[string]any{"type": "string"},
						"text":        map[string]any{"type": "string"},
						"scope":       map[string]any{"type": "string"},
					},
					"required": []string{"source_path", "text"},
				},
			},
		},
		"required": []string{"claims"},
	},
}

// decodeClaims decodes the forced tool call, falling back to the legacy
// prose-array parse. nil = malformed (caller tolerates).
func decodeClaims(resp wire.Response) []wireClaim {
	for _, blk := range resp.ToolUses() {
		if blk.Name != claimsTool.Name || len(blk.Input) == 0 {
			continue
		}
		var p struct {
			Claims []wireClaim `json:"claims"`
		}
		if json.Unmarshal(blk.Input, &p) == nil {
			return p.Claims
		}
	}
	var wire []wireClaim
	if json.Unmarshal([]byte(extractJSONArray(resp.Text())), &wire) != nil {
		return nil
	}
	return wire
}

func extractJSONArray(s string) string {
	i := strings.Index(s, "[")
	j := strings.LastIndex(s, "]")
	if i >= 0 && j > i {
		return s[i : j+1]
	}
	return "[]"
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n…(truncated)"
}
