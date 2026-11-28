package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/memcode-ai/memcode/internal/agent/secrets"
	"github.com/memcode-ai/memcode/internal/sandbox"

	"github.com/modelcontextprotocol/go-sdk/auth"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	listTimeout        = 15 * time.Second
	defaultCallTimeout = 120 * time.Second // per-tool-call ceiling unless a server sets its own
)

// Vars for tests.
var (
	// connectTimeout bounds the non-interactive handshake (npx download, cold HTTP server).
	connectTimeout = 20 * time.Second
	// authorizeTimeout is the budget for an interactive browser authorization during
	// connect: a person has to sign in and approve, which takes far longer than a
	// handshake. The connect clock is suspended while it runs and restarts after.
	authorizeTimeout = 5 * time.Minute
)

// Options configure a connection batch.
type Options struct {
	Version    string // memcode version reported to servers
	AllowOAuth bool   // attach the interactive OAuth flow to remote servers that need auth (interactive sessions only)
	// Root is the project the session runs in. When set, stdio servers run
	// inside memcode's OS shield (internal/sandbox): a third-party server can't
	// read memcode's credentials or rewrite its settings.
	Root string
}

// Tool is one discovered MCP tool, flattened for memcode's tool registry.
type Tool struct {
	Name        string         // namespaced for the model: mcp__<server>__<raw>
	Server      string         // declaring server name
	Raw         string         // original tool name on the server
	Description string         // server-provided description
	InputSchema map[string]any // JSON-schema object, ready for a tool definition
}

type serverConn struct {
	name    string
	session *mcpsdk.ClientSession
	cancel  context.CancelFunc // tears down this connection's context (and OAuth listener) on Close
	timeout time.Duration      // per-call ceiling for this server
}

type toolRef struct {
	conn *serverConn
	raw  string
}

// Resource is one MCP resource exposed by a server — external context (a doc, a schema,
// a runbook) the model can READ by uri. Namespaced like tools so servers can't collide.
type Resource struct {
	URI         string // the server's resource uri (what ReadResource takes)
	Server      string
	Name        string
	Description string
	MIMEType    string
}

// Prompt is one reusable MCP prompt template a server exposes. Name is namespaced
// (mcp__<server>__<raw>) for the catalog; the raw name is used with GetPrompt.
type Prompt struct {
	Name        string // namespaced
	Server      string
	Raw         string
	Description string
	Arguments   []PromptArg
}

// PromptArg describes one argument a prompt template accepts.
type PromptArg struct {
	Name        string
	Description string
	Required    bool
}

type resourceRef struct {
	conn *serverConn
	uri  string
}

type promptRef struct {
	conn *serverConn
	raw  string
}

// Manager owns the live connections to all configured MCP servers and the merged tool
// catalog. A nil *Manager is valid and behaves as "no MCP" — all methods are safe to call.
type Manager struct {
	conns     []*serverConn
	tools     []Tool
	byName    map[string]toolRef
	resources []Resource
	byURI     map[string]resourceRef
	prompts   []Prompt
	byPrompt  map[string]promptRef
	errs      []error // per-server connect/list failures (surfaced, not fatal)
}

// Connect connects to each given server (already resolved across scopes and filtered by
// approval policy — see Resolve and approvals.go), returning a Manager with the merged tool
// catalog. A server that fails to connect or list is skipped (recorded in Errors), never fatal —
// one broken server must not sink the session. Returns nil when the set is empty, so callers can
// treat "no MCP" as the zero case.
func Connect(ctx context.Context, servers map[string]ServerConfig, opts Options) *Manager {
	if len(servers) == 0 {
		return nil
	}
	m := &Manager{byName: map[string]toolRef{}, byURI: map[string]resourceRef{}, byPrompt: map[string]promptRef{}}
	m.Add(ctx, servers, opts)
	return m
}

// Add connects additional servers into an existing manager and merges their tools — used when a
// project server is approved mid-session. Safe on a nil receiver (returns it unchanged via the
// caller). Errors are appended to Errors(), never returned.
func (m *Manager) Add(ctx context.Context, servers map[string]ServerConfig, opts Options) {
	if m == nil {
		return
	}
	for _, name := range sortedKeys(servers) {
		conn, err := dial(ctx, name, servers[name], opts)
		if err != nil {
			m.errs = append(m.errs, fmt.Errorf("%s: %w", name, err))
			continue
		}
		m.conns = append(m.conns, conn)
		if err := m.listTools(ctx, conn); err != nil {
			m.errs = append(m.errs, fmt.Errorf("%s: list tools: %w", name, err))
		}
		// Resources and prompts are OPTIONAL MCP capabilities — a tools-only server
		// returns an error/nothing; that's not a failure, so only record real errors.
		m.listResources(ctx, conn)
		m.listPrompts(ctx, conn)
	}
}

