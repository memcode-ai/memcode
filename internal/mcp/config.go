// Package mcp gives memcode an MCP (Model Context Protocol) client: it discovers the
// servers configured across scopes (local / project / user), connects to them (stdio,
// streamable HTTP, or SSE), lists their tools, and exposes a single Call entrypoint. This
// is the same mechanism Claude Code uses to gain third-party tools — and it follows Claude
// Code's documented model: project-scoped servers live in a checked-in .mcp.json and require
// explicit approval before use (see approvals.go), while servers you add yourself (local /
// user scope, in ~/.memcode/mcp.json) are trusted.
//
// The protocol itself is spoken by the official SDK (github.com/modelcontextprotocol/go-sdk);
// this package handles config, scopes, approval, lifecycle, namespacing, and result
// flattening. The permission gate on each CALL lives in the runtime package (runtime/mcp.go).
package mcp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Scope is where a server is configured, mirroring Claude Code's three scopes.
type Scope string

const (
	// ScopeLocal is private to you in ONE project (stored in ~/.memcode/mcp.json under the
	// project path). Trusted — you added it.
	ScopeLocal Scope = "local"
	// ScopeProject is shared with the team via a checked-in <root>/.mcp.json. UNTRUSTED until
	// approved (see approvals.go) — it's repo content that could come from anyone.
	ScopeProject Scope = "project"
	// ScopeUser is available across all your projects (stored in ~/.memcode/mcp.json). Trusted.
	ScopeUser Scope = "user"
)

// ServerConfig is one declared MCP server. A stdio server is launched as a subprocess
// (Command+Args+Env); an http/sse server is reached at URL with optional Headers.
type ServerConfig struct {
	Type    string            `json:"type,omitempty"` // stdio | http | streamable-http | sse (inferred when empty)
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	// HeadersHelper is an optional command run at connect time whose stdout supplies HTTP
	// headers (JSON object, or "Name: value" lines) — for tokens minted per connection rather
	// than stored in config. Merged over static Headers.
	HeadersHelper string `json:"headersHelper,omitempty"`
	// Timeout is a per-server tool-call timeout in milliseconds (overrides the default).
	Timeout int `json:"timeout,omitempty"`
	// Auth controls remote authentication: "" / "auto" attaches the interactive OAuth flow when
	// no static auth header is present; "oauth" forces it; "none" disables it.
	Auth string `json:"auth,omitempty"`
}

// kind normalizes Type to one of stdio|http|sse, inferring from populated fields when omitted.
// "streamable-http" is the spec name for http and is accepted as an alias (as Claude Code does).
func (c ServerConfig) kind() string {
	switch strings.ToLower(c.Type) {
	case "http", "streamable-http":
		return "http"
	case "sse":
		return "sse"
	case "stdio":
		return "stdio"
	}
	if c.URL != "" {
		return "http"
	}
	return "stdio"
}

// Transport returns the normalized transport (stdio | http | sse) for display/inspection.
func (c ServerConfig) Transport() string { return c.kind() }

// ScopedServer is a server resolved with the scope that won it.
type ScopedServer struct {
	Name   string
	Scope  Scope
	Config ServerConfig // env-expanded, ready to connect
}

// config is the standard .mcp.json / mcpServers shape, reused for the project file and the
// per-scope sections of the user store.
type config struct {
	MCPServers map[string]ServerConfig `json:"mcpServers"`
}

// userStore is ~/.memcode/mcp.json: user-scoped servers plus a per-project map of local ones.
type userStore struct {
	MCPServers map[string]ServerConfig `json:"mcpServers,omitempty"` // user scope
	Projects   map[string]config       `json:"projects,omitempty"`   // local scope, keyed by abs project root
}

// ProjectFile is the path to a project's checked-in MCP config.
func ProjectFile(root string) string { return filepath.Join(root, ".mcp.json") }

// UserStoreFile is the path to the user-global MCP store (user + local scopes).
func UserStoreFile() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".memcode", "mcp.json")
}

// Resolve returns every configured server merged across scopes with Claude Code's precedence
// (local > project > user, matched by name; the whole winning entry is used, never merged), each
// env-expanded and tagged with its scope. The caller applies approval policy (project scope).
func Resolve(root string) []ScopedServer {
	out := map[string]ScopedServer{}
	add := func(scope Scope, servers map[string]ServerConfig) {
		for name, sc := range servers {
			if _, taken := out[name]; taken {
				continue // higher-precedence scope already claimed this name
			}
			out[name] = ScopedServer{Name: name, Scope: scope, Config: expandServer(sc)}
		}
	}
	// Highest precedence first so add() can skip names already claimed.
	add(ScopeLocal, LocalServers(root))
	add(ScopeProject, ProjectServers(root))
	add(ScopeUser, UserServers())

	names := make([]string, 0, len(out))
	for n := range out {
		names = append(names, n)
	}
	sort.Strings(names)
	res := make([]ScopedServer, 0, len(out))
	for _, n := range names {
		res = append(res, out[n])
	}
	return res
}

