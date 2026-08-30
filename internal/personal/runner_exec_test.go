package personal

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/memcode-ai/memcode/internal/llm"
	"github.com/memcode-ai/memcode/internal/provider"
	"github.com/memcode-ai/memcode/internal/wire"
)

// fakeProv is a scripted ModelProvider driving the executive loop deterministically.
type fakeProv struct {
	steps []wire.Response
	calls int
}

func (f *fakeProv) Complete(ctx context.Context, r wire.Request) (wire.Response, error) {
	if f.calls >= len(f.steps) {
		return wire.Response{StopReason: "end_turn", Blocks: []wire.Block{wire.TextBlock("done")}}, nil
	}
	resp := f.steps[f.calls]
	f.calls++
	return resp, nil
}
func (f *fakeProv) Endpoint() (provider.Endpoint, bool) { return provider.Endpoint{}, false }

func toolUse(id, name string, input any) wire.Block {
	b, _ := json.Marshal(input)
	return wire.Block{Type: "tool_use", ID: id, Name: name, Input: b}
}

func newTestExecutive(t *testing.T, prov provider.ModelProvider) (*Executive, *Store, string) {
	t.Helper()
	ctx := context.Background()
	home := t.TempDir()
	st, err := Open(ctx, home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.CreateObjective(ctx, Objective{ID: "primary", Description: "Keep dependencies fresh", SuccessCriteria: "no outdated deps", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	ex := &Executive{Store: st, Home: home, AgentID: "tester", Runner: llm.NewRunner(prov)}
	return ex, st, home
}

func approveTestPolicy(t *testing.T, st *Store) {
	t.Helper()
	ctx := context.Background()
	doc := DelegationPolicy{ObjectiveScope: "primary", ConsequenceClasses: []ConsequenceClass{Observe, LocalMutation}, MaxSeconds: 300, MaxActionsPerPeriod: 8}
	canon, hash, err := CanonicalPolicy(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.InsertPolicy(ctx, Policy{ID: "p1", ObjectiveID: "primary", Version: 1, Document: canon, Hash: hash, Status: "draft"}); err != nil {
		t.Fatal(err)
	}
	if err := st.ApprovePolicy(ctx, hash); err != nil {
		t.Fatal(err)
	}
}

func TestExecutiveBlocksWithoutPolicy(t *testing.T) {
	prov := &fakeProv{}
	ex, st, _ := newTestExecutive(t, prov)
	out, err := ex.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "blocked" || !strings.Contains(out.Report, "no approved policy") {
		t.Fatalf("expected blocked, got %+v", out)
	}
	if prov.calls != 0 {
		t.Fatal("LLM was called despite missing policy — fail-closed violated")
	}
	// No run should have been created.
	runs, _ := st.ListRuns(context.Background(), "primary", 10)
	if len(runs) != 0 {
		t.Fatalf("run created without policy: %v", runs)
	}
}

func TestExecutiveRunsAndJournals(t *testing.T) {
	prov := &fakeProv{steps: []wire.Response{
		{StopReason: "tool_use", Blocks: []wire.Block{toolUse("t1", "subgoal_update", map[string]any{"id": "sg1", "description": "scan deps", "status": "active", "priority": 5})}},
		{StopReason: "tool_use", Blocks: []wire.Block{toolUse("t2", "note_fact", map[string]any{"key": "deps.outdated", "value": "3", "source": "scan", "confirmed": true})}},
		{StopReason: "tool_use", Blocks: []wire.Block{toolUse("t3", "report", map[string]any{"summary": "found 3 outdated deps"})}},
	}}
	ex, st, _ := newTestExecutive(t, prov)
	approveTestPolicy(t, st)
	out, err := ex.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" {
		t.Fatalf("status=%s report=%s", out.Status, out.Report)
	}
	if !strings.Contains(out.Report, "outdated") {
		t.Fatalf("report=%q", out.Report)
	}
	// Subgoal + fact recorded.
	subs, _ := st.ListSubgoals(context.Background(), "primary")
	if len(subs) != 1 || subs[0].Description != "scan deps" {
		t.Fatalf("subgoals=%v", subs)
	}
	facts, _ := st.ListFacts(context.Background(), "primary")
	if len(facts) != 1 || facts[0].Key != "deps.outdated" {
		t.Fatalf("facts=%v", facts)
	}
	// Run recorded completed.
	runs, _ := st.ListRuns(context.Background(), "primary", 10)
	if len(runs) != 1 || runs[0].Status != "completed" {
		t.Fatalf("runs=%+v", runs)
	}
}

func TestExecutiveSuspendsAndResumes(t *testing.T) {
	prov := &fakeProv{steps: []wire.Response{
		{StopReason: "tool_use", Blocks: []wire.Block{toolUse("t1", "ask_user", map[string]any{"question": "proceed with upgrade?", "context": "3 deps outdated"})}},
	}}
	ex, st, home := newTestExecutive(t, prov)
	approveTestPolicy(t, st)
	ctx := context.Background()
	out, err := ex.RunOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "suspended" || out.InteractionID == "" {
		t.Fatalf("out=%+v", out)
	}
	// Interaction is pending.
	in, ok, err := st.GetInteraction(ctx, out.InteractionID)
	if err != nil || !ok || in.Status != "pending" {
		t.Fatalf("interaction=%+v ok=%v err=%v", in, ok, err)
	}
	// Inbox lists it.
	pend, _ := st.PendingInteractions(ctx, "tester")
	if len(pend) != 1 || pend[0].Question != "proceed with upgrade?" {
		t.Fatalf("inbox=%v", pend)
	}
	// Continuation file exists.
	if _, err := os.Stat(filepath.Join(home, "runs", out.RunID, "suspension-"+out.InteractionID+".json")); err != nil {
		t.Fatalf("continuation missing: %v", err)
	}
	// Resume actually re-runs the model with the answer; the resumed run then
	// completes (fake provider returns report on the next turn).
	prov2 := &fakeProv{steps: []wire.Response{
		{StopReason: "tool_use", Blocks: []wire.Block{toolUse("t9", "report", map[string]any{"summary": "upgraded after approval"})}},
	}}
	ex2 := &Executive{Store: st, Home: home, AgentID: "tester", Runner: llm.NewRunner(prov2)}
	rout, err := ex2.ResumeSuspended(ctx, in, "yes, upgrade")
	if err != nil {
		t.Fatal(err)
	}
	if rout.Status != "completed" || !strings.Contains(rout.Report, "upgraded") {
		t.Fatalf("resume outcome=%+v", rout)
	}
	if prov2.calls == 0 {
		t.Fatal("resume never called the model — fake resume regression")
	}
	// Resolve after successful resume; double-resolve must fail.
	if err := st.ResolveInteraction(ctx, out.InteractionID, "yes, upgrade"); err != nil {
		t.Fatal(err)
	}
	if err := st.ResolveInteraction(ctx, out.InteractionID, "again"); err == nil {
		t.Fatal("double resolve accepted")
	}
}

func TestExecutivePolicyDeniesWrite(t *testing.T) {
	// Policy grants only Observe, no LocalMutation → write_file tool is filtered out.
	prov := &fakeProv{steps: []wire.Response{
		{StopReason: "tool_use", Blocks: []wire.Block{toolUse("t1", "report", map[string]any{"summary": "observe only"})}},
	}}
	ex, st, _ := newTestExecutive(t, prov)
	ctx := context.Background()
	doc := DelegationPolicy{ObjectiveScope: "primary", ConsequenceClasses: []ConsequenceClass{Observe}, MaxSeconds: 300, MaxActionsPerPeriod: 8}
	canon, hash, _ := CanonicalPolicy(doc)
	_ = st.InsertPolicy(ctx, Policy{ID: "p1", ObjectiveID: "primary", Version: 1, Document: canon, Hash: hash, Status: "draft"})
	_ = st.ApprovePolicy(ctx, hash)
	// write_file must not be in the allowed tool list.
	var policyDoc DelegationPolicy
	_ = json.Unmarshal(canon, &policyDoc)
	for _, d := range ex.allowedTools(policyDoc) {
		if d.Name == "write_file" {
			t.Fatal("write_file exposed without local_mutation")
		}
	}
	if _, err := ex.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestPolicyApprovalMovesObjectiveActive(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_ = st.CreateObjective(ctx, Objective{ID: "primary", Description: "x", Status: "draft"})
	approveTestPolicy(t, st)
	if err := st.SetObjectiveStatus(ctx, "primary", "active"); err != nil {
		t.Fatal(err)
	}
	p, ok, _ := st.ApprovedPolicy(ctx, "primary")
	if !ok || p.Status != "approved" || p.ApprovedAt == nil {
		t.Fatalf("policy=%+v", p)
	}
	// Second draft supersedes on approval.
	doc2 := DelegationPolicy{ObjectiveScope: "primary", ConsequenceClasses: []ConsequenceClass{Observe}}
	canon2, hash2, _ := CanonicalPolicy(doc2)
	_ = st.InsertPolicy(ctx, Policy{ID: "p2", ObjectiveID: "primary", Version: 2, Document: canon2, Hash: hash2, Status: "draft"})
	_ = st.ApprovePolicy(ctx, hash2)
	p, _, _ = st.ApprovedPolicy(ctx, "primary")
	if p.Version != 2 {
		t.Fatalf("expected v2 approved, got v%d", p.Version)
	}
}

func TestTriggerWakeSchedulingViaTool(t *testing.T) {
	later := time.Now().UTC().Add(30 * time.Minute)
	prov := &fakeProv{steps: []wire.Response{
		{StopReason: "tool_use", Blocks: []wire.Block{toolUse("t1", "schedule_wake", map[string]any{"at": later.Format(time.RFC3339)})}},
		{StopReason: "end_turn", Blocks: []wire.Block{wire.TextBlock("scheduled")}},
	}}
	ex, st, _ := newTestExecutive(t, prov)
	approveTestPolicy(t, st)
	out, err := ex.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if out.NextWakeAt == nil {
		t.Fatal("no next wake recorded")
	}
	trigs, _ := st.ListTriggers(context.Background())
	if len(trigs) != 1 || trigs[0].Kind != "next_wake" {
		t.Fatalf("triggers=%v", trigs)
	}
}