// dial brings up one server connection, bounding the handshake with connectTimeout without
// killing a healthy session: the connection context is cancelled only on error/timeout/Close.
func dial(ctx context.Context, name string, sc ServerConfig, opts Options) (*serverConn, error) {
	transport, oauthStop, err := transportFor(sc, opts)
	if err != nil {
		return nil, err
	}
	var authEvents <-chan bool
	if st, ok := transport.(*mcpsdk.StreamableClientTransport); ok {
		if ia, ok := st.OAuthHandler.(*interactiveAuth); ok {
			authEvents = ia.events
		}
	}
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "memcode", Version: opts.Version}, nil)
	connCtx, cancel := context.WithCancel(ctx)
	ch := make(chan dialResult, 1)
	go func() {
		s, err := client.Connect(connCtx, transport, nil)
		ch <- dialResult{s, err}
	}()
	stop := func() {
		cancel()
		if oauthStop != nil {
			oauthStop()
		}
	}
	s, err := awaitConnect(ctx, ch, authEvents, stop)
	if err != nil {
		return nil, err
	}
	return &serverConn{name: name, session: s, cancel: stop, timeout: callTimeout(sc)}, nil
}

type dialResult struct {
	s   *mcpsdk.ClientSession
	err error
}

// awaitConnect waits for a Connect outcome. The handshake gets connectTimeout; while an
// interactive authorization is running (authEvents: true = started, false = finished) the
// clock switches to authorizeTimeout, then back to connectTimeout for the rest of the
// handshake. ctx cancels it at any point. On any failure stop abandons the dial.
func awaitConnect(ctx context.Context, ch <-chan dialResult, authEvents <-chan bool, stop func()) (*mcpsdk.ClientSession, error) {
	timer := time.NewTimer(connectTimeout)
	defer timer.Stop()
	authorizing := false
	for {
		select {
		case r := <-ch:
			if r.err != nil {
				stop()
				return nil, r.err
			}
			return r.s, nil
		case started := <-authEvents:
			authorizing = started
			if started {
				timer.Reset(authorizeTimeout)
			} else {
				timer.Reset(connectTimeout)
			}
		case <-timer.C:
			stop() // abandon the hung dial
			if authorizing {
				return nil, fmt.Errorf("authorization was not completed within %s — reconnect to try again", authorizeTimeout)
			}
			return nil, fmt.Errorf("connect timed out after %s", connectTimeout)
		case <-ctx.Done():
			stop()
			return nil, ctx.Err()
		}
	}
}

// interactiveAuth wraps the browser OAuth handler so the dial can tell a person-paced
// authorization (minutes) from a hung handshake (seconds), and bounds each authorization.
type interactiveAuth struct {
	auth.OAuthHandler
	events chan bool
}

func newInteractiveAuth(h auth.OAuthHandler) *interactiveAuth {
	return &interactiveAuth{OAuthHandler: h, events: make(chan bool, 8)}
}

func (a *interactiveAuth) signal(started bool) {
	select {
	case a.events <- started:
	default: // no dial is listening (mid-session re-auth): the request ctx bounds it
	}
}

// Authorize runs the browser flow under its own authorizeTimeout (and the request ctx).
func (a *interactiveAuth) Authorize(ctx context.Context, req *http.Request, resp *http.Response) error {
	a.signal(true)
	defer a.signal(false)
	actx, cancel := context.WithTimeout(ctx, authorizeTimeout)
	defer cancel()
	return a.OAuthHandler.Authorize(actx, req, resp)
}

