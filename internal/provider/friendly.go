package provider

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/memcode-ai/memcode/internal/providers/provcore"
)

// friendlyMax bounds a rendered error. Generous on purpose: the provider's own
// message is the useful part, and a 120-byte cut used to drop exactly it.
const friendlyMax = 400

// FriendlyError renders a model-call failure for a person: a plain lead
// keyed on the HTTP status, then the provider's own message pulled out of the
// JSON body ("Rate limited by the provider (HTTP 429): Number of request
// tokens has exceeded your per-minute rate limit") instead of the SDK's raw
// `POST "https://…": 429 Too Many Requests {"type":"error",…}` dump. Errors
// that already speak plainly (ErrNoLane, ErrLaneExhausted, sentinels) pass
// through. Nothing parseable ⇒ the raw text, whitespace-collapsed and bounded.
func FriendlyError(err error) string {
	if err == nil {
		return ""
	}
	var exh *ErrLaneExhausted
	if errors.As(err, &exh) {
		msg := exh.Error()
		if exh.Err != nil {
			if d := upstreamMessage(exh.Err.Error()); d != "" {
				msg += ": " + d
			}
		}
		return bound(msg)
	}
	var noLane *ErrNoLane
	if errors.As(err, &noLane) {
		return noLane.Error()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "The request timed out before the provider answered — try again"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "The connection to the provider timed out — check your network and try again"
	}

	raw := err.Error()
	status, _, ok := provcore.APIErrorInfo(err)
	if !ok {
		status = sniffStatus(raw)
	}
	detail := upstreamMessage(raw)
	lead := statusLead(status)
	switch {
	case lead != "" && detail != "":
		return bound(lead + " (HTTP " + strconv.Itoa(status) + "): " + detail)
	case lead != "":
		return bound(lead + " (HTTP " + strconv.Itoa(status) + ")")
	case detail != "":
		return bound(capitalize(detail))
	}
	if strings.Contains(raw, "connection refused") || strings.Contains(raw, "no such host") {
		return bound("Couldn't reach the provider — check your network or endpoint URL (" + oneLine(raw) + ")")
	}
	return bound(oneLine(raw))
}

// statusRe finds an HTTP status in SDK/transport error text: the SDKs'
// `POST "https://…": 429 Too Many Requests` (a reason phrase, Request-ID, or
// body must follow — 529 has no Go status text — so a JSON number like
// `"input": 500` never reads as a status), `http 503`,
// `status 401`, `status code: 400`.
var statusRe = regexp.MustCompile(`(?:":\s*([1-5]\d\d) [A-Z( {]|(?i:\bhttp\s+|^error\s+|\bstatus(?:\s*code)?\s*[:=]?\s*)([1-5]\d\d)\b)`)

func sniffStatus(msg string) int {
	m := statusRe.FindStringSubmatch(msg)
	if m == nil {
		return 0
	}
	code := m[1]
	if code == "" {
		code = m[2]
	}
	n, _ := strconv.Atoi(code)
	return n
}

func statusLead(status int) string {
	switch {
	case status == 400:
		return "The provider rejected the request"
	case status == 401:
		return "The provider rejected the credentials"
	case status == 402:
		return "The provider account is out of credit"
	case status == 403:
		return "The provider denied access"
	case status == 404:
		return "The model or endpoint wasn't found"
	case status == 408 || status == 504:
		return "The provider timed out — try again"
	case status == 413:
		return "The request is too large for this model"
	case status == 422:
		return "The provider couldn't process the request"
	case status == 429:
		return "Rate limited by the provider"
	case status == 529:
		return "The provider is overloaded — try again shortly"
	case status >= 500 && status <= 599:
		return "The provider had a server error — try again shortly"
	case status >= 400:
		return "The provider returned an error"
	}
	return ""
}

// geminiMsgRe matches the genai SDK's non-JSON shape:
// "Error 429, Message: Resource has been exhausted…, Status: RESOURCE_EXHAUSTED, Details: …".
var geminiMsgRe = regexp.MustCompile(`Message: (.+?), Status: `)

// upstreamMessage pulls the human message out of the first JSON object in an
// error string — the shapes Anthropic, OpenAI, Gemini, and compat servers use:
// {"error":{"message":…}}, {"message":…}, {"error":"…"}, {"detail":…},
// {"error_description":…}, {"errors":[{"message":…}]}. "" when none parses.
func upstreamMessage(s string) string {
	if m := geminiMsgRe.FindStringSubmatch(s); m != nil {
		return oneLine(m[1])
	}
	for i := strings.IndexByte(s, '{'); i >= 0; {
		var v any
		if json.NewDecoder(strings.NewReader(s[i:])).Decode(&v) == nil {
			if m := messageIn(v, 0); m != "" {
				return oneLine(m)
			}
		}
		next := strings.IndexByte(s[i+1:], '{')
		if next < 0 {
			break
		}
		i += 1 + next
	}
	return ""
}

func messageIn(v any, depth int) string {
	if depth > 4 {
		return ""
	}
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case []any:
		for _, e := range t {
			if m := messageIn(e, depth+1); m != "" {
				return m
			}
		}
	case map[string]any:
		for _, k := range []string{"error", "message", "detail", "error_description", "errors", "msg"} {
			if e, ok := t[k]; ok {
				if m := messageIn(e, depth+1); m != "" {
					return m
				}
			}
		}
	}
	return ""
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func bound(s string) string {
	r := []rune(s)
	if len(r) <= friendlyMax {
		return s
	}
	return strings.TrimRight(string(r[:friendlyMax]), " ") + "…"
}

func capitalize(s string) string {
	r := []rune(s)
	if len(r) == 0 {
		return s
	}
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}
