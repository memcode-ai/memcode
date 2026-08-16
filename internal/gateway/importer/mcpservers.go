// MCP server migration. Both Hermes (config.yaml mcp_servers) and OpenClaw
// (openclaw.json mcp.servers) configure MCP servers with the same essential
// shape memcode's .mcp.json uses — command/args/env for stdio, url for remote —
// so they carry straight over into the user scope. Fields memcode doesn't have
// a home for (cwd, per-server tool include/exclude filters) become notes, never
// silent drops.
package importer

import (
	"encoding/json"
	"fmt"
	"strings"

	yaml "go.yaml.in/yaml/v4"

	"github.com/memcode-ai/memcode/internal/mcp"
)

// srcMCPServer covers the fields both products use for one server.
type srcMCPServer struct {
	Command string            `yaml:"command" json:"command"`
	Args    []string          `yaml:"args" json:"args"`
	Env     map[string]string `yaml:"env" json:"env"`
	Cwd     string            `yaml:"cwd" json:"cwd"`
	URL     string            `yaml:"url" json:"url"`
	Headers map[string]string `yaml:"headers" json:"headers"`
	Tools   struct {
		Include []string `yaml:"include" json:"include"`
		Exclude []string `yaml:"exclude" json:"exclude"`
	} `yaml:"tools" json:"toolFilter"`
}

// convertMCP maps one source server to memcode's config, noting what didn't map.
func convertMCP(name string, s srcMCPServer, notes *[]string) (mcp.ServerConfig, bool) {
	out := mcp.ServerConfig{Command: s.Command, Args: s.Args, Env: s.Env, URL: s.URL, Headers: s.Headers}
	if strings.TrimSpace(s.Command) == "" && strings.TrimSpace(s.URL) == "" {
		*notes = append(*notes, fmt.Sprintf("mcp: server %q has neither a command nor a url — skipped", name))
		return out, false
	}
	if strings.TrimSpace(s.Cwd) != "" {
		*notes = append(*notes, fmt.Sprintf("mcp: server %q set a working directory (%s), which memcode doesn't carry — it runs from the project root", name, s.Cwd))
	}
	if len(s.Tools.Include) > 0 || len(s.Tools.Exclude) > 0 {
		*notes = append(*notes, fmt.Sprintf("mcp: server %q had a tool include/exclude filter, which memcode doesn't carry — all its tools are available (gated by approval)", name))
	}
	return out, true
}

// HermesMCPServers reads the mcp_servers block of a Hermes config.yaml.
func HermesMCPServers(configYAML []byte) (map[string]mcp.ServerConfig, []string) {
	var hc struct {
		Servers map[string]srcMCPServer `yaml:"mcp_servers"`
	}
	if err := yaml.Unmarshal(configYAML, &hc); err != nil || len(hc.Servers) == 0 {
		return nil, nil
	}
	return convertAll(hc.Servers)
}

// OpenClawMCPServers reads the mcp.servers block of an openclaw.json.
func OpenClawMCPServers(data []byte) (map[string]mcp.ServerConfig, []string) {
	var oc struct {
		MCP struct {
			Servers map[string]srcMCPServer `json:"servers"`
		} `json:"mcp"`
	}
	if err := json.Unmarshal(data, &oc); err != nil || len(oc.MCP.Servers) == 0 {
		return nil, nil
	}
	return convertAll(oc.MCP.Servers)
}

func convertAll(servers map[string]srcMCPServer) (map[string]mcp.ServerConfig, []string) {
	out := map[string]mcp.ServerConfig{}
	var notes []string
	for name, s := range servers {
		if sc, ok := convertMCP(name, s, &notes); ok {
			out[name] = sc
		}
	}
	return out, notes
}
