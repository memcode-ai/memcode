package llm

import (
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/memcode-ai/memcode/catalog"
	"github.com/memcode-ai/memcode/internal/wire"
)

// TestNoDirectProviderCallsOutsideGateway is the architectural guard: every model
// call must go through the metered Runner (with a Purpose), so cost/token/route
// accounting can't be bypassed. A direct provider call looks like
// `.Complete(ctx, provider.Request{…})` / `.Stream(ctx, req, …)` — the Request
// right after ctx. The gateway form has a Purpose in between
// (`.Complete(ctx, llm.MainLoop, req)`), so this regex only catches bypasses.
//
// Allowed exceptions: internal/llm (the metering gateway itself), internal/provider
// (the connection layer it wraps), and the folded wire adapters + side-channel
// client (internal/providers/, internal/gateway/) — all transport, no policy.
func TestNoDirectProviderCallsOutsideGateway(t *testing.T) {
	root := filepath.Join("..", "..") // internal/llm -> cli/
	banned := regexp.MustCompile(`\.(Complete|Stream)\(\s*ctx\s*,\s*(wire\.Request|common\.Request|provider\.Request|req[,)\s])`)
	var violations []string
	for _, dir := range []string{"internal", "cmd"} {
		_ = filepath.WalkDir(filepath.Join(root, dir), func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel := filepath.ToSlash(path)
			if strings.Contains(rel, "internal/llm/") || strings.Contains(rel, "internal/provider/") ||
				strings.Contains(rel, "internal/providers/") || strings.Contains(rel, "internal/gateway/") {
				return nil // the metering gateway and the transport layers are the only allowed callers
			}
			data, e := os.ReadFile(path)
			if e == nil && banned.Match(data) {
				violations = append(violations, rel)
			}
			return nil
		})
	}
	if len(violations) > 0 {
		t.Fatalf("direct provider Complete/Stream call(s) bypass the llm.Runner gateway in:\n  %s\n"+
			"route the call through a *llm.Runner with a Purpose so it's metered.",
			strings.Join(violations, "\n  "))
	}
}

// TestNoClientSidePromptConstruction is the SECOND architectural guard: prompt
// text has exactly ONE home — internal/doctrine — and no other product code may
// construct a system prompt. The chokepoint is the Request.System field —
// setting it is the only way to send a system prompt (direct provider calls
// are already banned above), so the field name may appear ONLY in the exempt
// dirs (see promptExempt). New model features set Mode+Facts (live path) /
// extend compose; if this test is in your way, the doctrine you're writing
// belongs in internal/doctrine.
func TestNoClientSidePromptConstruction(t *testing.T) {
	root := filepath.Join("..", "..")
	banned := regexp.MustCompile(`\bSystem:\s`)
	var violations []string
	for _, dir := range []string{"internal", "cmd"} {
		_ = filepath.WalkDir(filepath.Join(root, dir), func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel := filepath.ToSlash(path)
			if promptExempt(rel) {
				return nil
			}
			data, e := os.ReadFile(path)
			if e == nil && banned.Match(data) {
				violations = append(violations, rel)
			}
			return nil
		})
	}
	if len(violations) > 0 {
		t.Fatalf("client-side system-prompt construction in:\n  %s\n"+
			"prompt text lives ONLY in internal/doctrine; everywhere else set Request.Mode (+Facts).",
			strings.Join(violations, "\n  "))
	}
}

// promptExempt reports whether a file may touch system-prompt construction:
// the wire layer (internal/provider — serializes the spec, owns no prose) and
// the doctrine package (internal/doctrine — the ONE sanctioned home for
// prompt text). The ban holds everywhere else.
func promptExempt(rel string) bool {
	// The doctrine package is the ONE home for prompt PROSE. The folded wire
	// adapters (internal/providers/) also touch Request.System fields — pure
	// serialization onto vendor wires, zero prose — so they are exempt as
	// transport, like the compose hook plumbing in internal/provider/.
	return strings.Contains(rel, "internal/doctrine/") ||
		strings.Contains(rel, "internal/providers/") ||
		strings.Contains(rel, "internal/provider/")
}

// TestPromptGuardScoping pins the exemption surface itself: exactly the wire
// layer and the doctrine package, nothing else — so a future "just add my dir"
// edit is a visible, deliberate act, and the exempted doctrine dir actually
// exists (an exemption must never outlive the package it scopes).
func TestPromptGuardScoping(t *testing.T) {
	for rel, want := range map[string]bool{
		"memcode/internal/provider/wire.go":         true, // compose-hook plumbing (transport, no prose)
		"memcode/internal/providers/anthropic/a.go": true, // vendor-wire serialization (transport, no prose)
		"memcode/internal/doctrine/prompts.go":      true,
		"memcode/internal/agent/runtime/prompts.go": false,
		"memcode/internal/llm/runner.go":            false,
		"memcode/internal/agent/runtime/chat.go":    false,
		"memcode/main.go":                           false,
	} {
		if got := promptExempt(rel); got != want {
			t.Errorf("promptExempt(%q) = %v, want %v", rel, got, want)
		}
	}
	for _, dir := range []string{"internal/provider", "internal/providers", "internal/doctrine"} {
		if st, err := os.Stat(filepath.Join("..", "..", dir)); err != nil || !st.IsDir() {
			t.Errorf("exempt dir %s missing (%v) — remove its exemption if the package is gone", dir, err)
		}
	}
}

