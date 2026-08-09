package runtime

import (
	"os"
	"testing"
)

// TestMain (1) re-execs as a tiny stdio MCP server when MCP_TEST_SERVER=1, so the session-level
// MCP test can dial THIS binary as a real subprocess server (no production seam; see mcp_e2e_test.go),
// and (2) redirects HOME to a throwaway dir for the whole package, so tests that present a plan
// (synthesizePlan → savePlan writes ~/.memcode/plans) land in a temp HOME instead of polluting the
// developer's real ~/.memcode/plans. os.UserHomeDir honors $HOME on unix.
func TestMain(m *testing.M) {
	if os.Getenv("MCP_TEST_SERVER") == "1" {
		runEchoMCPServer()
		os.Exit(0)
	}
	tmp, err := os.MkdirTemp("", "memcode-runtime-home-")
	if err == nil {
		os.Setenv("HOME", tmp)
	}
	code := m.Run()
	if tmp != "" {
		os.RemoveAll(tmp)
	}
	os.Exit(code)
}
