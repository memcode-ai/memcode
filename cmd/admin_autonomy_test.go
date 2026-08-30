package cmd

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/memcode-ai/memcode/internal/agent/tools"
	gwconfig "github.com/memcode-ai/memcode/internal/gateway/config"
)

// setupAgentHome isolates HOME/XDG_CONFIG_HOME so a test's agents never touch
// the real ~/.memcode or ~/.config/memcode.
func setupAgentHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))
	return home
}

// admin runs one gw_* tool exactly as the admin cockpit would. This IS the
// interface — there is no CLI subcommand path and no second cockpit — so every
// test here goes through adminExecute.
func admin(t *testing.T, name string, in map[string]any) string {
	t.Helper()
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := adminExecute(context.Background(), name, b)
	if err != nil {
		t.Fatalf("%s(%v): %v", name, in, err)
	}
	return out
}

func adminErr(t *testing.T, name string, in map[string]any) error {
	t.Helper()
	b, _ := json.Marshal(in)
	_, err := adminExecute(context.Background(), name, b)
	return err
}

// Objective and autonomy are separate grants. Creating an agent with an
// objective must NOT make it autonomous — that was the original design mistake
// and the thing most likely to silently regress.
func TestObjectiveDoesNotImplyAutonomy(t *testing.T) {
	setupAgentHome(t)
	out := admin(t, tools.GwAgent, map[string]any{"action": "add", "name": "jobhunt", "objective": "Find backend roles"})
	if !strings.Contains(out, "NOT yet autonomous") {
		t.Fatalf("add-with-objective should say autonomy is a separate grant: %q", out)
	}
	cfg, err := gwconfig.Load()
	if err != nil {
		t.Fatal(err)
	}
	a := cfg.Agents["jobhunt"]
	if a.Objective == "" {
		t.Fatal("objective not stored")
	}
	if a.Autonomous || a.Unattended() {
		t.Fatalf("agent became autonomous from an objective alone: %+v", a)
	}

	admin(t, tools.GwAgent, map[string]any{"action": "autonomous", "name": "jobhunt", "autonomous": "true"})
	cfg, _ = gwconfig.Load()
	if !cfg.Agents["jobhunt"].Unattended() {
		t.Fatal("explicit grant did not make the agent autonomous")
	}

	// Anything but an explicit yes revokes — authority must not be granted by typo.
	admin(t, tools.GwAgent, map[string]any{"action": "autonomous", "name": "jobhunt", "autonomous": "maybe"})
	cfg, _ = gwconfig.Load()
	if cfg.Agents["jobhunt"].Autonomous {
		t.Fatal("a non-affirmative value granted autonomy")
	}
}

// The other half of the orthogonality: unattended with no objective at all.
func TestAutonomousWithoutObjective(t *testing.T) {
	setupAgentHome(t)
	admin(t, tools.GwAgent, map[string]any{"action": "add", "name": "digest"})
	admin(t, tools.GwAgent, map[string]any{"action": "autonomous", "name": "digest", "autonomous": "true"})
	cfg, _ := gwconfig.Load()
	a := cfg.Agents["digest"]
	if !a.Unattended() {
		t.Fatal("scheduled agent without an objective should still be governed as unattended")
	}
	if a.Objective != "" {
		t.Fatal("objective invented")
	}
	// It has nothing to advance, so an on-demand wake is refused rather than
	// making something up.
	if err := adminErr(t, tools.GwWake, map[string]any{"agent": "digest"}); err == nil {
		t.Fatal("wake without an objective should be refused")
	}
}

func TestAgentLifecycleAndPause(t *testing.T) {
	home := setupAgentHome(t)
	admin(t, tools.GwAgent, map[string]any{"action": "add", "name": "test-agent", "objective": "Maintain an outcome"})
	if _, err := os.Stat(filepath.Join(home, ".memcode", "agents", "test-agent")); err != nil {
		// The home is created lazily on first store use, not at add time.
		_ = err
	}
	admin(t, tools.GwAgent, map[string]any{"action": "pause", "name": "test-agent"})
	cfg, _ := gwconfig.Load()
	if !cfg.Agents["test-agent"].Paused {
		t.Fatal("pause not recorded")
	}
	admin(t, tools.GwAgent, map[string]any{"action": "resume", "name": "test-agent"})
	cfg, _ = gwconfig.Load()
	if cfg.Agents["test-agent"].Paused {
		t.Fatal("resume not recorded")
	}

	out := admin(t, tools.GwOverview, nil)
	if !strings.Contains(out, "test-agent") || !strings.Contains(out, "objective=") {
		t.Fatalf("overview missing agent or objective: %q", out)
	}

	admin(t, tools.GwAgent, map[string]any{"action": "remove", "name": "test-agent"})
	cfg, _ = gwconfig.Load()
	if _, ok := cfg.Agents["test-agent"]; ok {
		t.Fatal("agent not removed")
	}
}

