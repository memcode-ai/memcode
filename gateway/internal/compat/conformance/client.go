package conformance

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/memcode-ai/memcode/gateway/internal/compat"
)

// client is a deliberately minimal OpenAI-compat client: raw net/http + the
// shared compat wire types. It has NO per-vendor branches — that is the point
// of the contract (one transport, any base URL).
type client struct {
	base  string // e.g. https://api.openai.com/v1 (no trailing slash)
	key   string
	model string
	http  *http.Client
}

func newClient(base, key, model string) *client {
	return &client{
		base:  strings.TrimRight(base, "/"),
		key:   key,
		model: model,
		http:  &http.Client{Timeout: 300 * time.Second},
	}
}

func (c *client) newRequest(ctx context.Context, method, path string, body any) (*http.Request, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.key != "" {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	return req, nil
}

// chat POSTs a non-streamed completion. Returns the HTTP status, the parsed
// response (when the body parses), and the raw body for diagnostics.
func (c *client) chat(ctx context.Context, req compat.ChatRequest) (int, compat.ChatResponse, string, error) {
	var out compat.ChatResponse
	hr, err := c.newRequest(ctx, http.MethodPost, "/chat/completions", req)
	if err != nil {
		return 0, out, "", err
	}
	resp, err := c.http.Do(hr)
	if err != nil {
		return 0, out, "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return resp.StatusCode, out, "", err
	}
	_ = json.Unmarshal(raw, &out) // best-effort: error bodies aren't completions
	return resp.StatusCode, out, string(raw), nil
}

// streamResult is one consumed SSE stream.
type streamResult struct {
	status  int
	chunks  []compat.ChatChunk
	sawDone bool
	raw     string
}

// content re-assembles the streamed text.
func (s *streamResult) content() string {
	var b strings.Builder
	for _, c := range s.chunks {
		for _, ch := range c.Choices {
			if ch.Delta.Content != nil {
				b.WriteString(*ch.Delta.Content)
			}
		}
	}
	return b.String()
}

// toolCall accumulates streamed tool-call deltas by index, the way a real
// client does, and returns the first assembled call.
func (s *streamResult) toolCall() (name, args string, sawDelta bool) {
	names := map[int]*strings.Builder{}
	argsAcc := map[int]*strings.Builder{}
	for _, c := range s.chunks {
		for _, ch := range c.Choices {
			for _, td := range ch.Delta.ToolCalls {
				sawDelta = true
				if names[td.Index] == nil {
					names[td.Index] = &strings.Builder{}
					argsAcc[td.Index] = &strings.Builder{}
				}
				if td.Function != nil {
					names[td.Index].WriteString(td.Function.Name)
					argsAcc[td.Index].WriteString(td.Function.Arguments)
				}
			}
		}
	}
	if n, ok := names[0]; ok {
		return n.String(), argsAcc[0].String(), sawDelta
	}
	return "", "", sawDelta
}

// usage returns the last usage object seen on the stream (endpoints differ on
// where they attach it).
func (s *streamResult) usage() *compat.Usage {
	var u *compat.Usage
	for _, c := range s.chunks {
		if c.Usage != nil {
			u = c.Usage
		}
	}
	return u
}

// stream POSTs a streamed completion and consumes the SSE body.
func (c *client) stream(ctx context.Context, req compat.ChatRequest) (*streamResult, error) {
	req.Stream = true
	hr, err := c.newRequest(ctx, http.MethodPost, "/chat/completions", req)
	if err != nil {
		return nil, err
	}
	hr.Header.Set("Accept", "text/event-stream")
	resp, err := c.http.Do(hr)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out := &streamResult{status: resp.StatusCode}
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		out.raw = string(raw)
		return out, nil
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
	var rawAll strings.Builder
	for sc.Scan() {
		line := sc.Text()
		rawAll.WriteString(line + "\n")
		data, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue // SSE comments / event: lines / blanks
		}
		data = strings.TrimSpace(data)
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			out.sawDone = true
			continue
		}
		var chunk compat.ChatChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue // tolerate non-chunk data events (some endpoints send errors/pings)
		}
		out.chunks = append(out.chunks, chunk)
	}
	out.raw = rawAll.String()
	return out, sc.Err()
}