// transportFor builds the SDK transport for a server: a subprocess for stdio, or the streamable
// HTTP / SSE transport for remote servers. Remote transports get a client that injects static +
// helper-minted headers; and, in interactive sessions, an OAuth handler when the server needs
// auth (no static auth header). Returns an optional cleanup for any OAuth listener.
func transportFor(sc ServerConfig, opts Options) (mcpsdk.Transport, func(), error) {
	switch sc.kind() {
	case "sse": // deprecated transport, still supported for older servers (no OAuth wiring)
		if sc.URL == "" {
			return nil, nil, fmt.Errorf("sse server has no url")
		}
		headers, err := resolveHeaders(sc)
		if err != nil {
			return nil, nil, err
		}
		return &mcpsdk.SSEClientTransport{Endpoint: sc.URL, HTTPClient: httpClient(headers, sc.URL)}, nil, nil
	case "http":
		if sc.URL == "" {
			return nil, nil, fmt.Errorf("http server has no url")
		}
		headers, err := resolveHeaders(sc)
		if err != nil {
			return nil, nil, err
		}
		hc := httpClient(headers, sc.URL)
		t := &mcpsdk.StreamableClientTransport{Endpoint: sc.URL, HTTPClient: hc}
		if wantOAuth(sc, opts, headers) {
			h, stop, herr := oauthHandler(hc)
			if herr != nil {
				return nil, nil, herr
			}
			t.OAuthHandler = newInteractiveAuth(h)
			return t, stop, nil
		}
		return t, nil, nil
	default: // stdio
		if sc.Command == "" {
			return nil, nil, fmt.Errorf("stdio server has no command")
		}
		cmd := exec.Command(sc.Command, sc.Args...)
		if opts.Root != "" {
			if line, ok := sandbox.WrapArgv(append([]string{sc.Command}, sc.Args...), sandbox.ShieldOnly(opts.Root)); ok {
				cmd = exec.Command("/bin/sh", "-c", line)
			}
		}
		cmd.Env = secrets.ChildEnv(nil) // a third-party server never gets memcode's own credentials
		for k, v := range sc.Env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		return &mcpsdk.CommandTransport{Command: cmd}, nil, nil
	}
}

// wantOAuth reports whether to attach the interactive OAuth flow: the session must allow it, the
// server must not opt out ("none"), and either it explicitly asks ("oauth") or it has no static
// Authorization header to authenticate with.
func wantOAuth(sc ServerConfig, opts Options, headers map[string]string) bool {
	if !opts.AllowOAuth || strings.EqualFold(sc.Auth, "none") {
		return false
	}
	if strings.EqualFold(sc.Auth, "oauth") {
		return true
	}
	return !hasAuthHeader(headers)
}

func hasAuthHeader(headers map[string]string) bool {
	for k := range headers {
		if strings.EqualFold(k, "Authorization") {
			return true
		}
	}
	return false
}

func callTimeout(sc ServerConfig) time.Duration {
	if sc.Timeout > 0 {
		return time.Duration(sc.Timeout) * time.Millisecond
	}
	return defaultCallTimeout
}

// listTools pulls a connection's tools into the merged catalog, namespacing each so names
// from different servers can't collide and the gate can recognize an MCP call by its prefix.
func (m *Manager) listTools(ctx context.Context, conn *serverConn) error {
	lctx, cancel := context.WithTimeout(ctx, listTimeout)
	defer cancel()
	res, err := conn.session.ListTools(lctx, nil)
	if err != nil {
		return err
	}
	for _, t := range res.Tools {
		full := ToolName(conn.name, t.Name)
		m.tools = append(m.tools, Tool{
			Name:        full,
			Server:      conn.name,
			Raw:         t.Name,
			Description: t.Description,
			InputSchema: schemaMap(t.InputSchema),
		})
		m.byName[full] = toolRef{conn: conn, raw: t.Name}
	}
	return nil
}

// listResources pulls a connection's resources into the merged catalog. Best-effort:
// a server without the resources capability returns an error, which we ignore (it's not
// a failure — resources are optional). Resources are keyed by their raw uri (globally
// unique enough in practice; the ref remembers which connection serves it).
func (m *Manager) listResources(ctx context.Context, conn *serverConn) {
	lctx, cancel := context.WithTimeout(ctx, listTimeout)
	defer cancel()
	res, err := conn.session.ListResources(lctx, nil)
	if err != nil || res == nil {
		return
	}
	for _, r := range res.Resources {
		m.resources = append(m.resources, Resource{
			URI: r.URI, Server: conn.name, Name: r.Name,
			Description: r.Description, MIMEType: r.MIMEType,
		})
		m.byURI[r.URI] = resourceRef{conn: conn, uri: r.URI}
	}
}

// listPrompts pulls a connection's prompt templates into the merged catalog. Best-effort,
// like listResources — prompts are an optional capability.
func (m *Manager) listPrompts(ctx context.Context, conn *serverConn) {
	lctx, cancel := context.WithTimeout(ctx, listTimeout)
	defer cancel()
	res, err := conn.session.ListPrompts(lctx, nil)
	if err != nil || res == nil {
		return
	}
	for _, p := range res.Prompts {
		full := ToolName(conn.name, p.Name)
		pr := Prompt{Name: full, Server: conn.name, Raw: p.Name, Description: p.Description}
		for _, a := range p.Arguments {
			pr.Arguments = append(pr.Arguments, PromptArg{Name: a.Name, Description: a.Description, Required: a.Required})
		}
		m.prompts = append(m.prompts, pr)
		m.byPrompt[full] = promptRef{conn: conn, raw: p.Name}
	}
}

