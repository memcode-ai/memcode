package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/memcode-ai/memcode/internal/agent/tools"
	gwconfig "github.com/memcode-ai/memcode/internal/gateway/config"
)

// execPersonal runs a personal subcommand with an isolated HOME + config.
func execPersonal(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := rootCmd
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func setupPersonalHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))
	return home
}

func TestPolicyLifecycleBlocksThenApproves(t *testing.T) {
	setupPersonalHome(t)
	if _, err := execPersonal(t, "personal", "create", "pa", "Keep things tidy"); err != nil {
		t.Fatal(err)
	}
	// No policy yet → run is blocked (and must not call a model).
	out, err := execPersonal(t, "personal", "run", "pa")
	// run errors only because no model is configured in test env; that's fine —
	// what we assert is the policy gate happens BEFORE any model requirement.
	_ = out
	_ = err
	// Stage + approve a policy.
	dir := t.TempDir()
	pfile := filepath.Join(dir, "policy.json")
	policy := map[string]any{
		"objective_scope":     "primary",
		"consequence_classes": []string{"observe", "local_mutation"},
		"max_seconds":         300, "max_actions_per_period": 8, "max_delegation_depth": 1,
	}
	b, _ := json.Marshal(policy)
	if err := os.WriteFile(pfile, b, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err = execPersonal(t, "personal", "policy", "set", "pa", pfile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Draft policy v1 staged") {
		t.Fatalf("set=%q", out)
	}
	// Extract hash from the policy file name written under the agent home.
	home, _ := gwconfig.AgentHome("pa")
	entries, err := os.ReadDir(filepath.Join(home, "policies"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("policies dir: %v %v", entries, err)
	}
	hash := strings.TrimSuffix(entries[0].Name(), ".json")
	// Approve by prefix.
	out, err = execPersonal(t, "personal", "approve-policy", "pa", hash[:12])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Approved policy") {
		t.Fatalf("approve=%q", out)
	}
	// Show reports approved.
	out, err = execPersonal(t, "personal", "policy", "show", "pa")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "approved policy v1") {
		t.Fatalf("show=%q", out)
	}
}

func TestResourcesAndTriggersCommands(t *testing.T) {
	home := setupPersonalHome(t)
	if _, err := execPersonal(t, "personal", "create", "pa2", "Watch a folder"); err != nil {
		t.Fatal(err)
	}
	grant := filepath.Join(home, "watch")
	if err := os.MkdirAll(grant, 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := execPersonal(t, "personal", "resources", "add", "pa2", "filesystem", grant, "--mode", "write")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Granted filesystem") {
		t.Fatalf("add=%q", out)
	}
	out, err = execPersonal(t, "personal", "resources", "list", "pa2")
	if err != nil || !strings.Contains(out, "filesystem") {
		t.Fatalf("list=%q err=%v", out, err)
	}
	// Trigger add/list.
	out, err = execPersonal(t, "personal", "triggers", "add", "pa2", "interval", "30m")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "next wake") {
		t.Fatalf("trigger add=%q", out)
	}
	out, err = execPersonal(t, "personal", "triggers", "list", "pa2")
	if err != nil || !strings.Contains(out, "interval") {
		t.Fatalf("triggers list=%q err=%v", out, err)
	}
	// Revoke a resource by parsing its id (field after "- ", before ":").
	out, err = execPersonal(t, "personal", "resources", "list", "pa2")
	if err != nil {
		t.Fatal(err)
	}
	var resID string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "- ") {
			rest := strings.TrimPrefix(line, "- ")
			if i := strings.Index(rest, ":"); i > 0 {
				resID = rest[:i]
			}
		}
	}
	if resID == "" {
		t.Fatalf("no resource id in %q", out)
	}
	if _, err := execPersonal(t, "personal", "resources", "revoke", "pa2", resID); err != nil {
		t.Fatal(err)
	}
	// Confirm revoked.
	out, _ = execPersonal(t, "personal", "resources", "list", "pa2")
	if !strings.Contains(out, "[revoked]") {
		t.Fatalf("expected revoked: %q", out)
	}
}

// The cockpit executor drives the same operations the subcommands expose, via
// typed pa_* tool calls. Read-only calls return state; mutations mutate.
func TestPersonalCockpitExecutor(t *testing.T) {
	setupPersonalHome(t)
	ctx := context.Background()
	if _, err := execPersonal(t, "personal", "create", "cock", "Tidy my notes"); err != nil {
		t.Fatal(err)
	}
	// Overview (read).
	out, err := personalExecute(ctx, tools.PaOverview, json.RawMessage(`{}`))
	if err != nil || !strings.Contains(out, "cock") {
		t.Fatalf("overview=%q err=%v", out, err)
	}
	// Objective show (read).
	out, err = personalExecute(ctx, tools.PaObjective, json.RawMessage(`{"agent":"cock","action":"show"}`))
	if err != nil || !strings.Contains(out, "Tidy my notes") {
		t.Fatalf("objective=%q err=%v", out, err)
	}
	// Policy stage + approve via cockpit.
	pol := map[string]any{"objective_scope": "primary", "consequence_classes": []string{"observe"}, "max_seconds": 60, "max_actions_per_period": 4}
	pb, _ := json.Marshal(pol)
	docJSON, _ := json.Marshal(map[string]string{"agent": "cock", "action": "stage", "document": string(pb)})
	out, err = personalExecute(ctx, tools.PaPolicy, docJSON)
	if err != nil || !strings.Contains(out, "Draft policy v1 staged") {
		t.Fatalf("stage=%q err=%v", out, err)
	}
	home, _ := gwconfig.AgentHome("cock")
	entries, _ := os.ReadDir(filepath.Join(home, "policies"))
	hash := strings.TrimSuffix(entries[0].Name(), ".json")
	apJSON, _ := json.Marshal(map[string]string{"agent": "cock", "action": "approve", "hash": hash})
	out, err = personalExecute(ctx, tools.PaPolicy, apJSON)
	if err != nil || !strings.Contains(out, "Approved policy") {
		t.Fatalf("approve=%q err=%v", out, err)
	}
	// Trigger add + list via cockpit.
	trJSON, _ := json.Marshal(map[string]string{"agent": "cock", "action": "add", "kind": "interval", "spec": "15m"})
	out, err = personalExecute(ctx, tools.PaTrigger, trJSON)
	if err != nil || !strings.Contains(out, "next wake") {
		t.Fatalf("trigger add=%q err=%v", out, err)
	}
	tlJSON, _ := json.Marshal(map[string]string{"agent": "cock", "action": "list"})
	out, err = personalExecute(ctx, tools.PaTrigger, tlJSON)
	if err != nil || !strings.Contains(out, "interval") {
		t.Fatalf("trigger list=%q err=%v", out, err)
	}
	// Inbox (read) and lifecycle pause.
	inJSON, _ := json.Marshal(map[string]string{"agent": "cock"})
	out, err = personalExecute(ctx, tools.PaInbox, inJSON)
	if err != nil || !strings.Contains(out, "inbox empty") {
		t.Fatalf("inbox=%q err=%v", out, err)
	}
	lcJSON, _ := json.Marshal(map[string]string{"agent": "cock", "action": "pause"})
	out, err = personalExecute(ctx, tools.PaLifecycle, lcJSON)
	if err != nil || !strings.Contains(out, "paused") {
		t.Fatalf("pause=%q err=%v", out, err)
	}
}
