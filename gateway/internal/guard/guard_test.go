// Package guard states the gateway's side of the product/service boundary:
// the gateway shares the protocol vocabulary (internal/wire), the catalog,
// and the wire adapters (internal/providers) with the CLI — but it must never
// import the client-side runtime: the agent, the TUI, local state, or the
// side-channel client that talks TO the gateway. Pulling any of those would
// invert the boundary and drag client concerns server-side. (The reverse rule
// — product code never imports gateway/internal — lives in internal/guard,
// alongside the module-wide vendor-SDK and gofmt gates.)
package guard

import (
	"os/exec"
	"strings"
	"testing"
)

const modulePrefix = "github.com/memcode-ai/memcode"

func TestGatewayNeverImportsProductRuntime(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", modulePrefix+"/gateway/...").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps gateway/...: %v\n%s", err, out)
	}
	forbiddenPrefixes := []string{
		modulePrefix + "/internal/agent",          // the agent runtime
		modulePrefix + "/internal/vxui",           // the TUI
		modulePrefix + "/internal/store",          // local .memcode state
		modulePrefix + "/internal/gateway/client", // the client that talks TO this server
		modulePrefix + "/internal/doctrine",       // prompt doctrine is client-owned
		modulePrefix + "/internal/llm",            // the CLI's selection/metering runner
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		p := strings.TrimSpace(line)
		for _, f := range forbiddenPrefixes {
			if p == f || strings.HasPrefix(p, f+"/") {
				t.Errorf("gateway transitively depends on %q — the serving side must not import the product runtime", p)
			}
		}
	}
}

// TestGatewayCarriesNoCloudVocabulary is the doctrine's teeth: the OSS gateway
// must contain no memcode-cloud commercial machinery. Bans the vocabulary of
// a paid service in gateway/ source, allowing only the wire-protocol error
// codes a self-host gateway still speaks (insufficient_credits) and generic
// mentions in this guard file.
func TestGatewayCarriesNoCloudVocabulary(t *testing.T) {
	root, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}").Output()
	if err != nil {
		t.Fatal(err)
	}
	dir := strings.TrimSpace(string(root)) + "/gateway"
	// Case-insensitive banned substrings — cloud-service concepts that must
	// live only in the private gateway-cloud module.
	banned := []string{"stripe", "subscription", "secretmanager", "secret manager",
		"webapp", "web_app", "recordusage", "record-usage", "debit", "wallet"}
	out, _ := exec.Command("grep", "-rilnE", strings.Join(banned, "|"),
		"--include=*.go", dir).CombinedOutput()
	var bad []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" || strings.HasSuffix(line, "/guard/guard_test.go") {
			continue
		}
		bad = append(bad, line)
	}
	if len(bad) > 0 {
		t.Errorf("cloud-service vocabulary found in the OSS gateway (move it to the private gateway-cloud module):\n%s",
			strings.Join(bad, "\n"))
	}
}
