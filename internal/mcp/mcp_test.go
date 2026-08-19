package mcp

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestExpand(t *testing.T) {
	t.Setenv("MCP_TEST_TOK", "secret")
	cases := map[string]string{
		"Bearer ${MCP_TEST_TOK}":   "Bearer secret",
		"${MCP_MISSING:-fallback}": "fallback",
		"${MCP_TEST_TOK:-unused}":  "secret",
		"${MCP_MISSING}":           "",
		"no vars here":             "no vars here",
	}
	for in, want := range cases {
		if got := expand(in); got != want {
			t.Errorf("expand(%q) = %q; want %q", in, got, want)
		}
	}
}

func TestKind(t *testing.T) {
	cases := []struct {
		sc   ServerConfig
		want string
	}{
		{ServerConfig{URL: "https://x/mcp"}, "http"},
		{ServerConfig{Command: "npx"}, "stdio"},
		{ServerConfig{Type: "http"}, "http"},
		{ServerConfig{Type: "streamable-http", URL: "https://x"}, "http"}, // spec alias for http
		{ServerConfig{Type: "sse", URL: "https://x"}, "sse"},
		{ServerConfig{Type: "stdio", Command: "x"}, "stdio"},
	}
	for _, c := range cases {
		if got := c.sc.kind(); got != c.want {
			t.Errorf("%+v.kind() = %q; want %q", c.sc, got, c.want)
		}
	}
}

