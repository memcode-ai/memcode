// Package geminiwire is the Gemini adapter (google.golang.org/genai) — the
// ONE implementation of the Gemini dialect for BOTH backends: the Developer
// API (API key) and Vertex AI (service-account JSON, passed in — credential
// RESOLUTION stays with the caller). Transport encoding only — no routing
// policy, no metering.
package gemini

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"cloud.google.com/go/auth/credentials"
	"github.com/memcode-ai/memcode/catalog"
	"github.com/memcode-ai/memcode/internal/providers/provcore"
	"github.com/memcode-ai/memcode/internal/wire"
	"google.golang.org/genai"
)

// Gemini is the strong-tier ModelProvider backed by Google's Gemini API via the
// native genai SDK. It implements ModelProvider + Streamer + WebSearcher +
// WebFetcher + Model() — the StrongProvider surface — so the Hybrid router can
// swap it in as the strong tier per-turn via Intent.Vendor.
//
// Gemini's wire shape (Content/Parts/FunctionDeclaration, iter.Seq2 streaming)
// differs from both OpenAI and Anthropic, so this is a native mapping rather
// than an oaClient wrapper. wire.Request → []*genai.Content (Role + Parts:
// Text / FunctionCall / FunctionResponse / InlineData for images). Effort maps
// onto ThinkingConfig.ThinkingBudget (high=max budget, medium=medium, low=low,
// off=0). WebSearch/WebFetch use Gemini's GoogleSearch tool. Gemini has no
// cross-turn reasoning round-trip (no encrypted_content / signature), so
// thinking blocks are per-call only and dropped on resend (same as the oaClient
// treatment of Anthropic thinking blocks).
//
// Two backends: when serviceAccountJSON is set, the client runs on Vertex AI
// with the GCP service account (paid quota). Otherwise
// it falls back to the Gemini Developer API with an API key (free-tier quota).
// EnvGeminiKey is the environment variable holding the Google AI API key.
const EnvGeminiKey = "GEMINI_API_KEY"

// EnvGCPSAKey names the GCP service-account JSON env var referenced in
// credential guidance (resolution itself stays with the caller).
const EnvGCPSAKey = "GCP_SERVICE_ACCOUNT_KEY"

func init() {
	// Teach the shared retry kernel this vendor's API error shape. genai's
	// APIError carries the HTTP status in Code but exposes no response header,
	// so Retry-After can't be honored here — backoff applies.
	provcore.RegisterErrorInfo(func(err error) (int, http.Header, bool) {
		var apiErr *genai.APIError
		if errors.As(err, &apiErr) {
			return apiErr.Code, nil, true
		}
		return 0, nil, false
	})
}

type Gemini struct {
	apiKey             string
	serviceAccountJSON []byte
	project            string
	location           string
	http               *http.Client
	baseURL            string // override for tests; "" → the SDK default
}

// NewGemini returns a client using the given Google AI API key. baseURL is left
// empty so the SDK defaults; tests override it via g.baseURL before use (same
// pattern as Anthropic/OpenAI).
func NewGemini(apiKey string) *Gemini {
	return &Gemini{
		apiKey: apiKey,
		http:   provcore.NewTurnHTTPClient(),
	}
}

// NewGeminiVertex returns a client that runs on Vertex AI using a GCP service
// account JSON key. project is the GCP project
// ID; location is the Vertex AI region (e.g. "global" or "us-central1").
func NewGeminiVertex(serviceAccountJSON []byte, project, location string) *Gemini {
	return &Gemini{
		serviceAccountJSON: serviceAccountJSON,
		project:            project,
		location:           location,
		http:               provcore.NewTurnHTTPClient(),
	}
}

// SetBaseURL points the adapter at a different Gemini host (tests, proxies).
// "" restores the SDK default.
func (g *Gemini) SetBaseURL(u string) { g.baseURL = u }

// BaseURL reports the configured override ("" = the SDK default).
func (g *Gemini) BaseURL() string { return g.baseURL }

