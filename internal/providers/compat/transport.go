// Package compat is the CLI's ONLY turn transport: a stdlib-only HTTP client
// speaking OpenAI-compat chat/completions — streaming SSE, tools + forced
// tool_choice, image/file parts — against any base URL: the memcode gateway
// at {api}/v1 (Memcode=true: the optional extensions ride) or any arbitrary
// compat endpoint (Memcode=false: pure standard wire). The request shape is
// IDENTICAL either way — no memcode headers exist; model selection arrives
// already made (req.Pin, stamped by cli/internal/llm's policy). The transport
// itself carries no endpoint or routing knowledge.
package compat

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/memcode-ai/memcode/internal/wire"
)

// Config configures a Transport.
type Config struct {
	// Compose, when set, renders a legacy-shaped side call (Mode stamped,
	// doctrine not yet composed) into the two-system form before encoding. The
	// CALLER owns doctrine — the CLI passes its renderer; server-side lane use
	// leaves it nil.
	Compose func(wire.Request) (wire.Request, error)
	// Salvage enables the tool-call salvage net for small open models that
	// emit tool calls as TEXT (wrapped or bare JSON) instead of the structured
	// envelope — plus the MiniMax leak strip. The gateway's cheap lane runs
	// with this on; leave off for well-behaved backends.
	Salvage bool
	// Lane selects the SERVER-SIDE lane error contract (LaneRequestError for
	// request-shaped 4xx, generic errors otherwise) instead of the client
	// sentinel mapping, plus the lane's model-conditional reasoning_effort
	// vocabulary and the legacy max_tokens spelling.
	Lane bool

	// BaseURL is the FULL compat base including any path prefix,
	// e.g. https://code.memcode.ai/v1 — {base}/chat/completions is the
	// turn endpoint.
	BaseURL string
	// Token is the bearer credential; "" sends no Authorization header (a
	// keyless local endpoint).
	Token string
	// Memcode enables the memcode-backend extensions: memcode_opaque reasoning
	// round-trip (attach AND re-extract) and the memcode response object.
	// False = pure standard wire (arbitrary endpoints).
	Memcode bool
	// Model is the endpoint's session model, used when a request doesn't pin
	// one (Request.Pin). Arbitrary endpoints have no Automatic — every request
	// must name a concrete model — so with Memcode false and neither a pin nor
	// this default, calls fail with an actionable error instead of sending the
	// gateway sentinel "auto" to an endpoint that can't serve it.
	Model string
	// HTTPClient overrides the default client (tests, custom timeouts).
	HTTPClient *http.Client
}

// Transport speaks the compat wire to one endpoint. Safe for concurrent use
// after construction (SetRetryNotify is wiring-time, like the SDK client's).
type Transport struct {
	base    string
	token   string
	memcode bool
	model   string // endpoint default model (Config.Model)
	compose func(wire.Request) (wire.Request, error)
	salvage bool
	lane    bool
	http    *http.Client
	// retryNotify, when set, is called before each retry backoff so the host
	// can surface "retrying…". Same contract as the SDK client's.
	retryNotify func(attempt int, err error, delay time.Duration)
}

// New returns a Transport for one compat endpoint.
func New(cfg Config) *Transport {
	t := &Transport{
		base:    strings.TrimRight(cfg.BaseURL, "/"),
		token:   cfg.Token,
		memcode: cfg.Memcode,
		model:   cfg.Model,
		http:    cfg.HTTPClient,
		compose: cfg.Compose,
		salvage: cfg.Salvage,
		lane:    cfg.Lane,
	}
	if t.http == nil {
		// http.Client.Timeout caps the ENTIRE request including the streamed
		// body read — set just ABOVE the gateway's 60-min request timeout so a
		// genuinely-long turn is bounded by the GATEWAY (clean error) and this
		// is only a backstop against a hung connection (same rationale as the
		// legacy SDK client).
		t.http = &http.Client{Timeout: 62 * time.Minute}
	}
	return t
}

// SetRetryNotify wires a retry-notify callback after construction — the same
// seam the runtime uses on the legacy client to surface "⊙ retrying…".
func (t *Transport) SetRetryNotify(fn func(attempt int, err error, delay time.Duration)) {
	t.retryNotify = fn
}

// resolveModel finalizes the wire model for one request. The selection policy
// (cli/internal/llm) stamps a concrete label on every hosted call; an
// arbitrary endpoint substitutes its configured session model when the
// request didn't name one. A missing model is an actionable local error —
// nothing modelless ever leaves the machine.
func (t *Transport) resolveModel(body *ChatRequest) error {
	if body.Model != "" {
		return nil
	}
	if t.memcode {
		return errors.New("no model selected for this call — the selection policy did not run (this is a bug)")
	}
	if t.model == "" {
		return errors.New("no model selected for this endpoint — pick one with /model, or set MEMCODE_ENDPOINT_MODEL")
	}
	body.Model = t.model
	return nil
}