// models GETs {base}/models.
func (c *client) models(ctx context.Context) (int, compat.ModelList, error) {
	var list compat.ModelList
	hr, err := c.newRequest(ctx, http.MethodGet, "/models", nil)
	if err != nil {
		return 0, list, err
	}
	resp, err := c.http.Do(hr)
	if err != nil {
		return 0, list, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return resp.StatusCode, list, err
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return resp.StatusCode, list, fmt.Errorf("parsing models: %w (%.200s)", err, raw)
	}
	return resp.StatusCode, list, nil
}

// ── probe payloads ──────────────────────────────────────────────────────────

// redPNGDataURL renders a small solid-red PNG at runtime (stdlib image/png —
// no fixture bytes to rot) as a data: URL for the vision probe.
func redPNGDataURL() string {
	img := image.NewRGBA(image.Rect(0, 0, 24, 24))
	for y := 0; y < 24; y++ {
		for x := 0; x < 24; x++ {
			img.Set(x, y, color.RGBA{R: 255, A: 255})
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
}

// minimalPDF builds a small VALID single-page PDF (correct xref offsets, one
// Helvetica text line) so the file-part probe exercises a document a real PDF
// pipeline can parse — a broken fixture would report false "no"s.
func minimalPDF(text string) []byte {
	var buf bytes.Buffer
	offsets := make([]int, 0, 6)
	obj := func(body string) {
		offsets = append(offsets, buf.Len())
		buf.WriteString(body)
	}
	buf.WriteString("%PDF-1.4\n")
	obj("1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n")
	obj("2 0 obj\n<< /Type /Pages /Kids [3 0 R] /Count 1 >>\nendobj\n")
	obj("3 0 obj\n<< /Type /Page /Parent 2 0 R /MediaBox [0 0 400 144] /Resources << /Font << /F1 5 0 R >> >> /Contents 4 0 R >>\nendobj\n")
	stream := fmt.Sprintf("BT /F1 14 Tf 24 100 Td (%s) Tj ET\n", text)
	obj(fmt.Sprintf("4 0 obj\n<< /Length %d >>\nstream\n%sendstream\nendobj\n", len(stream), stream))
	obj("5 0 obj\n<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>\nendobj\n")
	xrefAt := buf.Len()
	buf.WriteString(fmt.Sprintf("xref\n0 %d\n0000000000 65535 f \n", len(offsets)+1))
	for _, off := range offsets {
		buf.WriteString(fmt.Sprintf("%010d 00000 n \n", off))
	}
	buf.WriteString(fmt.Sprintf("trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets)+1, xrefAt))
	return buf.Bytes()
}

func pdfDataURL(text string) string {
	return "data:application/pdf;base64," + base64.StdEncoding.EncodeToString(minimalPDF(text))
}

// ── the capability matrix ───────────────────────────────────────────────────

type row struct {
	tier string // "HARD REQUIRED" | "OPTIONAL / PREFERRED" | "OPTIONAL CAPABILITIES"
	name string
	ok   bool
	note string
}

type matrix struct {
	endpoint, model string
	rows            []row
}

func (m *matrix) add(tier, name string, ok bool, note string) {
	m.rows = append(m.rows, row{tier: tier, name: name, ok: ok, note: note})
}

func (m *matrix) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "── conformance capability matrix ──\n")
	fmt.Fprintf(&b, "endpoint: %s\nmodel:    %s\n", m.endpoint, m.model)
	last := ""
	for _, r := range m.rows {
		if r.tier != last {
			fmt.Fprintf(&b, "%s\n", r.tier)
			last = r.tier
		}
		status := map[bool]string{true: "PASS", false: "FAIL"}[r.ok]
		if r.tier != tierHard {
			status = map[bool]string{true: "yes", false: "no"}[r.ok]
		}
		name := r.name + " " + strings.Repeat(".", max(2, 30-len(r.name)))
		fmt.Fprintf(&b, "  %s %s", name, status)
		if r.note != "" {
			fmt.Fprintf(&b, "  (%s)", r.note)
		}
		fmt.Fprintf(&b, "\n")
	}
	return b.String()
}

const (
	tierHard = "HARD REQUIRED"
	tierPref = "OPTIONAL / PREFERRED"
	tierCap  = "OPTIONAL CAPABILITIES"
)