// Resources returns the merged resource catalog across all connected servers.
func (m *Manager) Resources() []Resource {
	if m == nil {
		return nil
	}
	return m.resources
}

// ReadResource fetches a resource's contents by uri, flattened to text.
func (m *Manager) ReadResource(ctx context.Context, uri string) (string, error) {
	if m == nil {
		return "", fmt.Errorf("no MCP servers configured")
	}
	ref, ok := m.byURI[uri]
	if !ok {
		return "", fmt.Errorf("unknown MCP resource: %s", uri)
	}
	if ref.conn.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, ref.conn.timeout)
		defer cancel()
	}
	res, err := ref.conn.session.ReadResource(ctx, &mcpsdk.ReadResourceParams{URI: uri})
	if err != nil {
		return "", err
	}
	return flattenResource(res.Contents), nil
}

// Prompts returns the merged prompt catalog across all connected servers.
func (m *Manager) Prompts() []Prompt {
	if m == nil {
		return nil
	}
	return m.prompts
}

// GetPrompt renders a namespaced prompt template with args, returning the flattened
// message text the model can use.
func (m *Manager) GetPrompt(ctx context.Context, name string, args map[string]string) (string, error) {
	if m == nil {
		return "", fmt.Errorf("no MCP servers configured")
	}
	ref, ok := m.byPrompt[name]
	if !ok {
		return "", fmt.Errorf("unknown MCP prompt: %s", name)
	}
	if ref.conn.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, ref.conn.timeout)
		defer cancel()
	}
	res, err := ref.conn.session.GetPrompt(ctx, &mcpsdk.GetPromptParams{Name: ref.raw, Arguments: args})
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, msg := range res.Messages {
		if txt := flatten([]mcpsdk.Content{msg.Content}); txt != "" {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(txt)
		}
	}
	return b.String(), nil
}

// Call invokes a namespaced MCP tool and returns its flattened text, whether the tool
// reported an error, and any transport error.
func (m *Manager) Call(ctx context.Context, name string, args map[string]any) (string, bool, error) {
	if m == nil {
		return "", true, fmt.Errorf("no MCP servers configured")
	}
	ref, ok := m.byName[name]
	if !ok {
		return "", true, fmt.Errorf("unknown MCP tool: %s", name)
	}
	if ref.conn.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, ref.conn.timeout)
		defer cancel()
	}
	res, err := ref.conn.session.CallTool(ctx, &mcpsdk.CallToolParams{Name: ref.raw, Arguments: args})
	if err != nil {
		return "", true, err
	}
	return flatten(res.Content), res.IsError, nil
}

// Tools returns the merged, namespaced tool catalog across all connected servers.
func (m *Manager) Tools() []Tool {
	if m == nil {
		return nil
	}
	return m.tools
}

// Lookup returns the catalog entry for a namespaced tool name.
func (m *Manager) Lookup(name string) (Tool, bool) {
	if m == nil {
		return Tool{}, false
	}
	for _, t := range m.tools {
		if t.Name == name {
			return t, true
		}
	}
	return Tool{}, false
}

// Has reports whether name is a known MCP tool (used by the dispatcher to route a call).
func (m *Manager) Has(name string) bool {
	if m == nil {
		return false
	}
	_, ok := m.byName[name]
	return ok
}

// Errors returns the per-server connect/list failures gathered during Connect.
func (m *Manager) Errors() []error {
	if m == nil {
		return nil
	}
	return m.errs
}

// Close tears down every connection. Safe on a nil Manager and idempotent.
func (m *Manager) Close() {
	if m == nil {
		return
	}
	for _, c := range m.conns {
		if c.session != nil {
			_ = c.session.Close()
		}
		if c.cancel != nil {
			c.cancel()
		}
	}
	m.conns = nil
}

// maxToolNameLen caps a model-facing tool name. The gateway forwards tool names verbatim to
// whichever lane serves the turn, and the strictest lane (the OpenAI-compatible function-calling
// schema used by the default planner lane) rejects a function name longer than 64 chars or with
// characters outside [A-Za-z0-9_-] — and a single bad name fails the WHOLE request (every tool that
// turn), not just the offending one. So we always honour the strictest limit.
const maxToolNameLen = 64