// Complete runs one non-streamed turn.
func (t *Transport) Complete(ctx context.Context, r wire.Request) (wire.Response, error) {
	r, err := t.composeLocal(r)
	if err != nil {
		return wire.Response{}, err
	}
	body, err := t.encodeBody(r, false)
	if err != nil {
		return wire.Response{}, err
	}
	if err := t.resolveModel(&body); err != nil {
		return wire.Response{}, err
	}
	for {
		payload, err := json.Marshal(body)
		if err != nil {
			return wire.Response{}, err
		}
		resp, err := t.do(ctx, payload, false)
		if err != nil {
			return wire.Response{}, err
		}
		raw, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			// A truncated 200 body (connection reset mid-read) surfaces its real
			// cause, not a vague decoding error.
			return wire.Response{}, fmt.Errorf("reading gateway response: %w", err)
		}
		if resp.StatusCode != http.StatusOK {
			if degradeReasoningForTools(resp.StatusCode, raw, &body) {
				continue // one retry with reasoning_effort:"none"
			}
			return wire.Response{}, t.mapError(resp.StatusCode, raw)
		}
		var out ChatResponse
		if err := json.Unmarshal(raw, &out); err != nil {
			return wire.Response{}, fmt.Errorf("decoding gateway response: %w", err)
		}
		dr, derr := decodeResponse(out, t.memcode)
		if derr != nil {
			return dr, derr
		}
		return t.finishResponse(dr, r, payload), nil
	}
}

// degradeReasoningForTools is a CAPABILITY DEGRADATION, not a vendor branch:
// OpenAI's chat/completions rejects function tools whenever reasoning is
// active for its reasoning models (which DEFAULT to reasoning when the field
// is omitted), directing callers to send reasoning_effort:"none". When an
// endpoint 400s a tool-carrying request with that complaint, retry once with
// the explicit "none" — the turn survives at the cost of thinking depth. The
// error-shape sniff mirrors isContextOverflow: keyed on the endpoint's own
// words, never on its hostname.
func degradeReasoningForTools(status int, raw []byte, body *ChatRequest) bool {
	if status != http.StatusBadRequest || len(body.Tools) == 0 || body.ReasoningEffort == "none" {
		return false
	}
	if !strings.Contains(strings.ToLower(string(raw)), "reasoning_effort") {
		return false
	}
	body.ReasoningEffort = "none"
	return true
}

// Stream runs one streamed turn, forwarding deltas to h and returning the
// assembled Response — the same contract as every provider's Stream.
func (t *Transport) Stream(ctx context.Context, r wire.Request, h wire.StreamHandler) (wire.Response, error) {
	r, err := t.composeLocal(r)
	if err != nil {
		return wire.Response{}, err
	}
	body, err := t.encodeBody(r, true)
	if err != nil {
		return wire.Response{}, err
	}
	if err := t.resolveModel(&body); err != nil {
		return wire.Response{}, err
	}
	var resp *http.Response
	for {
		payload, err := json.Marshal(body)
		if err != nil {
			return wire.Response{}, err
		}
		resp, err = t.do(ctx, payload, true)
		if err != nil {
			return wire.Response{}, err
		}
		if resp.StatusCode == http.StatusOK {
			break
		}
		raw, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			return wire.Response{}, fmt.Errorf("reading gateway error response: %w", readErr)
		}
		if degradeReasoningForTools(resp.StatusCode, raw, &body) {
			continue // one retry with reasoning_effort:"none"
		}
		return wire.Response{}, t.mapError(resp.StatusCode, raw)
	}
	defer resp.Body.Close()

	acc := newStreamAccum(t.memcode)
	sc := bufio.NewScanner(resp.Body)
	// A single data: line can carry a whole tool call (the gateway streams one
	// full delta per call — a large edit rides one line), so the line buffer
	// gets real headroom.
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data:")
		if !ok {
			continue // SSE comments / blank keep-alives
		}
		data = strings.TrimSpace(data)
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			return acc.response(), nil
		}
		// A mid-stream failure rides the standard error envelope on a data:
		// line (then [DONE]) — map it to the shared sentinels immediately.
		var env ErrorResponse
		if json.Unmarshal([]byte(data), &env) == nil && (env.Error.Message != "" || env.Error.Code != "") {
			return wire.Response{}, streamErr(env.Error)
		}
		var chunk ChatChunk
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue // tolerate non-chunk data events
		}
		acc.apply(chunk, h)
	}
	if err := sc.Err(); err != nil {
		if ctx.Err() != nil {
			return wire.Response{}, ctx.Err()
		}
		// A mid-stream read failure is transient transport — tag it so the
		// runtime retries the call rather than failing the turn.
		return wire.Response{}, fmt.Errorf("memcode api stream read: %v: %w", err, wire.ErrStreamIncomplete)
	}
	// The stream closed cleanly but never terminated with [DONE] (e.g. the
	// request cut at an infrastructure timeout) — also transient, also
	// retryable from the same history.
	return wire.Response{}, fmt.Errorf("memcode api stream ended without [DONE]: %w", wire.ErrStreamIncomplete)
}

