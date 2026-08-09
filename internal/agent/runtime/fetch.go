package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"

	"github.com/memcode-ai/memcode/internal/agent/tools"
	"github.com/memcode-ai/memcode/internal/browserrender"
	"github.com/memcode-ai/memcode/internal/provider"
)

const (
	fetchTimeout  = 30 * time.Second
	maxFetchBytes = 512 * 1024 // cap the download
	maxFetchText  = 48 * 1024  // cap the EXTRACTED text for the model — an article, not 8KB
)

// reBlankLines collapses runs of blank lines in ALREADY-EXTRACTED plain text (not
// HTML — see body2text for why HTML is never touched with regexes).
var reBlankLines = regexp.MustCompile(`[ \t]*\n[ \t\n]*\n[ \t\n]*`)

// skipText: elements whose contents are never readable page text.
var skipText = map[atom.Atom]bool{
	atom.Script: true, atom.Style: true, atom.Head: true,
	atom.Noscript: true, atom.Template: true, atom.Svg: true,
}

// blockText: elements that should emit a line break after their text, for readability.
var blockText = map[atom.Atom]bool{
	atom.P: true, atom.Div: true, atom.Br: true, atom.Li: true, atom.Tr: true,
	atom.H1: true, atom.H2: true, atom.H3: true, atom.H4: true, atom.H5: true, atom.H6: true,
	atom.Section: true, atom.Article: true, atom.Header: true, atom.Footer: true,
	atom.Ul: true, atom.Ol: true, atom.Blockquote: true,
}

// fetchTool fetches a URL over HTTP(S) and returns its text (HTML stripped). It's a
// read-only network tool — like read_file for the web. Marker: ⏺ Fetch(<url>).
func (s *Session) fetchTool(ctx context.Context, input json.RawMessage) toolResult {
	var in tools.FetchInput
	if err := json.Unmarshal(input, &in); err != nil {
		return errResult(err.Error())
	}
	url := strings.TrimSpace(in.URL)
	if url == "" {
		return errResult("fetch needs a `url`.")
	}
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		url = "https://" + url
	}
	// Light guard: never hit the cloud metadata endpoint (credential-theft vector).
	if isMetadataURL(url) {
		s.toolLine(true, "Fetch", url, "blocked (metadata endpoint)", true)
		return errResult("refused to fetch the cloud metadata endpoint")
	}

	// FAST PATH: a raw GET. Great for raw files, JSON, APIs, plain text, and static
	// HTML — instant, no model round-trip.
	cctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, url, nil)
	if err != nil {
		return errResult("fetch failed: " + err.Error())
	}
	req.Header.Set("User-Agent", "memcode/1.0 (+https://memcode.ai)")
	req.Header.Set("Accept", "text/html,text/plain,application/json,*/*")

	resp, err := (&http.Client{Timeout: fetchTimeout}).Do(req)
	if err != nil {
		// Couldn't reach it directly — let the server-side fetcher try (public URLs).
		if content, ok := s.tryWebFetch(ctx, url); ok {
			return textResult(content)
		}
		s.toolLine(true, "Fetch", url, "failed", true)
		return errResult("fetch failed: " + err.Error())
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxFetchBytes))
	ctype := resp.Header.Get("Content-Type")
	text := body2text(ctype, string(body))

	// ESCALATE when the fast path can't give readable text.
	switch {
	case isPDF(ctype):
		// Binary — only the server-side fetcher extracts PDF text.
		if content, ok := s.tryWebFetch(ctx, url); ok {
			return textResult(content)
		}
	case isJSShell(text) || thinText(text, len(body)):
		// JS-rendered shell (framework markers, OR a big page that yielded almost no
		// readable text — the signature of client-side rendering the markers missed).
		// web_fetch is also no-JS, so a LOCAL browser render (executes the page's JS)
		// is the only thing that reads it — consent-gated. Fall back to web_fetch
		// (may get metadata) if no browser / declined.
		if content, ok := s.renderWithConsent(ctx, url); ok {
			return textResult(content)
		}
		if content, ok := s.tryWebFetch(ctx, url); ok {
			return textResult(content)
		}
	}

	s.toolLine(true, "Fetch", url, "", false)
	s.printf("%s\n", metaStyle.Render(fmt.Sprintf("  ⎿ %s · %s", resp.Status, formatBytes(len(body)))))
	if strings.TrimSpace(text) == "" {
		return textResult(fmt.Sprintf("fetched %s (%s) — no readable text content (try web_search for JS-rendered pages)", url, resp.Status))
	}
	return textResult(s.redactor.Redact(truncate(text, maxFetchText)))
}

