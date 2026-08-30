package guard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These guards protect an invariant that was violated once and cost real bugs:
// the Personal Agents subsystem grew a parallel implementation of machinery the
// ordinary agent system already had — a second cron parser, a second (and
// third) suspend/resume design, a second cockpit — and the two paths drifted.
// Fixes landed on one side and not the other. The consolidation removed the
// duplicates; these tests keep them from quietly coming back.

// goFiles walks the module's own Go sources, skipping vendored forks, tests,
// and this guard package itself.
func goFiles(t *testing.T, skipTests bool) map[string]string {
	t.Helper()
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root = filepath.Dir(filepath.Dir(root)) // internal/guard -> module root
	out := map[string]string{}
	err = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // unreadable paths (symlinked node_modules) are not our concern
		}
		if info.IsDir() {
			switch info.Name() {
			case "node_modules", "forks", ".git", ".memcode", "desktop":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") {
			return nil
		}
		if skipTests && strings.HasSuffix(p, "_test.go") {
			return nil
		}
		if strings.Contains(p, "internal/guard/") {
			return nil
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		out[rel] = string(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) == 0 {
		t.Fatal("walked no Go files — the guard would pass vacuously")
	}
	return out
}

// TestSingleCronParser: exactly one package parses cron expressions. An
// autonomous agent's recurring cadence is an ordinary schedule, not a second
// scheduling system.
func TestSingleCronParser(t *testing.T) {
	var users []string
	for path, src := range goFiles(t, true) {
		if strings.Contains(src, "robfig/cron") {
			users = append(users, filepath.Dir(path))
		}
	}
	seen := map[string]bool{}
	var pkgs []string
	for _, d := range users {
		if !seen[d] {
			seen[d] = true
			pkgs = append(pkgs, d)
		}
	}
	// The gateway owns scheduling: the cron runner and the shared spec
	// validation both live under internal/gateway.
	for _, p := range pkgs {
		if !strings.HasPrefix(p, "internal/gateway/") {
			t.Errorf("%s parses cron — scheduling belongs to internal/gateway (one scheduler, reached via gw_schedule); a per-subsystem parser is how the two schedulers drifted apart", p)
		}
	}
}

// TestSingleSuspensionImplementation: durable suspend/resume lives in exactly
// one package. Three partial designs coexisted before this — one unused, one
// never written to, one hand-rolled and (briefly) not crash-safe.
func TestSingleSuspensionImplementation(t *testing.T) {
	const impl = "internal/agent/continuation/"
	for path, src := range goFiles(t, true) {
		if strings.HasPrefix(path, impl) {
			continue
		}
		// Marker of a bespoke continuation file format: writing a suspension
		// blob rather than going through the shared package.
		if strings.Contains(src, `"suspension-"`) || strings.Contains(src, `"tool_use_id":`) && strings.Contains(src, `"resolved"`) {
			t.Errorf("%s appears to hand-roll a suspension file format — use internal/agent/continuation instead", path)
		}
	}
}

// TestNoSecondCockpit: agents are managed through the admin surface. A second
// interactive management console means a second set of handlers, and the two
// drift (the config mirror was written on one path and not the other).
func TestNoSecondCockpit(t *testing.T) {
	for path, src := range goFiles(t, false) {
		if strings.Contains(src, "SetPersonal(") || strings.Contains(src, "personalMode") {
			t.Errorf("%s references the removed personal cockpit — agent management belongs to the admin tools (gw_*)", path)
		}
		if strings.Contains(src, `"pa_`) {
			t.Errorf("%s references a pa_* tool — those folded into the gw_* registry", path)
		}
	}
}

// TestNoAgentKind: autonomy is orthogonal settings on an agent, never a "kind"
// discriminator. A kind field is what made Personal a separate species.
//
// gwconfig.Agent.LegacyKind is the one allowed mention: it exists solely so an
// old `kind: personal` config is REJECTED with a fix rather than silently
// ignored by YAML. Behavior must never branch on it, so the check below looks
// for the branch, not the name.
func TestNoAgentKind(t *testing.T) {
	for path, src := range goFiles(t, false) {
		for _, line := range strings.Split(src, "\n") {
			if strings.Contains(line, "LegacyKind") {
				continue
			}
			if strings.Contains(line, `Kind: "personal"`) || strings.Contains(line, `Kind == "personal"`) || strings.Contains(line, `kind == "personal"`) {
				t.Errorf("%s still discriminates on an agent kind — use Agent.Autonomous / Agent.Objective:\n  %s", path, strings.TrimSpace(line))
			}
		}
	}
}
