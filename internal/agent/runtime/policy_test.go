package runtime

import (
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/agent/tools"
	"github.com/memcode-ai/memcode/internal/llm"
	"github.com/memcode-ai/memcode/internal/policy"
	"github.com/memcode-ai/memcode/internal/store"
	"github.com/memcode-ai/memcode/internal/wire"
)

// recordingProv captures the model of every request so a test can see which
// model each worker actually ran on.
type recordingProv struct{ models []string }

func (p *recordingProv) Complete(_ context.Context, r wire.Request) (wire.Response, error) {
	p.models = append(p.models, r.Pin)
	return wire.Response{StopReason: "end_turn", Model: r.Pin,
		Blocks: []wire.Block{wire.TextBlock("done")}, InputTokens: 1, OutputTokens: 1}, nil
}

func (p *recordingProv) Stream(ctx context.Context, r wire.Request, _ wire.StreamHandler) (wire.Response, error) {
	return p.Complete(ctx, r)
}

func policySession(t *testing.T, prov *recordingProv) *Session {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	root := t.TempDir()
	s := newSess(st, prov, root, "opus", permissions.ModeAuto, io.Discard)
	s.SetPin("opus", 1_000_000)
	s.SetPolicy(&policy.Resolver{Session: policy.Set{}, Workspace: policy.Set{}, User: policy.Set{}, Primary: "opus"})
	return s
}

// With no policy set, every delegated worker runs on the user's own model.
// This is the default, and it is why the policy layer changes nothing for
// someone who never configures it.
func TestWorkersInheritPrimaryByDefault(t *testing.T) {
	prov := &recordingProv{}
	s := policySession(t, prov)
	if _, err := s.spawnAgent(context.Background(), AgentSpec{Task: "look", ReadOnly: true}); err != nil {
		t.Fatal(err)
	}
	for i, m := range prov.models {
		if m != "opus" {
			t.Fatalf("worker call %d ran on %q, want the primary opus; all: %v", i, m, prov.models)
		}
	}
}

// An explicit agent.delegated policy is used by every delegated worker, and two
// workers in the same session cannot differ — nothing about an invocation can
// name a model.
func TestDelegatedPolicyAppliesToEveryWorker(t *testing.T) {
	prov := &recordingProv{}
	s := policySession(t, prov)
	s.policy.Workspace.Put(policy.AgentDelegated, "model", "haiku")

	for _, spec := range []AgentSpec{{Task: "one", ReadOnly: true}, {Task: "two"}, {Task: "three"}} {
		if _, err := s.spawnAgent(context.Background(), spec); err != nil {
			t.Fatalf("%s: %v", spec.Task, err)
		}
	}
	for i, m := range prov.models {
		if m != "haiku" {
			t.Fatalf("call %d ran on %q, want haiku; all: %v", i, m, prov.models)
		}
	}
}

// agent.explore narrows agent.delegated through DECLARED inheritance: an
// explorer takes explore's model when set, the parent's otherwise.
func TestExploreNarrowsDelegated(t *testing.T) {
	prov := &recordingProv{}
	s := policySession(t, prov)
	s.policy.Workspace.Put(policy.AgentDelegated, "model", "sonnet")
	s.policy.Workspace.Put(policy.AgentExplore, "model", "haiku")

	if _, err := s.spawnAgent(context.Background(), AgentSpec{Task: "scout", ReadOnly: true, Purpose: llm.Explore}); err != nil {
		t.Fatal(err)
	}
	if prov.models[0] != "haiku" {
		t.Fatalf("explorer ran on %q, want explore's haiku", prov.models[0])
	}
	prov.models = nil
	if _, err := s.spawnAgent(context.Background(), AgentSpec{Task: "work"}); err != nil {
		t.Fatal(err)
	}
	if prov.models[0] != "sonnet" {
		t.Fatalf("delegated worker ran on %q, want delegated's sonnet", prov.models[0])
	}
}

// The agent tool exposes NO way to name a model. This is the guard against
// re-growing per-invocation routing under a new field name — the previous
// version of that mistake was called `tier`.
func TestAgentToolCannotNameAModel(t *testing.T) {
	for _, d := range tools.Defs() {
		if d.Name != tools.Agent {
			continue
		}
		raw, err := json.Marshal(d.InputSchema)
		if err != nil {
			t.Fatal(err)
		}
		for _, banned := range []string{"model", "tier", "lane", "pin"} {
			if strings.Contains(strings.ToLower(string(raw)), `"`+banned+`"`) {
				t.Errorf("the agent tool schema exposes %q — a worker's model is POLICY, "+
					"set by the user, never chosen per invocation. Schema: %s", banned, raw)
			}
		}
		return
	}
	t.Fatal("the agent tool is not registered")
}