// ProjectServers reads the raw (unexpanded) project-scoped servers from <root>/.mcp.json.
func ProjectServers(root string) map[string]ServerConfig { return readProject(root).MCPServers }

// UserServers reads the raw user-scoped servers from the user store.
func UserServers() map[string]ServerConfig { return readUserStore().MCPServers }

// LocalServers reads the raw local-scoped servers for this project from the user store.
func LocalServers(root string) map[string]ServerConfig {
	abs, _ := filepath.Abs(root)
	if p, ok := readUserStore().Projects[abs]; ok {
		return p.MCPServers
	}
	return nil
}

// AddServer writes a server into the given scope's store (raw, unexpanded — secrets stay as
// ${VAR} references). Overwrites an existing entry of the same name in that scope.
func AddServer(root string, scope Scope, name string, sc ServerConfig) error {
	switch scope {
	case ScopeProject:
		cfg := readProject(root)
		cfg.MCPServers[name] = sc
		return writeProject(root, cfg)
	case ScopeUser:
		us := readUserStore()
		if us.MCPServers == nil {
			us.MCPServers = map[string]ServerConfig{}
		}
		us.MCPServers[name] = sc
		return writeUserStore(us)
	case ScopeLocal:
		us := readUserStore()
		abs, _ := filepath.Abs(root)
		if us.Projects == nil {
			us.Projects = map[string]config{}
		}
		p := us.Projects[abs]
		if p.MCPServers == nil {
			p.MCPServers = map[string]ServerConfig{}
		}
		p.MCPServers[name] = sc
		us.Projects[abs] = p
		return writeUserStore(us)
	}
	return fmt.Errorf("unknown scope %q", scope)
}

// RemoveServer deletes a named server from a scope, reporting whether it existed.
func RemoveServer(root string, scope Scope, name string) (bool, error) {
	switch scope {
	case ScopeProject:
		cfg := readProject(root)
		if _, ok := cfg.MCPServers[name]; !ok {
			return false, nil
		}
		delete(cfg.MCPServers, name)
		return true, writeProject(root, cfg)
	case ScopeUser:
		us := readUserStore()
		if _, ok := us.MCPServers[name]; !ok {
			return false, nil
		}
		delete(us.MCPServers, name)
		return true, writeUserStore(us)
	case ScopeLocal:
		us := readUserStore()
		abs, _ := filepath.Abs(root)
		p, ok := us.Projects[abs]
		if !ok {
			return false, nil
		}
		if _, ok := p.MCPServers[name]; !ok {
			return false, nil
		}
		delete(p.MCPServers, name)
		us.Projects[abs] = p
		return true, writeUserStore(us)
	}
	return false, fmt.Errorf("unknown scope %q", scope)
}

func readProject(root string) config {
	return readConfigFile(ProjectFile(root))
}

func readConfigFile(path string) config {
	var cfg config
	b, err := os.ReadFile(path)
	if err != nil {
		return config{MCPServers: map[string]ServerConfig{}}
	}
	if err := json.Unmarshal(b, &cfg); err != nil || cfg.MCPServers == nil {
		cfg.MCPServers = map[string]ServerConfig{}
	}
	return cfg
}

func writeProject(root string, cfg config) error {
	return writeJSON(ProjectFile(root), cfg)
}

func readUserStore() userStore {
	var us userStore
	path := UserStoreFile()
	if path == "" {
		return userStore{}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return userStore{}
	}
	_ = json.Unmarshal(b, &us)
	return us
}

func writeUserStore(us userStore) error {
	path := UserStoreFile()
	if path == "" {
		return os.ErrNotExist
	}
	return writeJSON(path, us)
}

func writeJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// expandServer expands ${VAR} / ${VAR:-default} in every string the config carries — so a
// secret like an access token lives in the environment, not committed in .mcp.json.
func expandServer(sc ServerConfig) ServerConfig {
	sc.Command = expand(sc.Command)
	sc.URL = expand(sc.URL)
	sc.HeadersHelper = expand(sc.HeadersHelper)
	if sc.Args != nil {
		args := make([]string, len(sc.Args))
		for i, a := range sc.Args {
			args[i] = expand(a)
		}
		sc.Args = args
	}
	sc.Env = expandMap(sc.Env)
	sc.Headers = expandMap(sc.Headers)
	return sc
}

func expandMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = expand(v)
	}
	return out
}

// expand resolves ${VAR} and ${VAR:-default}. os.Expand handles ${VAR} but not the
// :-default form, so we resolve both in one pass.
func expand(s string) string {
	if !strings.Contains(s, "${") {
		return s
	}
	return os.Expand(s, func(key string) string {
		name, def, hasDef := strings.Cut(key, ":-")
		if v, ok := os.LookupEnv(strings.TrimSpace(name)); ok && v != "" {
			return v
		}
		if hasDef {
			return def
		}
		return ""
	})
}