func TestPolicyLifecycleBlocksThenApproves(t *testing.T) {
	setupAgentHome(t)
	admin(t, tools.GwAgent, map[string]any{"action": "add", "name": "pa", "objective": "Keep things tidy"})

	// No policy yet → an on-demand wake is blocked before any model is built.
	out := admin(t, tools.GwWake, map[string]any{"agent": "pa"})
	if !strings.Contains(out, "blocked") {
		t.Fatalf("expected a blocked wake without a policy, got %q", out)
	}

	policy := map[string]any{
		"objective_scope": "primary", "consequence_classes": []string{"observe", "local_mutation"},
		"max_seconds": 300, "max_actions_per_period": 8, "max_delegation_depth": 1,
	}
	pb, _ := json.Marshal(policy)
	out = admin(t, tools.GwPolicy, map[string]any{"agent": "pa", "action": "stage", "document": string(pb)})
	if !strings.Contains(out, "Draft policy v1 staged") {
		t.Fatalf("stage=%q", out)
	}
	agentHome, _ := gwconfig.AgentHome("pa")
	entries, err := os.ReadDir(filepath.Join(agentHome, "policies"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("policies dir: %v %v", entries, err)
	}
	hash := strings.TrimSuffix(entries[0].Name(), ".json")

	out = admin(t, tools.GwPolicy, map[string]any{"agent": "pa", "action": "approve", "hash": hash[:12]})
	if !strings.Contains(out, "Approved policy") {
		t.Fatalf("approve=%q", out)
	}
	out = admin(t, tools.GwPolicy, map[string]any{"agent": "pa", "action": "show"})
	if !strings.Contains(out, "approved policy v1") {
		t.Fatalf("show=%q", out)
	}
	mirrored, err := os.ReadFile(filepath.Join(agentHome, "config.yaml"))
	if err != nil || !strings.Contains(string(mirrored), "approved: true") {
		t.Fatalf("config.yaml mirror missing approval: %v %q", err, mirrored)
	}
}

func TestGrantAndRevoke(t *testing.T) {
	home := setupAgentHome(t)
	admin(t, tools.GwAgent, map[string]any{"action": "add", "name": "pa2", "objective": "Watch a folder"})
	grant := filepath.Join(home, "watch")
	if err := os.MkdirAll(grant, 0o755); err != nil {
		t.Fatal(err)
	}
	// No type, no mode — the common case is just a path.
	out := admin(t, tools.GwGrant, map[string]any{"agent": "pa2", "action": "grant", "locator": grant})
	if !strings.Contains(out, "Granted filesystem") || !strings.Contains(out, "(read)") {
		t.Fatalf("grant=%q", out)
	}
	out = admin(t, tools.GwGrant, map[string]any{"agent": "pa2", "action": "list"})
	var resID string
	for _, line := range strings.Split(out, "\n") {
		if i := strings.Index(line, ":"); i > 0 {
			resID = line[:i]
			break
		}
	}
	if resID == "" {
		t.Fatalf("no resource id in %q", out)
	}
	admin(t, tools.GwGrant, map[string]any{"agent": "pa2", "action": "revoke", "id": resID})
	out = admin(t, tools.GwGrant, map[string]any{"agent": "pa2", "action": "list"})
	if !strings.Contains(out, "[revoked]") {
		t.Fatalf("expected revoked: %q", out)
	}
	agentHome, _ := gwconfig.AgentHome("pa2")
	mirrored, err := os.ReadFile(filepath.Join(agentHome, "config.yaml"))
	if err != nil || !strings.Contains(string(mirrored), "revoked") {
		t.Fatalf("config.yaml mirror missing revoke: %v %q", err, mirrored)
	}
}

// An autonomous agent's recurring cadence is an ORDINARY schedule delivering to
// the agent itself — there is no second scheduler.
func TestScheduleDefaultsToAgentRouteForAutonomousAgent(t *testing.T) {
	setupAgentHome(t)
	admin(t, tools.GwAgent, map[string]any{"action": "add", "name": "waker", "objective": "do a thing"})
	admin(t, tools.GwAgent, map[string]any{"action": "autonomous", "name": "waker", "autonomous": "true"})
	admin(t, tools.GwSchedule, map[string]any{
		"action": "add", "name": "waker-cadence", "every": "6h",
		"task": "Advance the objective.", "agent": "waker",
	})
	cfg, _ := gwconfig.Load()
	var found bool
	for _, sc := range cfg.Schedules {
		if sc.Name == "waker-cadence" {
			found = true
			if sc.DeliverTo != "agent:waker" {
				t.Fatalf("deliver_to = %q, want agent:waker", sc.DeliverTo)
			}
		}
	}
	if !found {
		t.Fatal("schedule not added")
	}
}

func TestDoctorAndInbox(t *testing.T) {
	setupAgentHome(t)
	admin(t, tools.GwAgent, map[string]any{"action": "add", "name": "cock", "objective": "Tidy my notes"})
	out := admin(t, tools.GwDoctor, map[string]any{"agent": "cock"})
	for _, want := range []string{"objective", "autonomous", "sandbox", "approved policy"} {
		if !strings.Contains(out, want) {
			t.Fatalf("doctor missing %q: %q", want, out)
		}
	}
	out = admin(t, tools.GwInbox, map[string]any{"agent": "cock"})
	if !strings.Contains(out, "inbox empty") {
		t.Fatalf("inbox=%q", out)
	}
}
