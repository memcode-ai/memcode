package doctrine

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

// factsFull exercises every key of the facts contract (see the package doc).
func factsFull() map[string]string {
	return map[string]string{
		"root":        "/repo",
		"platform":    "darwin",
		"shell":       "zsh",
		"overview":    "Subsystems: alpha, beta",
		"pack":        `{"files":[]}`,
		"plan":        "1. do the thing",
		"scope":       "billing",
		"room":        "repair",
		"personality": "dry",
		"extramile":   "on",
		"mcp":         "linear(12) github(8)",
		"nudge":       "wrapup",
	}
}

var todayLine = regexp.MustCompile(`^\[today: ([^\]]+)\]`)

// normalizeToday replaces the volatile suffix's leading date stamp so
// comparisons can't flake across a midnight-UTC boundary mid-test.
func normalizeToday(t *testing.T, volatile string) string {
	t.Helper()
	m := todayLine.FindStringSubmatch(volatile)
	if m == nil {
		t.Fatalf("volatile does not begin with a [today: …] stamp: %q", volatile[:min(len(volatile), 80)])
	}
	if _, err := time.Parse("2 January 2006", m[1]); err != nil {
		t.Fatalf("today stamp %q does not parse as %q: %v", m[1], "2 January 2006", err)
	}
	return todayLine.ReplaceAllString(volatile, "[today: X]")
}

// TestComposeAllModes: every mode on the surface composes; the surface is
// explicit (unknown modes error); needsRoot modes demand facts.root.
func TestComposeAllModes(t *testing.T) {
	if len(codeModes) != 25 {
		t.Fatalf("codeModes has %d modes, want 25 — update the parity/compose tests with the surface", len(codeModes))
	}
	for mode := range codeModes {
		stable, volatile, err := Compose(mode, factsFull(), "turn-scoped extra", "glm-5p2", false)
		if err != nil {
			t.Errorf("Compose(%s): %v", mode, err)
			continue
		}
		if stable == "" {
			t.Errorf("Compose(%s): empty stable prefix", mode)
		}
		normalizeToday(t, volatile) // every mode's volatile leads with the date stamp
		if !strings.HasSuffix(volatile, "turn-scoped extra") {
			t.Errorf("Compose(%s): extra must be the LAST volatile block", mode)
		}
	}
	if _, _, err := Compose("bogus", factsFull(), "", "", false); err == nil {
		t.Error("unknown mode must error, not best-effort compose")
	}
	for mode := range needsRoot {
		if _, _, err := Compose(mode, map[string]string{}, "", "", false); err == nil || !strings.Contains(err.Error(), "facts.root") {
			t.Errorf("Compose(%s) without root: want facts.root error, got %v", mode, err)
		}
	}
}

// TestFactsLandWhereTheyBelong pins each fact key to its observable home:
// environment/body facts in the STABLE prefix, per-turn signals in VOLATILE.
func TestFactsLandWhereTheyBelong(t *testing.T) {
	stableChecks := []struct{ mode, needle string }{
		{"chat", "/repo"},                                // root
		{"chat", "Platform: darwin"},                     // platform
		{"chat", "zsh"},                                  // shell
		{"chat", "Subsystems: alpha, beta"},              // overview
		{"exec", `ContextPack:` + "\n" + `{"files":[]}`}, // pack
		{"apply", "APPROVED PLAN:\n1. do the thing"},     // plan
		{"scout", "the billing subsystem"},               // scope
	}
	for _, c := range stableChecks {
		stable, volatile, err := Compose(c.mode, factsFull(), "", "", false)
		if err != nil {
			t.Fatalf("Compose(%s): %v", c.mode, err)
		}
		if !strings.Contains(stable, c.needle) {
			t.Errorf("%s: stable prefix missing %q", c.mode, c.needle)
		}
		if strings.Contains(volatile, c.needle) {
			t.Errorf("%s: %q leaked into the volatile suffix", c.mode, c.needle)
		}
	}
	// Per-turn signals ride VOLATILE (never the cached prefix), in fixed order.
	stable, volatile, err := Compose("chat", factsFull(), "the extra", "", false)
	if err != nil {
		t.Fatal(err)
	}
	wantOrder := []string{
		"[today: ",
		"[interaction signal — repair mode]",
		"[voice — tone only]",
		"[extra mile",
		"[mcp servers connected: linear(12) github(8)]",
		"[Write the FINAL plan NOW", // nudge=wrapup
		"the extra",
	}
	last := -1
	for _, needle := range wantOrder {
		i := strings.Index(volatile, needle)
		if i < 0 {
			t.Fatalf("volatile missing %q\nvolatile: %s", needle, volatile)
		}
		if i < last {
			t.Fatalf("volatile block %q out of order", needle)
		}
		last = i
		if strings.Contains(stable, needle) {
			t.Errorf("volatile block %q leaked into the stable prefix (cache-buster)", needle)
		}
	}
}