// client builds a per-call genai client from the struct fields (same pattern as
// Anthropic/OpenAI: building per-call is cheap and makes the baseURL test
// override work). When serviceAccountJSON is set, the client uses Vertex AI
// with the service account credentials; otherwise it uses the Gemini Developer
// API with the API key.
func (g *Gemini) client(ctx context.Context) (*genai.Client, error) {
	if len(g.serviceAccountJSON) > 0 {
		// Vertex AI with a service account (paid quota).
		creds, err := credentials.DetectDefault(&credentials.DetectOptions{
			CredentialsJSON: g.serviceAccountJSON,
			Scopes:          []string{"https://www.googleapis.com/auth/cloud-platform"},
		})
		if err != nil {
			return nil, fmt.Errorf("gemini vertex credentials: %w", err)
		}
		cfg := &genai.ClientConfig{
			Backend:     genai.BackendVertexAI,
			Project:     g.project,
			Location:    g.location,
			Credentials: creds,
		}
		// Do NOT set HTTPClient here — the SDK creates an authenticated transport
		// (with the SA token + quota project header) only when HTTPClient is nil.
		// Setting our own client bypasses that, producing unauthenticated requests.
		if g.baseURL != "" {
			cfg.HTTPOptions = genai.HTTPOptions{BaseURL: g.baseURL}
		}
		return genai.NewClient(ctx, cfg)
	}
	// Gemini Developer API with an API key (free-tier quota).
	cfg := &genai.ClientConfig{
		APIKey:  g.apiKey,
		Backend: genai.BackendGeminiAPI,
	}
	if g.http != nil {
		cfg.HTTPClient = g.http
	}
	if g.baseURL != "" {
		cfg.HTTPOptions = genai.HTTPOptions{BaseURL: g.baseURL}
	}
	return genai.NewClient(ctx, cfg)
}

// Model returns the default (balanced-tier) model id — for display and the
// StrongProvider surface.
func (g *Gemini) Model() string { return catalog.ModelGeminiFlash }

// Complete satisfies the non-streamed contract by streaming under the hood and
// assembling the full Response (same rationale as Anthropic/OpenAI.Complete).
func (g *Gemini) Complete(ctx context.Context, r wire.Request) (wire.Response, error) {
	return g.Stream(ctx, r, wire.StreamHandler{})
}

// mapEffort translates memcode's abstract Effort onto Gemini's ThinkingConfig.
// EffortHigh → a generous thinking budget (the model auto-caps), medium → a
// moderate budget, low → a small budget, off → 0 (disables thinking). Returns
// nil when thinking is off so the request omits the field entirely.
func (g *Gemini) mapEffort(e wire.Effort) *genai.ThinkingConfig {
	switch e {
	case wire.EffortHigh:
		// A high budget lets the model think deeply; Gemini auto-caps at its
		// model limit. 24576 is a generous budget for Gemini 3 Pro/Flash.
		b := int32(24576)
		return &genai.ThinkingConfig{ThinkingBudget: &b}
	case wire.EffortMedium:
		b := int32(8192)
		return &genai.ThinkingConfig{ThinkingBudget: &b}
	case wire.EffortLow:
		b := int32(2048)
		return &genai.ThinkingConfig{ThinkingBudget: &b}
	default: // EffortOff
		b := int32(0)
		return &genai.ThinkingConfig{ThinkingBudget: &b}
	}
}