// ── HTTP + retry ────────────────────────────────────────────────────────────
//
// Ported from the legacy SDK client (packages/sdk/go/client/client.go): on a
// retryable status (429, 500, 502, 503, 504) or a transient net error, back
// off with exponential + full jitter (honoring Retry-After) and re-issue the
// POST — the endpoint is stateless between calls. Other 4xx are permanent and
// never retried. Bounded so a genuinely-down endpoint fails cleanly.

const (
	maxRetries  = 3
	retryBaseMs = 1000  // base backoff: 1s
	retryFactor = 2     // exponential factor
	retryCapMs  = 10000 // backoff cap: 10s
)

// newRequest builds one attempt: body, auth, accept. Everything the backend
// needs rides the BODY (the model field carries the selection; `user` carries
// session affinity) — there are no memcode headers: the request sent to the
// gateway is byte-shape-identical to the one sent to Ollama, modulo base URL,
// key, and which concrete model id is in `model`.
func (t *Transport) newRequest(ctx context.Context, payload []byte, stream bool) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.base+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if t.token != "" {
		req.Header.Set("Authorization", "Bearer "+t.token)
	}
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	}
	return req, nil
}

// do issues the POST with the retry policy and returns a response with an OPEN
// body (any status) for the caller to consume; exhausted retries surface the
// mapped error directly.
func (t *Transport) do(ctx context.Context, payload []byte, stream bool) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		req, err := t.newRequest(ctx, payload, stream)
		if err != nil {
			return nil, err
		}
		resp, err := t.http.Do(req)
		if err != nil {
			if retryableNetErr(err) && attempt < maxRetries {
				if !t.sleep(ctx, attempt, err, backoffDelay(attempt)) {
					return nil, ctx.Err()
				}
				continue
			}
			return nil, fmt.Errorf("memcode api request: %w", err)
		}
		if !retryableStatus(resp.StatusCode) {
			return resp, nil
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if attempt < maxRetries {
			delay, ok := retryAfterDelay(resp)
			if !ok {
				delay = backoffDelay(attempt)
			}
			if !t.sleep(ctx, attempt, mapHTTPError(resp.StatusCode, raw), delay) {
				return nil, ctx.Err()
			}
			continue
		}
		return nil, mapHTTPError(resp.StatusCode, raw)
	}
}

// sleep waits out one backoff (notifying first), returning false on cancel.
func (t *Transport) sleep(ctx context.Context, attempt int, cause error, delay time.Duration) bool {
	if t.retryNotify != nil {
		t.retryNotify(attempt+1, cause, delay)
	}
	select {
	case <-ctx.Done():
		return false
	case <-time.After(delay):
		return true
	}
}

// retryableStatus reports whether an HTTP status is a transient failure worth
// retrying. 413 is NOT here — it may carry the context-overflow code, which
// the caller handles via compaction, not retry.
func retryableStatus(status int) bool {
	switch status {
	case http.StatusTooManyRequests, // 429
		http.StatusInternalServerError, // 500
		http.StatusBadGateway,          // 502
		http.StatusServiceUnavailable,  // 503
		http.StatusGatewayTimeout:      // 504
		return true
	}
	return false
}

// retryableNetErr reports whether a request error is transient transport (a
// reset, a DNS hiccup, a timeout) rather than a permanent config error.
func retryableNetErr(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr)
}

// retryAfterDelay parses the Retry-After header (seconds or HTTP-date),
// capped at retryCapMs. ok=false when absent or unparseable.
func retryAfterDelay(resp *http.Response) (time.Duration, bool) {
	v := resp.Header.Get("Retry-After")
	if v == "" {
		return 0, false
	}
	capped := func(d time.Duration) time.Duration {
		if d > time.Duration(retryCapMs)*time.Millisecond {
			return time.Duration(retryCapMs) * time.Millisecond
		}
		return d
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		return capped(time.Duration(secs) * time.Second), true
	}
	if at, err := http.ParseTime(v); err == nil {
		d := time.Until(at)
		if d < 0 {
			return 0, true
		}
		return capped(d), true
	}
	return 0, false
}

// backoffDelay computes exponential backoff with full jitter:
// rand(0, base·factor^attempt), capped — spread so concurrent retries don't
// synchronize.
func backoffDelay(attempt int) time.Duration {
	base := time.Duration(retryBaseMs) * time.Millisecond
	for i := 0; i < attempt; i++ {
		base *= time.Duration(retryFactor)
		if base > time.Duration(retryCapMs)*time.Millisecond {
			base = time.Duration(retryCapMs) * time.Millisecond
			break
		}
	}
	return time.Duration(rand.Int63n(int64(base)))
}