// TestVolatileGating: personality / extra-mile / mcp only ride their gated
// modes; room guidance and nudges are ungated.
func TestVolatileGating(t *testing.T) {
	for mode, want := range map[string]bool{"chat": true, "plan": true, "recap": true, "classify": false, "reflect": false, "compact": false} {
		_, volatile, err := Compose(mode, factsFull(), "", "", false)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(volatile, "[voice — tone only]"); got != want {
			t.Errorf("%s: personality gating = %v, want %v", mode, got, want)
		}
	}
	for mode, want := range map[string]bool{"chat": true, "exec": true, "plan": true, "apply": true, "recap": false, "next": false, "classify": false} {
		_, volatile, err := Compose(mode, factsFull(), "", "", false)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(volatile, "[extra mile"); got != want {
			t.Errorf("%s: extra-mile gating = %v, want %v", mode, got, want)
		}
		if got := strings.Contains(volatile, "[mcp servers connected"); got != want {
			t.Errorf("%s: mcp gating = %v, want %v", mode, got, want)
		}
	}
	// Room + nudge ride even structured modes.
	_, volatile, err := Compose("classify", factsFull(), "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(volatile, "[interaction signal — repair mode]") {
		t.Error("room guidance must be ungated")
	}
	if !strings.Contains(volatile, "[Write the FINAL plan NOW") {
		t.Error("nudge must be ungated")
	}
	// nudge=force selects the other nudge.
	f := factsFull()
	f["nudge"] = "force"
	_, volatile, err = Compose("plan", f, "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(volatile, "[Your previous turn did NOT produce a plan") {
		t.Error("nudge=force must select planForce")
	}
	// Custom personality: unknown spec is a user-authored voice, verbatim, guarded.
	f = factsFull()
	f["personality"] = "Talk like a pirate."
	_, volatile, err = Compose("chat", f, "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(volatile, "[voice — tone only] Talk like a pirate."+personalityGuard) {
		t.Error("custom personality must ride verbatim inside the guarded envelope")
	}
}

// TestStablePrefixByteStable: identical inputs must render byte-identical
// output across repeated calls, for EVERY mode — the stable prefix is the
// 1h-cache key, so a single wandering byte (map-iteration order, time, rand)
// is a silent cache-buster. The volatile suffix must be equally deterministic
// modulo its date stamp.
func TestStablePrefixByteStable(t *testing.T) {
	for mode := range codeModes {
		firstStable, firstVolatile := "", ""
		for i := 0; i < 25; i++ {
			stable, volatile, err := Compose(mode, factsFull(), "extra text", "glm-5p2", false)
			if err != nil {
				t.Fatalf("Compose(%s): %v", mode, err)
			}
			volatile = normalizeToday(t, volatile)
			if i == 0 {
				firstStable, firstVolatile = stable, volatile
				continue
			}
			if stable != firstStable {
				t.Fatalf("%s: stable prefix changed between identical renders (iteration %d) — cache-buster", mode, i)
			}
			if volatile != firstVolatile {
				t.Fatalf("%s: volatile suffix changed between identical renders (iteration %d)", mode, i)
			}
		}
	}
}

// TestDelegateDoctrineNotInCompose: the cheap-lane delegation fragment is
// ROUTING-owned prose — it lives with the routing decision, which is the
// Runner's selection policy now (cli/internal/llm appends it post-resolution,
// when it knows the selected lane). Compose must never emit it: it would ride
// every mode blindly, cheap lane or not.
func TestDelegateDoctrineNotInCompose(t *testing.T) {
	for _, mode := range []string{"chat", "exec"} {
		for _, model := range []string{"", "glm-5p2", "kimi-k2", "gpt-5.6-sol", "claude-opus-4-6"} {
			for _, pinned := range []bool{false, true} {
				stable, volatile, err := Compose(mode, factsFull(), "", model, pinned)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(stable, "Match the work to the model") || strings.Contains(volatile, "Match the work to the model") {
					t.Fatalf("%s/%s/pinned=%v: delegateDoctrine leaked into compose — it is appended by the Runner's selection policy, never here", mode, model, pinned)
				}
			}
		}
	}
}