// buildContents maps the block-structured conversation onto Gemini's []*Content
// shape. Each message becomes one Content (Role "user" or "model"); blocks
// become Parts (Text, FunctionCall, FunctionResponse, InlineData for images).
// Thinking blocks are DROPPED (Gemini has no cross-turn reasoning round-trip —
// same as the oaClient's treatment of Anthropic thinking).
func (g *Gemini) buildContents(r wire.Request) []*genai.Content {
	// First pass: map each tool_use id → its function name. Gemini's FunctionResponse
	// REQUIRES a name that matches the FunctionCall it answers, but tool_result blocks
	// carry only the ToolUseID (no name) — so without this the response name was empty
	// and Vertex 400'd on every multi-turn tool conversation (turn 2 onward).
	callName := map[string]string{}
	for _, msg := range r.Messages {
		for _, b := range msg.Blocks {
			if b.Type == "tool_use" && b.ID != "" {
				callName[b.ID] = b.Name
			}
		}
	}
	out := make([]*genai.Content, 0, len(r.Messages))
	for _, msg := range r.Messages {
		c := &genai.Content{}
		switch msg.Role {
		case "assistant":
			c.Role = "model"
		default:
			c.Role = "user"
		}
		for _, b := range msg.Blocks {
			p := g.blockToPart(b, callName)
			if p != nil {
				c.Parts = append(c.Parts, p)
			}
			// A document inside a tool_result can't ride the FunctionResponse map
			// (structured JSON only) — attach it as a sibling inline-data part in the
			// same user content, so the model still reads the PDF natively.
			if b.Type == "tool_result" {
				for _, cb := range b.ContentBlocks {
					if cb.Type == "document" && cb.Source != nil {
						if dp := inlineBlobPart(cb); dp != nil {
							c.Parts = append(c.Parts, dp)
						}
					}
				}
			}
		}
		if len(c.Parts) > 0 {
			out = append(out, c)
		}
	}
	return out
}

// inlineBlobPart decodes a media block (image or document) into a Gemini
// inline-data part. nil when the block has no source or malformed base64.
func inlineBlobPart(b wire.Block) *genai.Part {
	if b.Source == nil {
		return nil
	}
	data, err := base64.StdEncoding.DecodeString(b.Source.Data)
	if err != nil {
		return nil
	}
	return &genai.Part{InlineData: &genai.Blob{Data: data, MIMEType: b.Source.MediaType}}
}

// blockToPart maps one wire.Block onto a genai.Part. Text → Text part; image
// → InlineData (base64-decoded back to bytes); tool_use → FunctionCall;
// tool_result → FunctionResponse; thinking → dropped (per-call only on Gemini).
// toolUseFromPart converts a Gemini functionCall part into a tool_use block.
// It is the exact inverse of blockToPart's tool_use case, and exists as its own
// function so the round-trip can be tested without a live stream — the bug it
// guards against is only reachable across TWO turns.
//
// The thought signature is the load-bearing part. Gemini 3.x issues one with
// every functionCall and REQUIRES it echoed back when that call is replayed:
//
//	400 INVALID_ARGUMENT: Function call is missing a thought_signature in
//	functionCall parts.
//
// Dropping it made every tool-using Gemini turn fail on turn 2, across the
// whole model family. It is opaque provider data — stored base64 so it survives
// the JSON wire, echoed verbatim, never interpreted.
func toolUseFromPart(part *genai.Part) (wire.Block, bool) {
	fc := part.FunctionCall
	if fc == nil || fc.Name == "" {
		return wire.Block{}, false
	}
	args, _ := json.Marshal(fc.Args)
	if len(args) == 0 || string(args) == "null" {
		args = json.RawMessage("{}")
	}
	blk := wire.Block{Type: "tool_use", ID: fc.ID, Name: fc.Name, Input: args}
	if len(part.ThoughtSignature) > 0 {
		blk.Signature = base64.StdEncoding.EncodeToString(part.ThoughtSignature)
	}
	return blk, true
}

