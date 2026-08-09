package runtime

import (
	"regexp"
	"strings"
	"testing"

	"github.com/memcode-ai/memcode/internal/wire"
)

// "random" must resolve to a CONCRETE built-in voice (chaos mode), never to "random"
// itself or the empty default — the gateway should only ever see a real key.
func TestRandomPersonalityResolves(t *testing.T) {
	pool := map[string]bool{}
	for _, k := range personalityPool {
		if k == "" || k == personalityRandom {
			t.Fatalf("pool must not contain %q", k)
		}
		pool[k] = true
	}
	if len(pool) == 0 {
		t.Fatal("personalityPool is empty")
	}
	for i := 0; i < 100; i++ {
		if got := randomPersonality(); !pool[got] {
			t.Fatalf("randomPersonality returned %q, not in the pool", got)
		}
	}
}

// renderSpec is a fully-loaded spec for the render() tests: every fact that
// steers a volatile block, plus a turn-scoped extra.
func renderSpec() promptSpec {
	p := promptSpec{mode: "chat", facts: map[string]string{
		"root":        "/repo",
		"platform":    "darwin",
		"shell":       "zsh",
		"overview":    "Subsystems: alpha, beta",
		"room":        "repair",
		"personality": "dry",
		"extramile":   "on",
		"mcp":         "linear(12)",
	}}
	return p.withExtra("SKILLS CATALOG:\n- deploy")
}

var renderTodayLine = regexp.MustCompile(`^\[today: [^\]]+\]`)

// TestRenderTwoMessageShape: render() emits the one-wire two-system-message
// form — doctrine + environment facts in the STABLE first message, per-turn
// signals + the spec's extra in the VOLATILE second, never mixed.
func TestRenderTwoMessageShape(t *testing.T) {
	stable, volatile, err := renderSpec().render()
	if err != nil {
		t.Fatal(err)
	}
	// Stable: the doctrine body around the client facts.
	for _, needle := range []string{"Core laws", "/repo", "Platform: darwin", "Subsystems: alpha, beta"} {
		if !strings.Contains(stable, needle) {
			t.Errorf("stable message missing %q", needle)
		}
		if needle != "Core laws" && strings.Contains(volatile, needle) {
			t.Errorf("stable content %q leaked into the volatile message", needle)
		}
	}
	// Volatile: leads with the date stamp, carries the per-turn signals, ends
	// with the spec's extra.
	if !renderTodayLine.MatchString(volatile) {
		t.Fatalf("volatile message must lead with the [today: …] stamp, got %q", volatile[:min(len(volatile), 60)])
	}
	for _, needle := range []string{"[interaction signal — repair mode]", "[voice — tone only]", "[extra mile", "[mcp servers connected: linear(12)]"} {
		if !strings.Contains(volatile, needle) {
			t.Errorf("volatile message missing %q", needle)
		}
		if strings.Contains(stable, needle) {
			t.Errorf("volatile block %q leaked into the stable message — cache-buster", needle)
		}
	}
	if !strings.HasSuffix(volatile, "SKILLS CATALOG:\n- deploy") {
		t.Error("the turn-scoped extra must be the LAST block of the volatile message")
	}
	if strings.Contains(stable, "SKILLS CATALOG") {
		t.Error("the turn-scoped extra leaked into the stable message")
	}
}

// TestRenderStablePrefixByteStable: identical specs must render a byte-
// identical stable message every time — it is the 1h cache key; a wandering
// byte is a silent cache miss. Volatile must be equally deterministic modulo
// its date stamp.
func TestRenderStablePrefixByteStable(t *testing.T) {
	spec := renderSpec()
	firstStable, firstVolatile := "", ""
	for i := 0; i < 25; i++ {
		stable, volatile, err := spec.render()
		if err != nil {
			t.Fatal(err)
		}
		volatile = renderTodayLine.ReplaceAllString(volatile, "[today: X]")
		if i == 0 {
			firstStable, firstVolatile = stable, volatile
			continue
		}
		if stable != firstStable {
			t.Fatalf("stable message changed between identical renders (iteration %d)", i)
		}
		if volatile != firstVolatile {
			t.Fatalf("volatile message changed between identical renders (iteration %d)", i)
		}
	}
}

// TestRenderSpecTransforms: the with* copy-on-write helpers feed render()
// without cross-contaminating shared specs.
func TestRenderSpecTransforms(t *testing.T) {
	base := renderSpec()
	planned := base.withMode("plan").withFact("nudge", "wrapup")
	if _, _, err := base.render(); err != nil {
		t.Fatal(err)
	}
	stable, volatile, err := planned.render()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stable, "PLAN MODE") {
		t.Error("withMode(plan) must render the plan doctrine")
	}
	if !strings.Contains(volatile, "[Write the FINAL plan NOW") {
		t.Error("nudge fact must select the wrap-up nudge in the volatile message")
	}
	if base.mode != "chat" || base.facts["nudge"] != "" {
		t.Error("with* helpers must not mutate the shared base spec")
	}
}

// TestRenderErrorContract: compose errors surface, they don't best-effort.
func TestRenderErrorContract(t *testing.T) {
	if _, _, err := (promptSpec{mode: "bogus"}).render(); err == nil {
		t.Error("unknown mode must error")
	}
	if _, _, err := (promptSpec{mode: "exec", facts: map[string]string{}}).render(); err == nil || !strings.Contains(err.Error(), "facts.root") {
		t.Errorf("missing root must surface the facts.root error, got %v", err)
	}
}

// TestRequestWireSelection: request() is the wire seam. The legacy default
// request() composes locally via render() into the two-system form — Facts
// stay off the wire, Mode stays stamped (a selection input for the Runner's
// ladder), and the stable half is byte-stable across turns (the cache key).
func TestRequestWireSelection(t *testing.T) {
	spec := renderSpec()

	r := spec.request(wire.Request{})
	stable, volatile, err := spec.render()
	if err != nil {
		t.Fatal(err)
	}
	norm := func(s string) string { return renderTodayLine.ReplaceAllString(s, "[today: X]") }
	if r.System != stable {
		t.Error("compat request must carry the rendered stable prefix as the first system half")
	}
	if norm(r.SystemVolatile) != norm(volatile) {
		t.Error("compat request must carry the rendered volatile suffix (incl. the extra)")
	}
	if r.Facts != nil {
		t.Error("facts must stay OFF the wire on the compat path")
	}
	if r.Mode != "chat" {
		t.Error("mode must stay stamped — the Runner's semantic ladder reads it")
	}
	if r2 := spec.request(wire.Request{}); r2.System != r.System {
		t.Error("the stable prefix must be byte-stable across turns")
	}

	// A spec that cannot render falls through to the legacy stamp — the compat
	// transport composes at send time and surfaces the error on the turn.
	bad := promptSpec{mode: "bogus", facts: map[string]string{"k": "v"}}
	if rb := bad.request(wire.Request{}); rb.Mode != "bogus" || rb.Facts == nil {
		t.Errorf("unrenderable spec must fall through to the legacy stamp: %+v", rb)
	}
}