// TestLedgerRecordsByPurpose checks aggregate + per-purpose accounting.
func TestLedgerRecordsByPurpose(t *testing.T) {
	l := newLedger()
	l.record(catalog.ModelSonnet, MainLoop, wire.Response{InputTokens: 1000, OutputTokens: 500}, time.Second)
	l.record(catalog.ModelHaiku, Classify, wire.Response{InputTokens: 200, OutputTokens: 50}, time.Second)
	l.record(catalog.ModelSonnet, MainLoop, wire.Response{OutputTokens: 100, CacheReadTokens: 4000}, time.Second)

	tot := l.Total()
	if tot.Calls != 3 || tot.In != 1200 || tot.Out != 650 || tot.CacheRead != 4000 {
		t.Fatalf("totals wrong: %+v", tot)
	}
	if tot.USD <= 0 {
		t.Fatalf("cost should accumulate, got %v", tot.USD)
	}
	bp := l.ByPurpose()
	if bp[MainLoop].Calls != 2 || bp[Classify].Calls != 1 {
		t.Fatalf("per-purpose calls wrong: %+v", bp)
	}
	// Unlabelled calls fall under Other, still counted.
	l.record(catalog.ModelSonnet, "", wire.Response{OutputTokens: 10}, time.Second)
	if l.ByPurpose()[Other].Calls != 1 {
		t.Fatal("unlabelled call should be counted under Other")
	}
}

// TestLedgerByBackend checks the lane-economics telemetry: per-backend split,
// real pricing, latency, and fallback-reason counts. The cheap lane is REAL
// money post the 2026-06-12 cutover — a cheap-lane call must be priced at its served
// model's rate card, never $0 (the $0 branch hid a 3-7x underreport of burn).
func TestLedgerByBackend(t *testing.T) {
	l := newLedger()
	// Cheap-lane call: response tags the vendor-neutral wire backend "cheap" and the
	// SANITIZED bare model id (the gateway never exposes the vendor path).
	l.record(catalog.ModelSonnet, MainLoop, wire.Response{
		InputTokens: 1_000_000, OutputTokens: 1_000_000,
		Model: "glm-5p2", Backend: "cheap",
	}, 2*time.Second)
	// Anthropic absorber call while the lane erred.
	l.record(catalog.ModelSonnet, MainLoop, wire.Response{
		InputTokens: 1000, OutputTokens: 500,
		Model: catalog.ModelSonnet, Backend: "anthropic", FallbackReason: "cheap_lane_error",
	}, time.Second)

	bb := l.ByBackend()
	v, a := bb["cheap"], bb["anthropic"]
	if v.Calls != 1 || a.Calls != 1 {
		t.Fatalf("backend split wrong: %+v", bb)
	}
	// 1M in + 1M out at glm rates ($1.40 + $4.40) — the real cheap-lane bill.
	if math.Abs(v.USD-5.80) > 1e-9 {
		t.Fatalf("cheap-lane tokens must be priced at the glm rate ($5.80), got %v", v.USD)
	}
	if v.LatencyMS != 2000 {
		t.Fatalf("latency wrong: %v", v.LatencyMS)
	}
	if a.Reasons["cheap_lane_error"] != 1 {
		t.Fatalf("fallback reasons wrong: %+v", a.Reasons)
	}
	if a.USD <= 0 {
		t.Fatalf("anthropic call must have real cost: %+v", a)
	}
	// Total cost prices the models that ACTUALLY ran (cheap lane + anthropic).
	if tot := l.Total(); math.Abs(tot.USD-(v.USD+a.USD)) > 1e-9 {
		t.Fatalf("total must price the served models, got %v want %v", tot.USD, v.USD+a.USD)
	}
}

// TestForkSharesLedger: a forked runner is a distinct object but shares the Ledger,
// so sub-agent spend lands centrally without sharing the executor.
func TestForkSharesLedger(t *testing.T) {
	r := NewRunner(nil)
	f := r.Fork()
	if f == r {
		t.Fatal("Fork must return a distinct Runner")
	}
	if f.Ledger() != r.Ledger() {
		t.Fatal("Fork must share the same Ledger")
	}
}