func (g *Gemini) blockToPart(b wire.Block, callName map[string]string) *genai.Part {
	switch b.Type {
	case "text":
		return &genai.Part{Text: b.Text}
	case "image", "document":
		// Both ride Gemini's inline blob (images as their mime, PDFs as
		// application/pdf) — Gemini reads documents natively the same way.
		if p := inlineBlobPart(b); p != nil {
			return p
		}
		return &genai.Part{Text: ""}
	case "tool_use":
		var args map[string]any
		if len(b.Input) > 0 {
			if err := json.Unmarshal(b.Input, &args); err != nil {
				provcore.LogToolInputMalformed("gemini", err)
			}
		}
		part := &genai.Part{FunctionCall: &genai.FunctionCall{
			ID:   b.ID,
			Name: b.Name,
			Args: args,
		}}
		// Echo the thought signature Gemini issued for this call. Required on
		// replay (see the capture site) — a missing one is a hard 400, not a
		// degraded response. Absent for a call that came from another vendor,
		// which is correct: there is nothing of Gemini's to return.
		if b.Signature != "" {
			if sig, err := base64.StdEncoding.DecodeString(b.Signature); err == nil {
				part.ThoughtSignature = sig
			}
		}
		return part
	case "tool_result":
		var resp map[string]any
		if b.IsError {
			resp = map[string]any{"error": b.Content}
		} else {
			resp = map[string]any{"output": b.Content}
		}
		// Fold structured content blocks into the response text.
		if len(b.ContentBlocks) > 0 {
			var texts []string
			for _, cb := range b.ContentBlocks {
				if cb.Type == "text" {
					texts = append(texts, cb.Text)
				}
			}
			if len(texts) > 0 {
				resp["output"] = strings.Join(texts, "\n")
			}
		}
		// Name MUST match the FunctionCall this answers — resolve it from the id map
		// (tool_result blocks don't carry the name themselves).
		name := b.Name
		if name == "" {
			name = callName[b.ToolUseID]
		}
		return &genai.Part{FunctionResponse: &genai.FunctionResponse{
			ID:       b.ToolUseID,
			Name:     name,
			Response: resp,
		}}
	case "thinking", "redacted_thinking":
		// Dropped — Gemini has no cross-turn reasoning round-trip (same as the
		// oaClient's treatment of Anthropic thinking blocks on OpenAI/Fireworks).
		return nil
	default:
		return &genai.Part{Text: b.Text}
	}
}

// buildTools maps wire.ToolDef onto Gemini's FunctionDeclaration. The input
// schema (a JSON-Schema map) is converted to a genai.Schema via
// schemaFromMap. Returns nil when there are no tools (so the field is omitted).
func (g *Gemini) buildTools(r wire.Request) []*genai.Tool {
	if len(r.Tools) == 0 {
		return nil
	}
	decls := make([]*genai.FunctionDeclaration, 0, len(r.Tools))
	for _, t := range r.Tools {
		fd := &genai.FunctionDeclaration{
			Name:        t.Name,
			Description: t.Description,
		}
		if t.InputSchema != nil {
			fd.Parameters = schemaFromMap(t.InputSchema)
		}
		decls = append(decls, fd)
	}
	return []*genai.Tool{{FunctionDeclarations: decls}}
}

// schemaFromMap converts a JSON-Schema map (wire.ToolDef.InputSchema) onto a
// genai.Schema. Recurses into properties and items. Unknown keys are carried
// via the nearest-typed fallback (Gemini's Schema is a subset of JSON-Schema).
func schemaFromMap(m map[string]any) *genai.Schema {
	s := &genai.Schema{}
	if t, ok := m["type"].(string); ok {
		s.Type = mapJSONTypeToGenai(t)
	}
	if d, ok := m["description"].(string); ok {
		s.Description = d
	}
	if props, ok := m["properties"].(map[string]any); ok {
		s.Properties = make(map[string]*genai.Schema, len(props))
		for k, v := range props {
			if vm, ok := v.(map[string]any); ok {
				s.Properties[k] = schemaFromMap(vm)
			}
		}
	}
	if req, ok := m["required"].([]any); ok {
		for _, r := range req {
			if rs, ok := r.(string); ok {
				s.Required = append(s.Required, rs)
			}
		}
	}
	if items, ok := m["items"].(map[string]any); ok {
		s.Items = schemaFromMap(items)
	}
	if enum, ok := m["enum"].([]any); ok {
		for _, e := range enum {
			if es, ok := e.(string); ok {
				s.Enum = append(s.Enum, es)
			}
		}
	}
	return s
}