func TestResolveAndExpand(t *testing.T) {
	t.Setenv("MCP_TEST_TOK", "tok123")
	dir := t.TempDir()
	cfg := `{"mcpServers":{"supabase":{"type":"http","url":"https://mcp.supabase.com/mcp","headers":{"Authorization":"Bearer ${MCP_TEST_TOK}"}}}}`
	if err := os.WriteFile(filepath.Join(dir, ".mcp.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	servers := Resolve(dir)
	if len(servers) != 1 {
		t.Fatalf("expected 1 server, got %+v", servers)
	}
	sb := servers[0]
	if sb.Scope != ScopeProject {
		t.Errorf("scope = %q, want project", sb.Scope)
	}
	if sb.Config.URL != "https://mcp.supabase.com/mcp" {
		t.Errorf("url = %q", sb.Config.URL)
	}
	// Resolve returns the RAW entry — secrets stay ${VAR} references until connect time,
	// so approvals hash the committed config, never a resolved secret.
	if sb.Config.Headers["Authorization"] != "Bearer ${MCP_TEST_TOK}" {
		t.Errorf("Resolve must stay raw: %q", sb.Config.Headers["Authorization"])
	}
	if got := ExpandServer(sb.Config).Headers["Authorization"]; got != "Bearer tok123" {
		t.Errorf("header not expanded at connect time: %q", got)
	}
}

// TestConfigHashIgnoresEnv pins the ConfigHash contract: approvals key to the RAW config, so
// an environment change never invalidates them, while a raw-config edit always does.
func TestConfigHashIgnoresEnv(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("MCP_TEST_TOK", "a")
	root := t.TempDir()
	cfg := `{"mcpServers":{"x":{"type":"http","url":"https://x/mcp","headers":{"Authorization":"Bearer ${MCP_TEST_TOK}"}}}}`
	if err := os.WriteFile(filepath.Join(root, ".mcp.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	sc := Resolve(root)[0].Config
	if err := SaveApproval(root, "x", sc, Approved); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MCP_TEST_TOK", "b") // env change must not touch the approval
	sc2 := Resolve(root)[0].Config
	if LoadApprovals(root).Status("x", sc2) != Approved {
		t.Error("env change must not invalidate an approval")
	}
	// A raw-config edit does invalidate.
	edited := `{"mcpServers":{"x":{"type":"http","url":"https://x/mcp/v2","headers":{"Authorization":"Bearer ${MCP_TEST_TOK}"}}}}`
	if err := os.WriteFile(filepath.Join(root, ".mcp.json"), []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	if LoadApprovals(root).Status("x", Resolve(root)[0].Config) != "" {
		t.Error("raw config edit must reset the approval to pending")
	}
}

func TestResolveMissing(t *testing.T) {
	if got := Resolve(t.TempDir()); len(got) != 0 {
		t.Errorf("expected no servers, got %+v", got)
	}
}

func TestAddRemoveAndPrecedence(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", t.TempDir()) // isolate the user store

	// User-scope and project-scope servers with the SAME name: local/project beats user.
	if err := AddServer(root, ScopeUser, "db", ServerConfig{Type: "http", URL: "https://user/mcp"}); err != nil {
		t.Fatal(err)
	}
	if err := AddServer(root, ScopeProject, "db", ServerConfig{Type: "http", URL: "https://project/mcp"}); err != nil {
		t.Fatal(err)
	}
	servers := Resolve(root)
	if len(servers) != 1 {
		t.Fatalf("expected 1 merged server, got %+v", servers)
	}
	if servers[0].Scope != ScopeProject || servers[0].Config.URL != "https://project/mcp" {
		t.Errorf("project should win over user: got %+v", servers[0])
	}

	// Remove the project one → the user-scoped server becomes visible.
	ok, err := RemoveServer(root, ScopeProject, "db")
	if err != nil || !ok {
		t.Fatalf("remove project: ok=%v err=%v", ok, err)
	}
	servers = Resolve(root)
	if len(servers) != 1 || servers[0].Scope != ScopeUser {
		t.Fatalf("expected user server to surface, got %+v", servers)
	}
}

func TestApprovals(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // approvals live in the user-level store
	root := t.TempDir()
	sc := ServerConfig{Type: "http", URL: "https://x/mcp"}
	a := LoadApprovals(root)
	if a.Status("x", sc) != "" {
		t.Errorf("fresh server should be pending")
	}
	if err := SaveApproval(root, "x", sc, Approved); err != nil {
		t.Fatal(err)
	}
	if LoadApprovals(root).Status("x", sc) != Approved {
		t.Errorf("should be approved")
	}
	// Changing the config invalidates the approval (re-prompt).
	changed := ServerConfig{Type: "http", URL: "https://x/mcp/v2"}
	if LoadApprovals(root).Status("x", changed) != "" {
		t.Errorf("config change should reset approval to pending")
	}
	// Reset clears everything.
	if err := ResetApprovals(root); err != nil {
		t.Fatal(err)
	}
	if LoadApprovals(root).Status("x", sc) != "" {
		t.Errorf("reset should clear approval")
	}
}

func TestCallGrants(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // approvals live in the user-level store
	root := t.TempDir()
	sc := ServerConfig{Type: "http", URL: "https://x/mcp"}
	if LoadApprovals(root).CallAllowed("x", sc, "get_issue") {
		t.Errorf("no grant yet — nothing should be allowed")
	}
	// "Execute and remember" one tool — works for an unlisted (local/user) server too.
	if err := RememberCalls(root, "x", sc, "get_issue"); err != nil {
		t.Fatal(err)
	}
	a := LoadApprovals(root)
	if !a.CallAllowed("x", sc, "get_issue") {
		t.Errorf("remembered tool should be allowed")
	}
	if a.CallAllowed("x", sc, "delete_repo") {
		t.Errorf("only the remembered tool is granted, not the server")
	}
	// "Don't ask again for <server>" covers everything.
	if err := RememberCalls(root, "x", sc, ""); err != nil {
		t.Fatal(err)
	}
	if !LoadApprovals(root).CallAllowed("x", sc, "delete_repo") {
		t.Errorf("server-wide grant should cover every tool")
	}
	// A config edit kills grants exactly like it kills connect trust.
	changed := ServerConfig{Type: "http", URL: "https://x/mcp/v2"}
	if LoadApprovals(root).CallAllowed("x", changed, "get_issue") {
		t.Errorf("config change must reset invocation grants")
	}
	// Same-config re-approve keeps grants; rejection drops them.
	if err := SaveApproval(root, "x", sc, Approved); err != nil {
		t.Fatal(err)
	}
	if !LoadApprovals(root).CallAllowed("x", sc, "get_issue") {
		t.Errorf("re-approving the same config must keep grants")
	}
	if err := SaveApproval(root, "x", sc, Rejected); err != nil {
		t.Fatal(err)
	}
	if LoadApprovals(root).CallAllowed("x", sc, "get_issue") {
		t.Errorf("rejection must drop grants")
	}
}

func TestParseHelperHeaders(t *testing.T) {
	// JSON object form.
	j := parseHelperHeaders([]byte(`{"Authorization":"Bearer abc","X-Org":"acme"}`))
	if j["Authorization"] != "Bearer abc" || j["X-Org"] != "acme" {
		t.Errorf("json form: %+v", j)
	}
	// "Name: value" line form (note the colon-in-value is preserved).
	l := parseHelperHeaders([]byte("Authorization: Bearer xyz\nX-Trace: a:b:c\n"))
	if l["Authorization"] != "Bearer xyz" || l["X-Trace"] != "a:b:c" {
		t.Errorf("line form: %+v", l)
	}
}

func TestCallTimeout(t *testing.T) {
	if got := callTimeout(ServerConfig{}); got != defaultCallTimeout {
		t.Errorf("default: %v", got)
	}
	if got := callTimeout(ServerConfig{Timeout: 5000}); got != 5*time.Second {
		t.Errorf("override: %v", got)
	}
}

func TestWantOAuth(t *testing.T) {
	withAuth := map[string]string{"Authorization": "Bearer x"}
	noAuth := map[string]string{}
	cases := []struct {
		sc      ServerConfig
		allow   bool
		headers map[string]string
		want    bool
	}{
		{ServerConfig{}, true, noAuth, true},                // interactive + no static auth → OAuth
		{ServerConfig{}, false, noAuth, false},              // headless → never
		{ServerConfig{}, true, withAuth, false},             // static auth present → no OAuth
		{ServerConfig{Auth: "oauth"}, true, withAuth, true}, // forced even with a header
		{ServerConfig{Auth: "none"}, true, noAuth, false},   // opted out
	}
	for i, c := range cases {
		if got := wantOAuth(c.sc, Options{AllowOAuth: c.allow}, c.headers); got != c.want {
			t.Errorf("case %d: got %v want %v", i, got, c.want)
		}
	}
}

func TestToolName(t *testing.T) {
	if got := ToolName("supabase", "execute_sql"); got != "mcp__supabase__execute_sql" {
		t.Errorf("got %q", got)
	}
	if got := ToolName("my server", "x"); got != "mcp__my_server__x" {
		t.Errorf("sanitized server segment: got %q", got)
	}

	// A function name must match [A-Za-z0-9_-]{1,64} on the strictest lane, so the RAW segment
	// has to be sanitized too — a dotted/hyphenated tool name can't reach the wire raw.
	nameRE := regexp.MustCompile(`^[A-Za-z0-9_]{1,64}$`)
	for _, tc := range []struct{ server, raw string }{
		{"db", "read-file.contents"},                       // illegal chars in raw
		{"srv", strings.Repeat("x", 80)},                   // over-long raw → must be capped
		{strings.Repeat("s", 60), strings.Repeat("y", 60)}, // both pathologically long
		{"weird srv!", "do/it now"},
	} {
		got := ToolName(tc.server, tc.raw)
		if !nameRE.MatchString(got) {
			t.Errorf("ToolName(%q,%q)=%q not a valid function name", tc.server, tc.raw, got)
		}
		if len(got) > 64 {
			t.Errorf("ToolName(%q,%q)=%q exceeds 64 chars (%d)", tc.server, tc.raw, got, len(got))
		}
	}

	// Distinct raw names that sanitize to the same string MUST stay distinct (hash disambiguates),
	// so byName keys never collide.
	a := ToolName("s", "a.b")
	b := ToolName("s", "a/b")
	if a == b {
		t.Errorf("distinct raws collided: both %q", a)
	}
	// Deterministic: same inputs → same name (byName lookup is stable across a session).
	if ToolName("s", "a.b") != a {
		t.Error("ToolName must be deterministic")
	}
}

func TestSchemaMap(t *testing.T) {
	if got := schemaMap(nil); got["type"] != "object" {
		t.Errorf("nil schema should default to object, got %+v", got)
	}
	var typedNil *struct{ X int }
	if got := schemaMap(typedNil); got["type"] != "object" {
		t.Errorf("typed-nil schema should default to object, got %+v", got)
	}
}
