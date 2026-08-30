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

// setupPersonalHome isolates HOME/XDG_CONFIG_HOME so a test's Personal Agents
// never touch the real ~/.memcode or ~/.config/memcode.
func setupPersonalHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))
	return home
}

// call runs one pa_* tool exactly as the cockpit would — this IS the
// interface now (see cmd/personal.go): there is no CLI subcommand path to
// fall back to, so every test here goes through personalExecute.
func call(t *testing.T, name string, in map[string]any) string {
	t.Helper()
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := personalExecute(context.Background(), name, b)
	if err != nil {
		t.Fatalf("%s(%v): %v", name, in, err)
	}
	return out
}

func TestPaCreateListPauseResumeStopDelete(t *testing.T) {
	home := setupPersonalHome(t)
	call(t, tools.PaCreate, map[string]any{"agent": "test-agent", "objective": "Maintain an arbitrary outcome"})

	cfg, err := gwconfig.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Agents["test-agent"].Kind != "personal" {
		t.Fatalf("agent=%+v", cfg.Agents["test-agent"])
	}
	if _, err := os.Stat(filepath.Join(home, ".memcode", "agents", "test-agent", "personal.db")); err != nil {
		t.Fatal(err)
	}

	call(t, tools.PaLifecycle, map[string]any{"agent": "test-agent", "action": "pause"})
	call(t, tools.PaLifecycle, map[string]any{"agent": "test-agent", "action": "resume"})
	call(t, tools.PaLifecycle, map[string]any{"agent": "test-agent", "action": "stop"})

	out := call(t, tools.PaOverview, map[string]any{})
	if !strings.Contains(out, "test-agent") {
		t.Fatalf("overview missing agent: %q", out)
	}

	call(t, tools.PaLifecycle, map[string]any{"agent": "test-agent", "action": "delete"})
	if _, err := os.Stat(filepath.Join(home, ".memcode", "agents", "test-agent")); err != nil {
		t.Fatal("non-destructive delete removed home")
	}
}

func TestPaPolicyLifecycleBlocksThenApproves(t *testing.T) {
	setupPersonalHome(t)
	call(t, tools.PaCreate, map[string]any{"agent": "pa", "objective": "Keep things tidy"})

	// No policy yet → wake is blocked.
	out := call(t, tools.PaWake, map[string]any{"agent": "pa"})
	if !strings.Contains(out, "blocked") {
		t.Fatalf("expected blocked wake, got %q", out)
	}

	policy := map[string]any{
		"objective_scope": "primary", "consequence_classes": []string{"observe", "local_mutation"},
		"max_seconds": 300, "max_actions_per_period": 8, "max_delegation_depth": 1,
	}
	pb, _ := json.Marshal(policy)
	out = call(t, tools.PaPolicy, map[string]any{"agent": "pa", "action": "stage", "document": string(pb)})
	if !strings.Contains(out, "Draft policy v1 staged") {
		t.Fatalf("stage=%q", out)
	}
	home, _ := gwconfig.AgentHome("pa")
	entries, err := os.ReadDir(filepath.Join(home, "policies"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("policies dir: %v %v", entries, err)
	}
	hash := strings.TrimSuffix(entries[0].Name(), ".json")

	out = call(t, tools.PaPolicy, map[string]any{"agent": "pa", "action": "approve", "hash": hash[:12]})
	if !strings.Contains(out, "Approved policy") {
		t.Fatalf("approve=%q", out)
	}
	out = call(t, tools.PaPolicy, map[string]any{"agent": "pa", "action": "show"})
	if !strings.Contains(out, "approved policy v1") {
		t.Fatalf("show=%q", out)
	}

	// Config mirror actually reflects the approved policy.
	mirrored, err := os.ReadFile(filepath.Join(home, "policy.yaml"))
	if err != nil || !strings.Contains(string(mirrored), "approved: true") {
		t.Fatalf("policy.yaml mirror missing approval: %v %q", err, mirrored)
	}
}

func TestPaResourceAndTrigger(t *testing.T) {
	home := setupPersonalHome(t)
	call(t, tools.PaCreate, map[string]any{"agent": "pa2", "objective": "Watch a folder"})

	grant := filepath.Join(home, "watch")
	if err := os.MkdirAll(grant, 0o755); err != nil {
		t.Fatal(err)
	}
	// No type, no mode — the common case a person would actually ask for.
	out := call(t, tools.PaResource, map[string]any{"agent": "pa2", "action": "grant", "locator": grant})
	if !strings.Contains(out, "Granted filesystem") || !strings.Contains(out, "(read)") {
		t.Fatalf("grant=%q", out)
	}
	out = call(t, tools.PaResource, map[string]any{"agent": "pa2", "action": "list"})
	if !strings.Contains(out, "filesystem") {
		t.Fatalf("list=%q", out)
	}

	out = call(t, tools.PaTrigger, map[string]any{"agent": "pa2", "action": "add", "kind": "interval", "spec": "30m"})
	if !strings.Contains(out, "next wake") {
		t.Fatalf("trigger add=%q", out)
	}
	out = call(t, tools.PaTrigger, map[string]any{"agent": "pa2", "action": "list"})
	if !strings.Contains(out, "interval") {
		t.Fatalf("trigger list=%q", out)
	}

	// Revoke by parsing the id out of the list output.
	out = call(t, tools.PaResource, map[string]any{"agent": "pa2", "action": "list"})
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
	call(t, tools.PaResource, map[string]any{"agent": "pa2", "action": "revoke", "id": resID})
	out = call(t, tools.PaResource, map[string]any{"agent": "pa2", "action": "list"})
	if !strings.Contains(out, "[revoked]") {
		t.Fatalf("expected revoked: %q", out)
	}

	// resources.yaml mirror reflects the revoke.
	agentHome, _ := gwconfig.AgentHome("pa2")
	mirrored, err := os.ReadFile(filepath.Join(agentHome, "resources.yaml"))
	if err != nil || !strings.Contains(string(mirrored), "revoked") {
		t.Fatalf("resources.yaml mirror missing revoke: %v %q", err, mirrored)
	}
}

func TestPaDoctorAndOverview(t *testing.T) {
	setupPersonalHome(t)
	call(t, tools.PaCreate, map[string]any{"agent": "cock", "objective": "Tidy my notes"})

	out := call(t, tools.PaOverview, map[string]any{})
	if !strings.Contains(out, "cock") {
		t.Fatalf("overview=%q", out)
	}
	out = call(t, tools.PaObjective, map[string]any{"agent": "cock", "action": "show"})
	if !strings.Contains(out, "Tidy my notes") {
		t.Fatalf("objective=%q", out)
	}
	out = call(t, tools.PaDoctor, map[string]any{"agent": "cock"})
	if !strings.Contains(out, "objective") || !strings.Contains(out, "sandbox") {
		t.Fatalf("doctor=%q", out)
	}
	out = call(t, tools.PaInbox, map[string]any{"agent": "cock"})
	if !strings.Contains(out, "inbox empty") {
		t.Fatalf("inbox=%q", out)
	}
}