// mapJSONTypeToGenai maps a JSON-Schema type string onto Gemini's Type enum.
func mapJSONTypeToGenai(t string) genai.Type {
	switch t {
	case "string":
		return genai.TypeString
	case "number":
		return genai.TypeNumber
	case "integer":
		return genai.TypeInteger
	case "boolean":
		return genai.TypeBoolean
	case "array":
		return genai.TypeArray
	case "object":
		return genai.TypeObject
	case "null":
		return genai.TypeNULL
	default:
		return genai.TypeUnspecified
	}
}

// buildConfig assembles the GenerateContentConfig for a request: system
// instruction, tools, thinking budget, max output tokens, and tool choice
// (forcing a named tool when set).
func (g *Gemini) buildConfig(r wire.Request, maxTok int) *genai.GenerateContentConfig {
	cfg := &genai.GenerateContentConfig{
		ThinkingConfig: g.mapEffort(r.Effort),
	}
	if maxTok > 0 {
		// 0 = uncapped: omit the field so the model's own max output applies (an
		// arbitrary default silently truncated verdicts on reasoning models).
		cfg.MaxOutputTokens = int32(maxTok)
	}
	if sys := r.System; sys != "" {
		if r.SystemVolatile != "" {
			sys = strings.TrimRight(sys, "\n") + "\n\n" + r.SystemVolatile
		}
		cfg.SystemInstruction = &genai.Content{Role: "user", Parts: []*genai.Part{{Text: sys}}}
	}
	if tools := g.buildTools(r); tools != nil {
		cfg.Tools = tools
		if r.ToolChoice != "" {
			cfg.ToolConfig = &genai.ToolConfig{
				FunctionCallingConfig: &genai.FunctionCallingConfig{
					Mode:                 genai.FunctionCallingConfigModeAny,
					AllowedFunctionNames: []string{r.ToolChoice},
				},
			}
		}
	}
	return cfg
}

// Stream sends a streaming GenerateContentStream request, forwarding text/usage
// to h as they arrive, and returns the fully assembled Response (so the agent
// loop is identical to the non-streaming path).
func (g *Gemini) Stream(ctx context.Context, r wire.Request, h wire.StreamHandler) (wire.Response, error) {
	if g.apiKey == "" && len(g.serviceAccountJSON) == 0 {
		return wire.Response{}, fmt.Errorf("no Gemini credentials (set %s or %s)", EnvGeminiKey, EnvGCPSAKey)
	}
	maxTok := r.MaxTokens // 0 = uncapped (field omitted in buildConfig; model max applies)
	model := r.Model
	if model == "" {
		model = catalog.ModelGeminiFlash
	}
	contents := g.buildContents(r)
	cfg := g.buildConfig(r, maxTok)

	cl, err := g.client(ctx)
	if err != nil {
		return wire.Response{}, fmt.Errorf("gemini client: %w", err)
	}

	// Shared emitted-aware retry (a stream can't resume mid-flight, so it only
	// re-attempts when nothing was forwarded to h yet). genai's *APIError is
	// recognized via the extractor registered at init.
	return provcore.StreamWithRetry(ctx, func() (wire.Response, bool, error) {
		return g.streamOnce(ctx, cl, model, contents, cfg, r, h)
	})
}