// tryWebFetch uses the provider's server-side web_fetch (clean text/PDF extraction)
// for a PUBLIC url. Returns ok=false for local/private urls, providers without the
// capability, or any error/empty result — so the caller falls back to the raw text.
func (s *Session) tryWebFetch(ctx context.Context, url string) (string, bool) {
	wf, ok := s.prov.(provider.WebFetcher)
	if !ok || isLocalURL(url) {
		return "", false
	}
	content, err := wf.WebFetch(ctx, url)
	if err != nil || strings.TrimSpace(content) == "" {
		return "", false
	}
	s.toolLine(true, "Fetch", url, "", false)
	s.printf("%s\n", metaStyle.Render("  ⎿ read via web_fetch"))
	return s.redactor.Redact(truncate(content, maxFetchText)), true
}

// renderWithConsent renders a JS page with a LOCAL browser (executing its JS),
// gated behind explicit user consent (asked once per session). Returns ok=false if
// no browser is available, the user declines, or rendering fails.
func (s *Session) renderWithConsent(ctx context.Context, url string) (string, bool) {
	if s.ask == nil || !browserrender.Available() {
		return "", false
	}
	if !s.browserRenderOK {
		resp := s.ask(ctx, AskRequest{
			Question: "This page is JavaScript-rendered; raw fetch and web_fetch can't read it. Render it with your local Chrome? This executes the page's JavaScript locally — only for pages you trust.",
			Options:  []AskOption{{Label: "Render it"}, {Label: "Skip"}},
		})
		a := strings.ToLower(strings.TrimSpace(resp.Answer))
		if !(strings.HasPrefix(a, "render") || strings.HasPrefix(a, "always") || a == "yes" || a == "1") {
			return "", false
		}
		s.browserRenderOK = true
	}
	s.printf("%s\n", metaStyle.Render("  ⎿ rendering with local browser…"))
	rendered, err := browserrender.Render(ctx, url)
	if err != nil {
		s.printf("%s\n", metaStyle.Render("  ⎿ render failed: "+clip(err.Error(), 60)))
		return "", false
	}
	text := body2text("text/html", rendered)
	if len(strings.TrimSpace(text)) < 500 {
		// The browser ran but the page STILL yielded almost no readable text — it's
		// bot-blocked, login-walled, or genuinely empty. Be honest; don't pass the
		// title off as the article. Caller falls through to the clear fail message.
		s.printf("%s\n", metaStyle.Render("  ⎿ rendered, but the page returned no readable content (likely bot-blocked or login-walled)"))
		return "", false
	}
	s.toolLine(true, "Fetch", url, "", false)
	s.printf("%s\n", metaStyle.Render("  ⎿ rendered via local browser"))
	return s.redactor.Redact(truncate(text, maxFetchText)), true
}

func isPDF(contentType string) bool { return strings.Contains(strings.ToLower(contentType), "pdf") }

// isJSShell reports that stripped HTML is a JS-rendered shell (hydration data, not
// readable prose) — the content lives in client-side JS we can't execute here.
func isJSShell(stripped string) bool {
	return strings.Contains(stripped, "self.__next_f") ||
		strings.Contains(stripped, "__NEXT_DATA__") ||
		strings.Contains(stripped, "window.__NUXT__") ||
		strings.Contains(stripped, "__remixContext")
}

