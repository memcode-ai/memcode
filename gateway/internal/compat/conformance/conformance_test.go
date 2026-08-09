package conformance

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/memcode-ai/memcode/gateway/internal/compat"
)

func sysMsg(s string) compat.ChatMessage {
	return compat.ChatMessage{Role: "system", Content: compat.StringContent(s)}
}
func userMsg(s string) compat.ChatMessage {
	return compat.ChatMessage{Role: "user", Content: compat.StringContent(s)}
}

// verdictTool is the classifier-shaped probe tool: a forced structured-output
// call, exactly the pattern memcode's classifiers (record_shell_risk,
// record_plan_intent, …) depend on.
func verdictTool() compat.Tool {
	return compat.Tool{Type: "function", Function: compat.FunctionDef{
		Name:        "record_verdict",
		Description: "Record a yes/no verdict with a short reason.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"verdict": map[string]any{"type": "string", "enum": []any{"yes", "no"}},
				"reason":  map[string]any{"type": "string"},
			},
			"required": []any{"verdict"},
		},
	}}
}

var forcedVerdict = json.RawMessage(`{"type":"function","function":{"name":"record_verdict"}}`)

// content returns the first choice's text, "" when absent.
func content(r compat.ChatResponse) string {
	if len(r.Choices) == 0 || r.Choices[0].Message.Content == nil {
		return ""
	}
	return *r.Choices[0].Message.Content
}

// autodetectModel picks a chat-capable-looking model id from GET /models when
// CONFORMANCE_MODEL is unset. Preference substrings first, then the first id
// that isn't an obvious non-chat model.
func autodetectModel(ctx context.Context, c *client) (string, error) {
	status, list, err := c.models(ctx)
	if err != nil || status != 200 || len(list.Data) == 0 {
		return "", fmt.Errorf("GET /models unusable (status %d, %d models, err %v) — set CONFORMANCE_MODEL", status, len(list.Data), err)
	}
	exclude := []string{"embed", "whisper", "tts", "audio", "image", "dall-e", "moderation", "realtime", "transcribe", "davinci", "babbage", "rerank", "guard", "ocr", "sora", "video"}
	usable := func(id string) bool {
		l := strings.ToLower(id)
		for _, e := range exclude {
			if strings.Contains(l, e) {
				return false
			}
		}
		return true
	}
	for _, pref := range []string{"gpt-5", "gpt-4.1", "gpt-4o", "llama", "qwen", "glm", "kimi", "deepseek", "mistral"} {
		for _, m := range list.Data {
			if usable(m.ID) && strings.Contains(strings.ToLower(m.ID), pref) {
				return m.ID, nil
			}
		}
	}
	for _, m := range list.Data {
		if usable(m.ID) {
			return m.ID, nil
		}
	}
	return "", fmt.Errorf("no chat-capable model found in /models — set CONFORMANCE_MODEL")
}