// streamOnce runs a single streaming attempt. emitted reports whether any content
// bytes were forwarded to h (after which a retry would duplicate output).
func (g *Gemini) streamOnce(ctx context.Context, cl *genai.Client, model string, contents []*genai.Content, cfg *genai.GenerateContentConfig, r wire.Request, h wire.StreamHandler) (_ wire.Response, emitted bool, _ error) {
	var text strings.Builder
	var blocks []wire.Block
	var inTok, outTok, cacheRead int
	var stopReason string
	var fnCalls []wire.Block

	for result, err := range cl.Models.GenerateContentStream(ctx, model, contents, cfg) {
		if err != nil {
			// Return whatever the vendor already billed before the cut — a cancelled or
			// failed stream still costs the tokens consumed so far, and the Runner
			// meters a response with usage even on error (mirrors the Anthropic path).
			partial := wire.Response{
				InputTokens:     inTok - cacheRead,
				CacheReadTokens: cacheRead,
				OutputTokens:    outTok,
				Model:           model,
				Backend:         "gemini",
			}
			if ctx.Err() != nil {
				return partial, emitted, ctx.Err()
			}
			if isGeminiOverflow(err) {
				return partial, emitted, &provcore.ContextOverflowError{Backend: "gemini", Message: err.Error()}
			}
			return partial, emitted, fmt.Errorf("gemini stream: %w", err) // %w keeps errors.As working for the retry loop
		}
		// Usage metadata (arrives on each chunk; the final is authoritative).
		if u := result.UsageMetadata; u != nil {
			inTok = int(u.PromptTokenCount)
			// Thinking tokens are billed by Google as output but reported separately
			// from CandidatesTokenCount — include them or a reasoning-heavy turn
			// under-meters output by a large multiple (up to the 24576 thinking budget).
			outTok = int(u.CandidatesTokenCount) + int(u.ThoughtsTokenCount)
			cacheRead = int(u.CachedContentTokenCount)
			if h.Usage != nil {
				h.Usage(inTok, outTok)
			}
		}
		if len(result.Candidates) == 0 {
			continue
		}
		cand := result.Candidates[0]
		if cand == nil {
			continue
		}
		if cand.FinishReason != "" {
			stopReason = geminiStopReason(string(cand.FinishReason))
		}
		if cand.Content == nil {
			continue
		}
		for _, part := range cand.Content.Parts {
			if part == nil {
				continue
			}
			if part.Thought {
				continue // reasoning — per-call only, not streamed to the user
			}
			if part.Text != "" {
				text.WriteString(part.Text)
				if h.Text != nil {
					h.Text(part.Text)
					emitted = true
				}
			}
			if blk, ok := toolUseFromPart(part); ok {
				fnCalls = append(fnCalls, blk)
			}
		}
	}

	// Assemble blocks: text first (if any), then function calls.
	if t := text.String(); t != "" {
		blocks = append(blocks, wire.Block{Type: "text", Text: t})
	}
	blocks = append(blocks, fnCalls...)

	if stopReason == "" {
		if len(fnCalls) > 0 {
			stopReason = "tool_use"
		} else if len(blocks) > 0 {
			stopReason = "end_turn"
		}
	}
	out := wire.Response{
		StopReason:      stopReason,
		Blocks:          blocks,
		InputTokens:     inTok - cacheRead,
		CacheReadTokens: cacheRead,
		OutputTokens:    outTok,
		Model:           model,
		Backend:         "gemini",
	}
	if len(r.Tools) > 0 && len(fnCalls) > 0 {
		out.ToolOrigin = "structured_gemini"
	}
	return out, emitted, nil
}

// geminiStopReason maps Gemini's FinishReason onto the provider-neutral
// vocabulary the agent loop already speaks.
func geminiStopReason(s string) string {
	switch s {
	case "STOP":
		return "end_turn"
	case "MAX_TOKENS":
		return "max_tokens"
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII":
		return "safety"
	default:
		return strings.ToLower(s)
	}
}

// isGeminiOverflow reports whether a Gemini API error is a context-window
// overflow — the shared token/context phrases only. RESOURCE_EXHAUSTED is
// deliberately NOT matched: that is Gemini's 429 rate-limit status, and
// classifying it as overflow sent rate-limited turns into compaction instead
// of the retry path (it now retries as a 429 via the registered extractor).
func isGeminiOverflow(err error) bool {
	return err != nil && provcore.IsOverflowMessage(err.Error())
}

