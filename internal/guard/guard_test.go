// Package guard holds module-wide dependency-wall tests — the teeth behind the
// layering invariants of the folded monorepo: the wire package is stdlib-only,
// the side-channel client stays thin, vendor SDKs appear only inside their own
// provider adapters, the shared kernel stays vendor-free, and the memcode
// dialect has exactly one constructor. (The serving gateway's own walls live
// with the gateway deployment, not in this repo.)
package guard

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"
)

const modulePrefix = "github.com/memcode-ai/memcode"

// goListLines runs `go list` and returns its STDOUT lines.
//
// Reading stdout only is load-bearing, not tidiness. `go list` writes advisory
// warnings to stderr while still exiting 0 with a complete, correct package
// list — most commonly "warning: ignoring symlink ..." when an untracked
// sibling directory (a node_modules tree from another branch, say) sits in the
// module root. CombinedOutput folds those warning lines into the results, each
// one then gets handed back to `go list` as if it were a package path, and THAT
// invocation fails. The original symptom looked like a desktop/node_modules
// problem; it was really this function laundering stderr into data.
//
// Genuine failures still fail: a non-zero exit is fatal, and so is an empty
// package list, which would otherwise let a guard pass vacuously.
func goListLines(t *testing.T, label string, args ...string) []string {
	t.Helper()
	cmd := exec.Command("go", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s: %v\n%s", label, err, stderr.String())
	}
	var ps []string
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			ps = append(ps, line)
		}
	}
	return ps
}

// deps returns the transitive import closure of pkg (including pkg itself).
// `go list` keeps the guard honest about TRANSITIVE deps, not just direct ones.
func deps(t *testing.T, pkg string) []string {
	t.Helper()
	ps := goListLines(t, "go list -deps "+pkg, "list", "-deps", pkg)
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
	"github.com/golang-jwt/jwt":              modulePrefix + "/internal/webjwt",
	"github.com/gorilla/websocket":           modulePrefix + "/internal/channels/mattermost",
	"golang.org/x/oauth2":                    modulePrefix + "/internal/triggers/googlechat",
}

func directImports(t *testing.T, pkg string) []string {
	t.Helper()
	return goListLines(t, "go list "+pkg, "list", "-f", `{{join .Imports "\n"}}`, pkg)
}

func modulePackages(t *testing.T) []string {
	pkgs := goListLines(t, "go list", "list", modulePrefix+"/...")
	if len(pkgs) == 0 {
		t.Fatal("go list returned no packages — every guard below would pass vacuously")
	}
	return pkgs
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

// TestOneModelAuthority is the teeth behind the pinned-model doctrine.
//
// Two catalog values are load-bearing precisely because they are consulted in
// exactly one place each:
//
//   - catalog.DefaultModel() SEEDS the pin on an install that has never chosen
//     anything, and only the pin resolver may read it. The moment a second
//     caller resolves it — "no model? use the default" on every request — the
//     default stops being an initializer and becomes Automatic routing again,
//     which is the thing this whole change deleted.
//   - catalog.UtilityModel() serves internal plumbing (classify/authorize,
//     compact, shrinkwrap) and only selection may read it. A second caller
//     would be a second model-selection authority, which is what the pin
//     replaced.
//
// If this fails, do not add an exception. Route the call through the pin
// resolver, or use the session's pin.
func TestOneModelAuthority(t *testing.T) {
	cases := []struct {
		call    string
		allowed string // the ONE non-test file permitted to make it
	}{
		{"catalog.DefaultModel()", "internal/config/pin.go"},
		{"catalog.UtilityModel()", "internal/llm/resolve.go"},
	}
	for _, tc := range cases {
		out, err := exec.Command("grep", "-rln", "--include=*.go", tc.call, "../..").Output()
		if err != nil && len(out) == 0 {
			t.Fatalf("grep for %s found nothing at all — has the call been renamed?", tc.call)
		}
		for _, f := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			f = strings.TrimPrefix(strings.TrimSpace(f), "../../")
			switch {
			case f == "" || strings.HasSuffix(f, "_test.go"):
				continue // tests may assert on it
			case f == "catalog/catalog.go":
				continue // the definition itself
			case f == tc.allowed:
				continue // the one authority
			default:
				t.Errorf("%s is called from %s — only %s may call it. "+
					"A second caller turns a one-time default into per-request routing; "+
					"route through the pin resolver instead.", tc.call, f, tc.allowed)
			}
		}
	}
}

// TestPolicyIsUserAuthored is the teeth behind the policy layer's one rule:
//
//	Policy chooses behavior. The model may not synthesize policy.
//
// Mechanically, that means the policy store has exactly two writers — the tool
// the user's instruction flows through, and the runtime that resolves it. If a
// third appears, the likeliest reason is something deciding policy on its own,
// which is the automatic routing memcode deleted arriving through a settings
// API instead of a router.
//
// If this fails, do not add an exception. Route the change through the tool.
func TestPolicyIsUserAuthored(t *testing.T) {
	allowed := map[string]bool{
		"internal/policy/store.go":             true, // the store itself
		"internal/agent/runtime/policytool.go": true, // the user's instruction
		"internal/agent/runtime/runtime.go":    true, // constructs the resolver
		"internal/agent/runtime/ui.go":         true, // SetPolicy at the cmd boundary
	}
	for _, call := range []string{"policy.SetField(", "policy.UnsetTarget(", "policy.Save("} {
		out, _ := exec.Command("grep", "-rln", "--include=*.go", call, "../..").Output()
		for _, f := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			f = strings.TrimPrefix(strings.TrimSpace(f), "../../")
			if f == "" || strings.HasSuffix(f, "_test.go") || allowed[f] {
				continue
			}
			t.Errorf("%s writes policy from %s. Policy is authored by an explicit user "+
				"instruction routed through the policy tool — a new writer is how a model "+
				"starts choosing behavior for itself.", call, f)
		}
	}
}

// TestPrefsCannotWritePolicy keeps the two preference systems separate.
//
// internal/prefs INFERS standing preferences from repeated signals and injects
// advisory prose. internal/policy is explicit, immediate and programmatic. A
// write path from the first into the second would mean a reducer noticing a
// pattern could silently rewire which model runs — exactly the spookiness the
// split exists to prevent.
func TestPrefsCannotWritePolicy(t *testing.T) {
	out, _ := exec.Command("grep", "-rln", "--include=*.go",
		"memcode/internal/policy", "../prefs").Output()
	for _, f := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.TrimSpace(f) == "" {
			continue
		}
		t.Errorf("internal/prefs imports internal/policy (%s). Inferred preferences must "+
			"never create or modify executable policy — they are advisory prose, and policy "+
			"is what the user explicitly asked for.", strings.TrimSpace(f))
	}
}