// The tool records an explicit instruction; the runtime then obeys it. This is
// the whole contract: instruction -> tool -> typed policy -> deterministic use.
func TestPolicyToolSetsAndPersists(t *testing.T) {
	prov := &recordingProv{}
	s := policySession(t, prov)

	res := s.policyTool(context.Background(), json.RawMessage(
		`{"action":"set","target":"agent.explore","fields":{"model":"haiku"},"scope":"workspace"}`))
	if res.isError {
		t.Fatalf("set failed: %s", resText(res))
	}
	if got := s.policy.Resolve(policy.AgentExplore).Model("model"); got != "haiku" {
		t.Fatalf("resolved explore model = %q, want haiku", got)
	}
	// It reached the workspace file, so the next session sees it.
	if got := policy.Load(policy.WorkspacePath(s.root))[policy.AgentExplore]["model"]; got != "haiku" {
		t.Fatalf("workspace file = %v, want haiku", got)
	}
	// The user's own model is untouched.
	if s.Pin() != "opus" {
		t.Fatalf("primary pin = %q — policy must never change the model the user talks to", s.Pin())
	}

	// Unset restores the inherited default.
	res = s.policyTool(context.Background(), json.RawMessage(
		`{"action":"unset","target":"agent.explore","scope":"workspace"}`))
	if res.isError {
		t.Fatalf("unset failed: %s", resText(res))
	}
	if got := s.policy.Resolve(policy.AgentExplore).Model("model"); got != "opus" {
		t.Fatalf("after unset explore = %q, want the inherited primary opus", got)
	}
}

// Bad input is refused whole — a half-applied policy is worse than a refused
// one, because the user believes it took.
func TestPolicyToolRefusesBadInput(t *testing.T) {
	prov := &recordingProv{}
	s := policySession(t, prov)
	for _, in := range []string{
		`{"action":"set","target":"agent.explore","fields":{"model":"not-a-model"}}`,
		`{"action":"set","target":"nope.nope","fields":{"model":"haiku"}}`,
		`{"action":"set","target":"plan.review","fields":{"mode":"sometimes"}}`,
		`{"action":"set","target":"agent.explore","fields":{}}`,
		`{"action":"set","target":"agent.explore","fields":{"model":"haiku"},"scope":"galaxy"}`,
		`{"action":"frobnicate"}`,
	} {
		if res := s.policyTool(context.Background(), json.RawMessage(in)); !res.isError {
			t.Errorf("%s should be refused, got %q", in, resText(res))
		}
	}
	if got := s.policy.Resolve(policy.AgentExplore).Model("model"); got != "opus" {
		t.Fatalf("a refused call changed policy to %q", got)
	}
	// A partially-valid set applies nothing.
	s.policyTool(context.Background(), json.RawMessage(
		`{"action":"set","target":"agent.explore","fields":{"model":"haiku","concurrency":999}}`))
	if got := s.policy.Resolve(policy.AgentExplore).Model("model"); got != "opus" {
		t.Fatalf("a partially-invalid set applied %q — it must apply nothing", got)
	}
}

// An operation-scoped override belongs to the plan in flight and is gone once
// that plan ends. A one-shot that outlives its operation is a bug.
func TestOperationScopedOverrideDiesWithThePlan(t *testing.T) {
	prov := &recordingProv{}
	s := policySession(t, prov)

	// Refused outside an operation — there is nothing to attach it to.
	if res := s.policyTool(context.Background(), json.RawMessage(
		`{"action":"set","target":"plan.review","fields":{"model":"haiku"},"scope":"operation"}`)); !res.isError {
		t.Fatal("operation scope with no plan in flight should be refused")
	}

	enterPlanForTest(s, "do a thing")
	if res := s.policyTool(context.Background(), json.RawMessage(
		`{"action":"set","target":"plan.review","fields":{"model":"haiku"},"scope":"operation"}`)); res.isError {
		t.Fatalf("operation scope during a plan should work: %s", resText(res))
	}
	if got := s.planCtl.PolicyOverride(string(policy.PlanReview)); got["model"] != "haiku" {
		t.Fatalf("override = %v, want haiku on this plan", got)
	}
	// Nothing was persisted anywhere.
	if len(policy.Load(policy.WorkspacePath(s.root))) != 0 {
		t.Error("an operation override must not reach the workspace file")
	}
	if got := s.policy.Resolve(policy.PlanReview).Model("model"); got == "haiku" {
		t.Error("an operation override must not leak into stored policy")
	}

	// Ending the plan drops it.
	s.ExitPlan(context.Background(), false)
	if got := s.planCtl.PolicyOverride(string(policy.PlanReview)); got != nil {
		t.Fatalf("override survived the plan: %v", got)
	}
}

// resText flattens a toolResult for assertion messages.
func resText(r toolResult) string {
	var b strings.Builder
	for _, blk := range r.blocks {
		b.WriteString(blk.Text)
	}
	return b.String()
}
