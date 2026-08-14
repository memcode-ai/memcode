// Package guard holds module-wide dependency-wall tests — the teeth behind the
// layering invariants of the folded monorepo: the wire package is stdlib-only,
// the side-channel client stays thin, vendor SDKs appear only inside their own
// provider adapters, the shared kernel stays vendor-free, and the memcode
// dialect has exactly one constructor. (The gateway's own walls live in
// gateway/internal/guard.)
package guard

import (
	"os/exec"
	"strings"
	"testing"
)

const modulePrefix = "github.com/memcode-ai/memcode"

// deps returns the transitive import closure of pkg (including pkg itself).
// `go list` keeps the guard honest about TRANSITIVE deps, not just direct ones.
func deps(t *testing.T, pkg string) []string {
	t.Helper()
	out, err := exec.Command("go", "list", "-deps", pkg).CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps %s: %v\n%s", pkg, err, out)
	}
	var ps []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			ps = append(ps, line)
		}
	}
	return ps
}

// isStdlib: first path segment contains no dot.
func isStdlib(p string) bool {
	seg := p
	if i := strings.IndexByte(p, '/'); i >= 0 {
		seg = p[:i]
	}
	return !strings.Contains(seg, ".")
}

// TestWireIsStdlibOnly: the wire package is the protocol vocabulary — it must
// carry zero third-party dependencies so every consumer (the gateway included)
// pays nothing to speak it.
func TestWireIsStdlibOnly(t *testing.T) {
	for _, p := range deps(t, modulePrefix+"/internal/wire") {
		if isStdlib(p) || p == modulePrefix+"/internal/wire" {
			continue
		}
		t.Errorf("internal/wire must be stdlib-only, but transitively depends on %q", p)
	}
}

// TestCatalogIsStdlibOnly: model facts are data + lookups, nothing more.
func TestCatalogIsStdlibOnly(t *testing.T) {
	for _, p := range deps(t, modulePrefix+"/catalog") {
		if isStdlib(p) || p == modulePrefix+"/catalog" {
			continue
		}
		t.Errorf("catalog must be stdlib-only, but transitively depends on %q", p)
	}
}

// TestSideChannelClientStaysThin: the gateway side-channel client may use only
// the stdlib plus wire/catalog — if it ever pulls the runtime (sqlite, TUI,
// chrome), the transport layer is leaking upward.
func TestSideChannelClientStaysThin(t *testing.T) {
	allowed := map[string]bool{
		modulePrefix + "/internal/cloudclient": true,
		modulePrefix + "/internal/wire":        true,
		modulePrefix + "/catalog":              true,
	}
	for _, p := range deps(t, modulePrefix+"/internal/cloudclient") {
		if isStdlib(p) || allowed[p] {
			continue
		}
		t.Errorf("internal/cloudclient must stay thin (stdlib + wire/catalog), but depends on %q", p)
	}
}

// vendorSDKs may be imported ONLY by their own adapter packages — the
// one-implementation-per-protocol invariant. provcore is deliberately
// vendor-free (error-shape extractors register at adapter init).
var vendorSDKs = map[string]string{
	"github.com/openai/openai-go":            modulePrefix + "/internal/providers/openai",
	"github.com/anthropics/anthropic-sdk-go": modulePrefix + "/internal/providers/anthropic",
	"google.golang.org/genai":                modulePrefix + "/internal/providers/gemini",
	"github.com/emersion/go-imap":            modulePrefix + "/internal/channels/email",
	"github.com/bwmarrin/discordgo":          modulePrefix + "/internal/channels/discord",
	"github.com/slack-go/slack":              modulePrefix + "/internal/channels/slack",
}

func directImports(t *testing.T, pkg string) []string {
	t.Helper()
	out, err := exec.Command("go", "list", "-f", `{{join .Imports "\n"}}`, pkg).CombinedOutput()
	if err != nil {
		t.Fatalf("go list %s: %v\n%s", pkg, err, out)
	}
	var ps []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			ps = append(ps, line)
		}
	}
	return ps
}

func modulePackages(t *testing.T) []string {
	t.Helper()
	out, err := exec.Command("go", "list", modulePrefix+"/...").CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	var ps []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			ps = append(ps, line)
		}
	}
	return ps
}

// TestVendorSDKsOnlyInTheirAdapters: a vendor client library imported outside
// its adapter means a second protocol implementation is growing.
func TestVendorSDKsOnlyInTheirAdapters(t *testing.T) {
	for _, pkg := range modulePackages(t) {
		for _, imp := range directImports(t, pkg) {
			for sdkPrefix, home := range vendorSDKs {
				if (imp == sdkPrefix || strings.HasPrefix(imp, sdkPrefix+"/")) && pkg != home {
					t.Errorf("%s imports %s — vendor SDKs live ONLY in %s", pkg, imp, home)
				}
			}
		}
	}
}

// TestProvcoreIsVendorFree: the shared kernel must not hard-link any vendor
// SDK — that is what lets a consumer link one adapter without dragging in
// every vendor's client library.
func TestProvcoreIsVendorFree(t *testing.T) {
	for _, p := range deps(t, modulePrefix+"/internal/providers/provcore") {
		for sdkPrefix := range vendorSDKs {
			if p == sdkPrefix || strings.HasPrefix(p, sdkPrefix+"/") {
				t.Errorf("provcore transitively depends on %q — keep the kernel vendor-free (extractor registry)", p)
			}
		}
	}
}

// TestMemcodeDialectConstructedOnlyViaItsPackage: Config.Memcode is the
// dialect switch; setting it anywhere but internal/providers/memcode bypasses
// the /v1 mount and the dialect's one constructor. Source grep, non-test files.
func TestMemcodeDialectConstructedOnlyViaItsPackage(t *testing.T) {
	root, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}").Output()
	if err != nil {
		t.Fatal(err)
	}
	dir := strings.TrimSpace(string(root))
	out, _ := exec.Command("grep", "-rln", "--include=*.go", "Memcode: *true", dir).CombinedOutput()
	for _, f := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if f == "" || strings.HasSuffix(f, "_test.go") || strings.Contains(f, "/internal/providers/memcode/") {
			continue
		}
		t.Errorf("%s sets Memcode: true — the memcode dialect is constructed only via providers/memcode.New", f)
	}
}

// TestNoGatewayInternalsOutsideGateway: the ONE hard layering rule of the
// product/service split — CLI/product code never imports the gateway's
// serving internals. Go's internal visibility already enforces this at
// compile time; the test states the intent and catches path tricks.
func TestNoGatewayInternalsOutsideGateway(t *testing.T) {
	for _, pkg := range modulePackages(t) {
		if strings.HasPrefix(pkg, modulePrefix+"/gateway") {
			continue
		}
		for _, imp := range directImports(t, pkg) {
			if strings.HasPrefix(imp, modulePrefix+"/gateway/internal") {
				t.Errorf("%s imports %s — product code must never depend on gateway serving internals", pkg, imp)
			}
		}
	}
}

// TestModuleGofmt: every file formatted (the ONE gofmt gate for the module).
func TestModuleGofmt(t *testing.T) {
	root, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}").Output()
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("gofmt", "-l", strings.TrimSpace(string(root))).CombinedOutput()
	if err != nil {
		t.Fatalf("gofmt -l: %v\n%s", err, out)
	}
	if files := strings.TrimSpace(string(out)); files != "" {
		t.Errorf("unformatted files:\n%s", files)
	}
}
