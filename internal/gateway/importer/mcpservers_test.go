package importer

import "testing"

func TestHermesMCPServers(t *testing.T) {
	yaml := `
mcp_servers:
  github:
    command: gh-mcp
    args: ["--stdio"]
    env: {GH_TOKEN: x}
  docs:
    url: https://mcp.example.com/sse
  broken:
    cwd: /tmp
`
	servers, notes := HermesMCPServers([]byte(yaml))
	if len(servers) != 2 {
		t.Fatalf("want 2 servers, got %d (%+v)", len(servers), servers)
	}
	if s := servers["github"]; s.Command != "gh-mcp" || len(s.Args) != 1 || s.Env["GH_TOKEN"] != "x" {
		t.Errorf("stdio server mapped wrong: %+v", s)
	}
	if s := servers["docs"]; s.URL != "https://mcp.example.com/sse" {
		t.Errorf("remote server mapped wrong: %+v", s)
	}
	if len(notes) == 0 {
		t.Error("the command-less server must produce a note")
	}
}

func TestOpenClawMCPServers(t *testing.T) {
	data := `{"mcp":{"servers":{"linear":{"command":"linear-mcp","toolFilter":{"include":["issues"]}}}}}`
	servers, notes := OpenClawMCPServers([]byte(data))
	if len(servers) != 1 || servers["linear"].Command != "linear-mcp" {
		t.Fatalf("mapped wrong: %+v", servers)
	}
	if len(notes) == 0 {
		t.Error("a dropped tool filter must produce a note")
	}
}
