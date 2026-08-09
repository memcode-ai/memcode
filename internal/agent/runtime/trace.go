package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/memcode-ai/memcode/internal/agent/tools"
)

// traceTool traces an artifact through memcode-owned transformations and reports
// per-stage size + preview so the agent can locate the first corruption before
// theorizing about causes (law #8: TRACE BEFORE THEORY). Diagnostic only: no fixing,
// no summarizing. `target` is a URL (fetch pipeline) or a file path. Alias: wiretap.
// Marker: ⏺ Trace(…).
func (s *Session) traceTool(ctx context.Context, input json.RawMessage) toolResult {
	var in tools.TraceInput
	if err := json.Unmarshal(input, &in); err != nil {
		return errResult(err.Error())
	}
	target := strings.TrimSpace(in.Target)
	if target == "" {
		return errResult("trace needs a `target` — a URL or a file path.")
	}
	s.toolLine(true, "Trace", target, "", false)
	if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") {
		return textResult(s.traceURL(ctx, target))
	}
	return textResult(s.traceFile(target))
}

const tracePreviewLen = 160 // chars of each stage's content shown as evidence

// traceURL traces the FETCH pipeline: raw GET → body2text → truncate.
func (s *Session) traceURL(ctx context.Context, url string) string {
	// SSRF guard: refuse the cloud metadata endpoint, exactly as fetch does. trace
	// legitimately targets local dev servers, so block only the metadata endpoints
	// (the credential-theft target), not all private hosts.
	if isMetadataURL(url) {
		return "trace: refused to fetch the cloud metadata endpoint"
	}
	cctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, url, nil)
	if err != nil {
		return "trace: " + err.Error()
	}
	req.Header.Set("User-Agent", "memcode/1.0 (+https://memcode.ai)")
	resp, err := (&http.Client{Timeout: fetchTimeout}).Do(req)
	if err != nil {
		return "trace: GET failed: " + err.Error()
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxFetchBytes))
	text := body2text(resp.Header.Get("Content-Type"), string(body))
	truncated := truncate(text, maxFetchText)
	ct := firstField(resp.Header.Get("Content-Type"))
	if ct == "" {
		ct = "unknown"
	}
	stages := []traceStage{
		{"fetch.raw", len(body), previewBytes(body)},
		{"extract.text", len(text), previewStr(text)},
		{"truncate.out", len(truncated), previewStr(truncated)},
	}
	header := fmt.Sprintf("trace %s  (%s, %s)", url, resp.Status, ct)
	if readErr != nil {
		header += "  [read error: " + clip(readErr.Error(), 60) + "]"
	}
	return formatTrace(header, stages)
}

// traceFile traces a local file: raw bytes → (if HTML) extracted text.
func (s *Session) traceFile(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return "trace: " + err.Error()
	}
	stages := []traceStage{{"file.raw", len(b), previewBytes(b)}}
	head := b
	if len(head) > 512 {
		head = head[:512]
	}
	if strings.Contains(strings.ToLower(path), ".htm") || strings.Contains(strings.ToLower(string(head)), "<html") {
		text := body2text("text/html", string(b))
		stages = append(stages, traceStage{"extract.text", len(text), previewStr(text)})
	}
	return formatTrace("trace "+path, stages)
}

// traceStage is one boundary the artifact crossed. n is BYTES (len) throughout, so
// stages are directly comparable; preview is the first bytes of that stage's content.
type traceStage struct {
	name    string
	n       int
	preview string
}

// formatTrace prints each stage's size + preview and flags the FIRST stage where a
// substantial input collapses — near-empty (out < 500) OR under 5% survived. Showing
// the preview at each stage is the point: it PROVES where the artifact changed.
func formatTrace(header string, stages []traceStage) string {
	var b strings.Builder
	b.WriteString(header + "\n")
	firstBad := ""
	for i, st := range stages {
		mark := ""
		if i > 0 {
			in, out := stages[i-1].n, st.n
			drop := 0.0
			if in > 0 {
				drop = 100 * float64(in-out) / float64(in)
			}
			status := "ok"
			if in > 5000 && (out < 500 || out*20 < in) { // catastrophic, or <5% survived
				status = "DROPPED"
				if firstBad == "" {
					firstBad = st.name
				}
			}
			mark = fmt.Sprintf("   %s %.1f%% loss", status, drop)
		}
		b.WriteString(fmt.Sprintf("  %-14s %9s%s\n", st.name, formatBytes(st.n), mark))
		if st.preview != "" {
			b.WriteString("    ↳ " + st.preview + "\n")
		}
	}
	if firstBad != "" {
		b.WriteString("  → first corruption: " + firstBad + "\n")
	} else {
		b.WriteString("  → data survives every stage (the bug is elsewhere)\n")
	}
	return b.String()
}

// previewBytes converts only the HEAD of a (possibly 512KB) byte slice — never the
// whole thing — into a compact one-line preview.
func previewBytes(b []byte) string {
	if len(b) > 4096 {
		b = b[:4096]
	}
	return previewStr(string(b))
}

func previewStr(s string) string {
	if len(s) > 4096 {
		s = s[:4096]
	}
	return clip(oneLine(s), tracePreviewLen)
}

// oneLine collapses all whitespace runs to single spaces (for a compact preview).
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// firstField returns the value before the first ';' or ' ' (e.g. a bare content-type).
func firstField(s string) string {
	if i := strings.IndexAny(s, "; "); i >= 0 {
		return s[:i]
	}
	return s
}
