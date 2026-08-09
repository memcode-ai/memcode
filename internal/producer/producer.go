// Package producer attributes work to who/what produced it — a human or a
// specific AI tool — from commit metadata. It is deliberately conservative:
// detectors are few and each result carries a confidence and the evidence, so
// the reducer can reason over attribution without overfitting to signatures.
package producer

import "strings"

// Producer is who/what authored a change.
type Producer string

const (
	Human      Producer = "human"
	ClaudeCode Producer = "claude-code"
	Cursor     Producer = "cursor"
	Codex      Producer = "codex"
	Copilot    Producer = "copilot"
	Memcode    Producer = "memcode" // memcode's own agent (assigned at event creation, not here)
	Unknown    Producer = "unknown"
)

// Result is an attribution with its confidence and supporting evidence.
type Result struct {
	Producer   Producer `json:"producer"`
	Confidence string   `json:"confidence"` // low | medium | high
	Evidence   string   `json:"evidence"`
}

// Classify attributes a commit to a producer from its author, email and message.
func Classify(author, email, subject, body string) Result {
	text := strings.ToLower(subject + "\n" + body)
	el := strings.ToLower(email)
	hasCoAuthor := strings.Contains(text, "co-authored-by:")

	switch {
	case strings.Contains(el, "noreply@anthropic.com") ||
		strings.Contains(text, "co-authored-by: claude") ||
		strings.Contains(text, "generated with claude code") ||
		strings.Contains(text, "generated with [claude code]"):
		return Result{ClaudeCode, "high", "Anthropic email or Claude co-author trailer"}

	case strings.Contains(text, "co-authored-by: codex") ||
		strings.Contains(text, "generated with codex") ||
		(hasCoAuthor && strings.Contains(text, "openai")):
		return Result{Codex, "high", "Codex/OpenAI co-author trailer"}

	case strings.Contains(el, "cursor") || strings.Contains(text, "co-authored-by: cursor") ||
		strings.Contains(text, "generated with cursor"):
		return Result{Cursor, "medium", "Cursor signature"}

	case strings.Contains(text, "github copilot") || strings.Contains(text, "co-authored-by: copilot"):
		return Result{Copilot, "medium", "Copilot signature"}

	case hasCoAuthor:
		return Result{Unknown, "low", "unrecognized co-author trailer"}

	default:
		return Result{Human, "medium", "no agent signature"}
	}
}