// WebSearch answers a query using Gemini's GoogleSearch tool, returning the
// synthesized text. The model runs the searches within the single turn; we
// return the assistant's text parts.
func (g *Gemini) WebSearch(ctx context.Context, query string) (string, wire.Response, error) {
	if g.apiKey == "" && len(g.serviceAccountJSON) == 0 {
		return "", wire.Response{}, fmt.Errorf("no Gemini credentials (set %s or %s)", EnvGeminiKey, EnvGCPSAKey)
	}
	cl, err := g.client(ctx)
	if err != nil {
		return "", wire.Response{}, fmt.Errorf("gemini client: %w", err)
	}
	cfg := &genai.GenerateContentConfig{
		SystemInstruction: &genai.Content{Role: "user", Parts: []*genai.Part{{Text: provcore.WebSearchSystemPrompt}}},
		MaxOutputTokens:   3000,
		Tools:             []*genai.Tool{{GoogleSearch: &genai.GoogleSearch{}}},
	}
	resp, err := provcore.WithRetry(ctx, func() (*genai.GenerateContentResponse, error) {
		return cl.Models.GenerateContent(ctx, catalog.ModelGeminiFlash,
			[]*genai.Content{{Role: "user", Parts: []*genai.Part{{Text: query}}}}, cfg)
	})
	if err != nil {
		return "", wire.Response{Model: catalog.ModelGeminiFlash, Backend: "gemini"}, err
	}
	return strings.TrimSpace(resp.Text()), geminiSideUsage(resp), nil
}

// geminiSideUsage maps a GenerateContent result onto the neutral usage shape for
// side-channel metering (websearch/webfetch run on Flash). SearchCount is
// deliberately left 0: Google Search grounding bills per grounded PROMPT (not per
// search, with a free daily tier), the genai usage metadata exposes no billed
// search counter, and models.json search_fees carries no "gemini" entry — so no
// surcharge is modeled for this vendor. Revisit if grounding spend shows up.
func geminiSideUsage(resp *genai.GenerateContentResponse) wire.Response {
	out := wire.Response{Model: catalog.ModelGeminiFlash, Backend: "gemini"}
	if u := resp.UsageMetadata; u != nil {
		out.InputTokens = int(u.PromptTokenCount)
		out.OutputTokens = int(u.CandidatesTokenCount)
	}
	return out
}

// WebFetch retrieves a URL using Gemini's GoogleSearch tool with the URL in the
// prompt (Gemini has no dedicated url_fetch tool; the model retrieves and
// summarizes the URL content). Returns the readable content as text.
func (g *Gemini) WebFetch(ctx context.Context, url string) (string, wire.Response, error) {
	if g.apiKey == "" && len(g.serviceAccountJSON) == 0 {
		return "", wire.Response{}, fmt.Errorf("no Gemini credentials (set %s or %s)", EnvGeminiKey, EnvGCPSAKey)
	}
	cl, err := g.client(ctx)
	if err != nil {
		return "", wire.Response{}, fmt.Errorf("gemini client: %w", err)
	}
	cfg := &genai.GenerateContentConfig{
		MaxOutputTokens: 8192,
		Tools:           []*genai.Tool{{GoogleSearch: &genai.GoogleSearch{}}},
	}
	prompt := "Fetch this URL and return its full readable content verbatim as markdown, no commentary: " + url
	resp, err := provcore.WithRetry(ctx, func() (*genai.GenerateContentResponse, error) {
		return cl.Models.GenerateContent(ctx, catalog.ModelGeminiFlash,
			[]*genai.Content{{Role: "user", Parts: []*genai.Part{{Text: prompt}}}}, cfg)
	})
	if err != nil {
		return "", wire.Response{Model: catalog.ModelGeminiFlash, Backend: "gemini"}, err
	}
	return strings.TrimSpace(resp.Text()), geminiSideUsage(resp), nil
}