// thinText reports a page that returned substantial HTML but almost no readable
// text — the framework-agnostic signature of client-side rendering (the content
// lives in scripts/the DOM, hydrated by JS). Conservative thresholds so a genuinely
// short static page doesn't trip it.
func thinText(text string, bodyLen int) bool {
	return bodyLen > 10000 && len(strings.TrimSpace(text)) < 500
}

// isMetadataURL reports whether a URL targets a cloud instance-metadata endpoint — the
// classic SSRF credential-theft target (AWS/GCP 169.254.169.254, GCP metadata.google.internal,
// ECS 169.254.170.2, IMDSv6 fd00:ec2::254). Refused on every server-side fetch/trace path.
// Substring match (like the original inline guard): over-blocking a path that merely contains
// the string is astronomically rare and harmless; a parse miss would be a credential leak.
func isMetadataURL(url string) bool {
	u := strings.ToLower(url)
	return strings.Contains(u, "169.254.169.254") ||
		strings.Contains(u, "metadata.google.internal") ||
		strings.Contains(u, "169.254.170.2") ||
		strings.Contains(u, "fd00:ec2::254")
}

// isLocalURL reports whether a URL points at the local machine / private network,
// which the provider's server-side fetch can't reach — those use the raw GET path.
func isLocalURL(url string) bool {
	h := url
	if i := strings.Index(h, "://"); i >= 0 {
		h = h[i+3:]
	}
	if i := strings.IndexAny(h, "/?#"); i >= 0 {
		h = h[:i]
	}
	if i := strings.LastIndex(h, ":"); i >= 0 { // strip :port
		h = h[:i]
	}
	h = strings.ToLower(strings.Trim(h, "[]"))
	if h == "localhost" || h == "127.0.0.1" || h == "::1" || h == "0.0.0.0" || strings.HasSuffix(h, ".local") {
		return true
	}
	return strings.HasPrefix(h, "10.") || strings.HasPrefix(h, "192.168.") ||
		strings.HasPrefix(h, "169.254.") || strings.HasPrefix(h, "172.16.") ||
		strings.HasPrefix(h, "172.17.") || strings.HasPrefix(h, "172.18.") ||
		strings.HasPrefix(h, "172.19.") || strings.HasPrefix(h, "172.2") ||
		strings.HasPrefix(h, "172.30.") || strings.HasPrefix(h, "172.31.")
}

// body2text returns readable text. HTML is parsed with a REAL HTML parser
// (golang.org/x/net/html) and its visible text walked out of the DOM — NEVER with
// regexes. Regex HTML "stripping" mis-binds on real markup: the prior version ate a
// 235KB server-rendered article down to its 33-char <title>. Non-HTML (JSON, plain
// text, raw files) passes through unchanged.
func body2text(contentType, body string) string {
	isHTML := strings.Contains(strings.ToLower(contentType), "html") ||
		strings.Contains(body, "<html") || strings.Contains(body, "<!DOCTYPE") || strings.Contains(body, "<!doctype")
	if !isHTML {
		return strings.TrimSpace(body)
	}
	doc, err := html.Parse(strings.NewReader(body))
	if err != nil {
		return strings.TrimSpace(body) // never lose content to a parse hiccup
	}
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && skipText[n.DataAtom] {
			return
		}
		if n.Type == html.TextNode {
			if t := strings.TrimSpace(n.Data); t != "" {
				b.WriteString(t)
				b.WriteByte(' ')
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
		if n.Type == html.ElementNode && blockText[n.DataAtom] {
			b.WriteByte('\n')
		}
	}
	walk(doc)
	return strings.TrimSpace(reBlankLines.ReplaceAllString(b.String(), "\n\n"))
}

// formatBytes renders a byte count the way a human reads one: 512 B, 236KB,
// 1.4MB (decimal units — matches how people quote page sizes).
func formatBytes(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fMB", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.0fKB", float64(n)/1_000)
	default:
		return fmt.Sprintf("%d B", n)
	}
}