// ToolName builds the model-facing name for a server's tool: mcp__<server>__<raw>. Both segments
// are sanitized to the function-name charset [A-Za-z0-9_] and the whole is capped at
// maxToolNameLen, so NO lane rejects the request over an odd or long server/tool name. The
// ORIGINAL raw name is preserved in Tool.Raw and used for the actual CallTool (see Call), so a
// sanitized/truncated model-facing name never changes which server tool runs. When sanitizing or
// truncating could make two distinct raw names collide, a short stable hash of the (server,raw)
// pair disambiguates — keeping byName keys unique. The clean, short common case is returned
// verbatim (Claude Code's scheme) so familiar names like mcp__supabase__execute_sql are unchanged.
func ToolName(server, raw string) string {
	s := sanitize(server)
	name := "mcp__" + s + "__" + raw
	if raw == sanitize(raw) && len(name) <= maxToolNameLen {
		return name
	}
	// Reserve room for a stable hash suffix derived from the originals, then fit the raw segment.
	sum := sha256.Sum256([]byte(server + "\x00" + raw))
	suffix := "_" + hex.EncodeToString(sum[:])[:6] // 7 chars, distinct per (server,raw)
	if len(s) > 20 {
		s = s[:20] // a pathologically long server name can't crowd out the raw + hash
	}
	prefix := "mcp__" + s + "__"
	budget := maxToolNameLen - len(prefix) - len(suffix)
	r := sanitize(raw)
	if budget < 0 {
		budget = 0
	}
	if len(r) > budget {
		r = r[:budget]
	}
	return prefix + r + suffix
}

func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

// schemaMap converts the SDK's typed JSON-Schema into a generic map for the tool registry,
// falling back to a minimal object schema when a tool declares none.
func schemaMap(schema any) map[string]any {
	if schema == nil {
		return map[string]any{"type": "object"}
	}
	b, err := json.Marshal(schema)
	if err != nil {
		return map[string]any{"type": "object"}
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil || out == nil {
		return map[string]any{"type": "object"}
	}
	if _, ok := out["type"]; !ok {
		out["type"] = "object"
	}
	return out
}

// flatten joins an MCP result's content blocks into one string. Text content is taken
// verbatim; any non-text block is noted so the model knows something was returned.
func flatten(content []mcpsdk.Content) string {
	var parts []string
	for _, c := range content {
		switch v := c.(type) {
		case *mcpsdk.TextContent:
			parts = append(parts, v.Text)
		default:
			parts = append(parts, "[non-text content]")
		}
	}
	return strings.Join(parts, "\n")
}

// flattenResource joins a resource read's contents to text (text parts inline; binary
// blobs noted by mime type rather than dumped).
func flattenResource(contents []*mcpsdk.ResourceContents) string {
	var parts []string
	for _, c := range contents {
		if c == nil {
			continue
		}
		if c.Text != "" {
			parts = append(parts, c.Text)
		} else if len(c.Blob) > 0 {
			mt := c.MIMEType
			if mt == "" {
				mt = "binary"
			}
			parts = append(parts, fmt.Sprintf("[%s resource, %d bytes]", mt, len(c.Blob)))
		}
	}
	return strings.Join(parts, "\n")
}

// httpClient returns an HTTP client that injects the configured headers (e.g. an
// Authorization bearer token) — the SDK's sanctioned auth hook for remote servers. Injection
// is scoped to the configured server's host ONLY: the same client also carries the SDK's
// OAuth metadata/registration/token calls (third-party authorization servers) and follows
// redirects, and a configured secret must never leak to any of those other hosts.
// Returns the default client when there are no headers.
func httpClient(headers map[string]string, serverURL string) *http.Client {
	// No Client.Timeout: streamable HTTP keeps a long-lived event stream open, and each
	// request is already bounded by its ctx (connect/list/call timeouts). The transport
	// bounds the phases that have no ctx-independent end: dialing and the TLS handshake.
	if len(headers) == 0 {
		return &http.Client{Transport: baseTransport}
	}
	host := ""
	if u, err := url.Parse(serverURL); err == nil {
		host = strings.ToLower(u.Host)
	}
	return &http.Client{Transport: &headerRoundTripper{base: baseTransport, headers: headers, host: host}}
}

// baseTransport is the default transport with bounded dial and TLS handshake.
var baseTransport http.RoundTripper = func() http.RoundTripper {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	t.TLSHandshakeTimeout = 15 * time.Second
	return t
}()

type headerRoundTripper struct {
	base    http.RoundTripper
	headers map[string]string
	host    string // the configured MCP server's host — the only host that gets the headers
}

func (h *headerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if h.host == "" || !strings.EqualFold(req.URL.Host, h.host) {
		return h.base.RoundTrip(req) // different host (auth server, redirect) — no injection
	}
	req = req.Clone(req.Context())
	for k, v := range h.headers {
		req.Header.Set(k, v)
	}
	return h.base.RoundTrip(req)
}

func sortedKeys(m map[string]ServerConfig) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