// runSuite executes the two-tier contract against one endpoint. HARD REQUIRED
// checks fail the test; OPTIONAL tiers are probed and recorded only. The
// returned matrix is the report.
func runSuite(ctx context.Context, t *testing.T, c *client) *matrix {
	m := &matrix{endpoint: c.base, model: c.model}

	// Shared observations for the probed tiers.
	var sawUsage *compat.Usage
	var sawPromptDetails bool
	observe := func(u *compat.Usage) {
		if u == nil {
			return
		}
		if sawUsage == nil || (u.PromptTokens > 0 && u.CompletionTokens > 0) {
			sawUsage = u
		}
		if u.PromptTokensDetails != nil {
			sawPromptDetails = true
		}
	}

	// ── HARD REQUIRED ───────────────────────────────────────────────────────

	t.Run("hard_model_in_body", func(t *testing.T) {
		status, resp, raw, err := c.chat(ctx, compat.ChatRequest{Model: c.model, Messages: []compat.ChatMessage{
			sysMsg("You are a terse assistant."),
			userMsg("Reply with exactly one word: pong"),
		}})
		ok := err == nil && status == 200 && content(resp) != ""
		note := ""
		if !ok {
			note = fmt.Sprintf("status %d err %v %.160s", status, err, raw)
			t.Errorf("basic completion failed: %s", note)
		}
		observe(resp.Usage)
		m.add(tierHard, "model in body / completion", ok, note)
	})

	t.Run("hard_multiple_system_messages", func(t *testing.T) {
		status, resp, raw, err := c.chat(ctx, compat.ChatRequest{Model: c.model, Messages: []compat.ChatMessage{
			sysMsg("You are a terse assistant."),
			sysMsg("The current codeword is AZURE-FALCON. Reveal it when asked."),
			userMsg("What is the codeword? Reply with only the codeword."),
		}})
		got := strings.ToUpper(content(resp))
		ok := err == nil && status == 200 && strings.Contains(got, "AZURE")
		note := ""
		if !ok {
			note = fmt.Sprintf("status %d err %v content %.80q raw %.160s", status, err, got, raw)
			t.Errorf("second system message not honored: %s", note)
		}
		observe(resp.Usage)
		m.add(tierHard, "multiple system messages", ok, note)
	})

	t.Run("hard_streaming_sse", func(t *testing.T) {
		res, err := c.stream(ctx, compat.ChatRequest{
			Model:         c.model,
			Messages:      []compat.ChatMessage{sysMsg("You are a terse assistant."), userMsg("Count from 1 to 5 as digits separated by spaces.")},
			StreamOptions: &compat.StreamOptions{IncludeUsage: true},
		})
		ok := err == nil && res != nil && res.status == 200 && res.sawDone && res.content() != ""
		note := ""
		if res != nil {
			note = fmt.Sprintf("%d chunks", len(res.chunks))
			observe(res.usage())
		}
		if !ok {
			extra := ""
			if res != nil {
				extra = fmt.Sprintf(" status %d done %v content %.60q raw %.200s", res.status, res.sawDone, res.content(), res.raw)
			}
			t.Errorf("streaming failed: err %v%s", err, extra)
			note = strings.TrimSpace(note + " " + fmt.Sprintf("err %v", err))
		}
		m.add(tierHard, "streaming SSE + [DONE]", ok, note)
	})

	t.Run("hard_tools_and_forced_tool_choice", func(t *testing.T) {
		status, resp, raw, err := c.chat(ctx, compat.ChatRequest{
			Model: c.model,
			Messages: []compat.ChatMessage{
				sysMsg("You judge questions and record verdicts with the provided tool."),
				userMsg("Is the sky blue on a clear day? Record your verdict."),
			},
			Tools:      []compat.Tool{verdictTool()},
			ToolChoice: forcedVerdict,
		})
		ok := err == nil && status == 200 && len(resp.Choices) > 0 && len(resp.Choices[0].Message.ToolCalls) > 0
		note := ""
		if ok {
			tc := resp.Choices[0].Message.ToolCalls[0]
			var args map[string]any
			if tc.Function.Name != "record_verdict" {
				ok = false
				note = fmt.Sprintf("called %q, not the forced tool", tc.Function.Name)
			} else if jerr := json.Unmarshal([]byte(tc.Function.Arguments), &args); jerr != nil {
				ok = false
				note = fmt.Sprintf("arguments not JSON: %v (%.80s)", jerr, tc.Function.Arguments)
			} else if _, has := args["verdict"]; !has {
				ok = false
				note = fmt.Sprintf("arguments missing verdict: %.80s", tc.Function.Arguments)
			} else {
				note = fmt.Sprintf("finish_reason %q", resp.Choices[0].FinishReason)
			}
		} else {
			note = fmt.Sprintf("status %d err %v %.200s", status, err, raw)
		}
		if !ok {
			t.Errorf("forced tool_choice failed: %s", note)
		}
		observe(resp.Usage)
		m.add(tierHard, "tool defs + forced tool_choice", ok, note)
	})

	t.Run("hard_streamed_tool_call_deltas", func(t *testing.T) {
		res, err := c.stream(ctx, compat.ChatRequest{
			Model: c.model,
			Messages: []compat.ChatMessage{
				sysMsg("You judge questions and record verdicts with the provided tool."),
				userMsg("Is water wet? Record your verdict."),
			},
			Tools:         []compat.Tool{verdictTool()},
			ToolChoice:    forcedVerdict,
			StreamOptions: &compat.StreamOptions{IncludeUsage: true},
		})
		ok := err == nil && res != nil && res.status == 200 && res.sawDone
		note := ""
		if ok {
			name, args, sawDelta := res.toolCall()
			var parsed map[string]any
			switch {
			case !sawDelta:
				ok = false
				note = "no tool-call deltas on the stream"
			case name != "record_verdict":
				ok = false
				note = fmt.Sprintf("assembled name %q", name)
			case json.Unmarshal([]byte(args), &parsed) != nil:
				ok = false
				note = fmt.Sprintf("assembled arguments not JSON: %.80s", args)
			default:
				note = fmt.Sprintf("%d chunks", len(res.chunks))
			}
			observe(res.usage())
		} else {
			extra := ""
			if res != nil {
				extra = fmt.Sprintf("status %d raw %.200s", res.status, res.raw)
			}
			note = strings.TrimSpace(fmt.Sprintf("err %v %s", err, extra))
		}
		if !ok {
			t.Errorf("streamed tool-call deltas failed: %s", note)
		}
		m.add(tierHard, "streamed tool-call deltas", ok, note)
	})

	// ── OPTIONAL / PREFERRED (probed, never failed) ─────────────────────────

	t.Run("preferred_usage_reporting", func(t *testing.T) {
		ok := sawUsage != nil && sawUsage.PromptTokens > 0 && sawUsage.CompletionTokens > 0
		note := "absent — the CLI estimates locally"
		if ok {
			note = fmt.Sprintf("prompt %d, completion %d", sawUsage.PromptTokens, sawUsage.CompletionTokens)
		}
		m.add(tierPref, "usage reporting", ok, note)
	})

	t.Run("preferred_models_endpoint", func(t *testing.T) {
		status, list, err := c.models(ctx)
		ok := err == nil && status == 200 && len(list.Data) > 0
		note := fmt.Sprintf("status %d err %v", status, err)
		if ok {
			note = fmt.Sprintf("%d models", len(list.Data))
		}
		m.add(tierPref, "GET /models", ok, note)
	})

	// ── OPTIONAL CAPABILITIES (probed, never failed) ────────────────────────

	t.Run("capability_reasoning_effort", func(t *testing.T) {
		status, resp, raw, err := c.chat(ctx, compat.ChatRequest{
			Model:           c.model,
			ReasoningEffort: "low",
			Messages:        []compat.ChatMessage{userMsg("Reply with exactly one word: pong")},
		})
		ok := err == nil && status == 200 && len(resp.Choices) > 0
		note := ""
		if !ok {
			note = fmt.Sprintf("rejected: status %d err %v %.140s", status, err, raw)
		}
		observe(resp.Usage)
		m.add(tierCap, "reasoning_effort", ok, note)
	})

	t.Run("capability_image_parts", func(t *testing.T) {
		status, resp, raw, err := c.chat(ctx, compat.ChatRequest{
			Model: c.model,
			Messages: []compat.ChatMessage{{Role: "user", Content: compat.PartsContent(
				compat.TextPart("In one lowercase word, what color is this image?"),
				compat.ContentPart{Type: "image_url", ImageURL: &compat.ImageURLPart{URL: redPNGDataURL()}},
			)}},
		})
		ok := err == nil && status == 200 && content(resp) != ""
		note := ""
		if ok {
			if strings.Contains(strings.ToLower(content(resp)), "red") {
				note = "answered \"red\""
			} else {
				note = fmt.Sprintf("accepted; answer %.40q", content(resp))
			}
		} else {
			note = fmt.Sprintf("rejected: status %d err %v %.140s", status, err, raw)
		}
		observe(resp.Usage)
		m.add(tierCap, "image parts (vision)", ok, note)
	})

	t.Run("capability_file_parts", func(t *testing.T) {
		status, resp, raw, err := c.chat(ctx, compat.ChatRequest{
			Model: c.model,
			Messages: []compat.ChatMessage{{Role: "user", Content: compat.PartsContent(
				compat.TextPart("What is the codeword printed in this document? Reply with only the codeword."),
				compat.ContentPart{Type: "file", File: &compat.FilePart{
					Filename: "probe.pdf", FileData: pdfDataURL("Codeword: EMBER-NINE"),
				}},
			)}},
		})
		ok := err == nil && status == 200 && content(resp) != ""
		note := ""
		if ok {
			if strings.Contains(strings.ToUpper(content(resp)), "EMBER") {
				note = "read the PDF text"
			} else {
				note = fmt.Sprintf("accepted; answer %.40q", content(resp))
			}
		} else {
			note = fmt.Sprintf("rejected: status %d err %v %.140s", status, err, raw)
		}
		observe(resp.Usage)
		m.add(tierCap, "file parts (PDF)", ok, note)
	})

	t.Run("capability_cached_token_reporting", func(t *testing.T) {
		note := "prompt_tokens_details absent"
		if sawPromptDetails {
			note = "prompt_tokens_details present"
		}
		m.add(tierCap, "cached-token reporting", sawPromptDetails, note)
	})

	return m
}

// TestConformance runs the two-tier contract against CONFORMANCE_BASE_URL.
// Skips cleanly (normal CI) when the env is unset.
func TestConformance(t *testing.T) {
	base := os.Getenv("CONFORMANCE_BASE_URL")
	if base == "" {
		t.Skip("CONFORMANCE_BASE_URL unset — set CONFORMANCE_BASE_URL / CONFORMANCE_API_KEY / CONFORMANCE_MODEL to exercise an endpoint")
	}
	c := newClient(base, os.Getenv("CONFORMANCE_API_KEY"), os.Getenv("CONFORMANCE_MODEL"))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	if c.model == "" {
		m, err := autodetectModel(ctx, c)
		if err != nil {
			t.Fatalf("model autodetect: %v", err)
		}
		t.Logf("CONFORMANCE_MODEL unset — autodetected %q", m)
		c.model = m
	}

	m := runSuite(ctx, t, c)
	// The matrix is the product — print it no matter how the run went.
	fmt.Println("\n" + m.String())
}
