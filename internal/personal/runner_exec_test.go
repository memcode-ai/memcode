package personal

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/memcode-ai/memcode/internal/agent/continuation"
	"github.com/memcode-ai/memcode/internal/browser/broker"
	"github.com/memcode-ai/memcode/internal/jobs"
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

// testObjective stands in for gwconfig.Agent.Objective — the executive now
// reads its goal from configuration rather than the store.
const testObjective = "Keep dependencies fresh"

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
	ex := &Executive{Store: st, Home: home, AgentID: "tester", Objective: testObjective, Runner: llm.NewRunner(prov)}
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
	// A loadable continuation exists (shared continuation package, not a
	// bespoke file layout — assert through its API, not the filename).
	if _, err := continuation.Load(suspensionDir(home, out.RunID), out.InteractionID); err != nil {
		t.Fatalf("continuation missing or unloadable: %v", err)
	}
	// Resume actually re-runs the model with the answer; the resumed run then
	// completes (fake provider returns report on the next turn).
	prov2 := &fakeProv{steps: []wire.Response{
		{StopReason: "tool_use", Blocks: []wire.Block{toolUse("t9", "report", map[string]any{"summary": "upgraded after approval"})}},
	}}
	ex2 := &Executive{Store: st, Home: home, AgentID: "tester", Objective: testObjective, Runner: llm.NewRunner(prov2)}
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

// TestExecutiveDelegatesToWorker exercises delegate → check_delegate against a
// real (detached) jobs.SpawnWithSpec call. Under `go test`, the spawned "worker"
// is the test binary itself re-exec'd with flags that run zero tests (see
// jobs.isTestBinary), so it never calls jobs.Finish — the job settles at
// StatusStopped once the process exits, not StatusDone. That's enough to prove
// the wiring: an executive whose policy allows delegation actually launches a
// real, tracked, policy-scoped child process and can read its outcome back on
// a later wake, instead of failing closed or silently no-op'ing.
// TestExecutiveDelegateFailsClosedWithoutBroker proves the fail-closed
// requirement: delegating browser work when no gateway (and therefore no
// broker socket) is running must be REJECTED, not silently downgraded to
// ephemeral Chrome. No job may be spawned in this case at all.
func TestExecutiveDelegateFailsClosedWithoutBroker(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir()) // guarantees no broker socket exists here
	prov := &fakeProv{steps: []wire.Response{
		{StopReason: "tool_use", Blocks: []wire.Block{toolUse("t1", "delegate", map[string]any{
			"task": "check gmail", "expected_output": "a summary", "completion_condition": "read the inbox",
			"toolsets": []string{"browser"}, "consequences": []string{"observe"},
		})}},
		{StopReason: "tool_use", Blocks: []wire.Block{toolUse("t2", "report", map[string]any{"summary": "done"})}},
	}}
	ex, st, _ := newTestExecutive(t, prov)
	ctx := context.Background()
	doc := DelegationPolicy{ObjectiveScope: "primary", ConsequenceClasses: []ConsequenceClass{Observe}, MaxDelegationDepth: 1, MaxSeconds: 300, MaxActionsPerPeriod: 8}
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

	if _, err := ex.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	// The delegate tool call must have failed (surfaced as a tool_result error,
	// not a spawned job) — no delegation fact should exist.
	facts, err := st.ListFacts(ctx, "primary")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range facts {
		if strings.HasPrefix(f.Key, "delegation.") {
			t.Fatalf("expected no delegation to have been spawned without a broker, got fact %v", f)
		}
	}
	actions, err := st.ListActions(ctx, "primary", 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range actions {
		if a.Kind == "delegate" {
			t.Fatalf("expected no journaled delegate action without a broker, got %+v", a)
		}
	}
}

// TestExecutiveDelegateUsesExistingChromeWhenBrokerRunning proves the other
// half of the fail-closed contract: when a broker IS reachable, a bare
// "browser" toolset resolves to existing_chrome (never silently downgrades to
// ephemeral), and that mode actually rides the spawned job's SpawnSpec.
func TestExecutiveDelegateUsesExistingChromeWhenBrokerRunning(t *testing.T) {
	// Unix socket paths have a short OS limit (~104 bytes on macOS/BSD) —
	// t.TempDir() nests deep enough to blow past it, so use a short root.
	short, err := os.MkdirTemp("", "pab")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(short) })
	t.Setenv("XDG_CONFIG_HOME", short)
	sock, err := broker.SocketPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(sock), 0o700); err != nil {
		t.Fatal(err)
	}
	srv, err := broker.Serve(broker.New(), sock)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	prov := &fakeProv{steps: []wire.Response{
		{StopReason: "tool_use", Blocks: []wire.Block{toolUse("t1", "delegate", map[string]any{
			"task": "check gmail", "expected_output": "a summary", "completion_condition": "read the inbox",
			"toolsets": []string{"browser"}, "consequences": []string{"observe"},
		})}},
		{StopReason: "tool_use", Blocks: []wire.Block{toolUse("t2", "report", map[string]any{"summary": "delegated"})}},
	}}
	ex, st, _ := newTestExecutive(t, prov)
	ctx := context.Background()
	doc := DelegationPolicy{ObjectiveScope: "primary", ConsequenceClasses: []ConsequenceClass{Observe}, MaxDelegationDepth: 1, MaxSeconds: 300, MaxActionsPerPeriod: 8}
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

	if _, err := ex.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	facts, err := st.ListFacts(ctx, "primary")
	if err != nil {
		t.Fatal(err)
	}
	var jobID string
	for _, f := range facts {
		if strings.HasPrefix(f.Key, "delegation.") {
			jobID = strings.TrimPrefix(f.Key, "delegation.")
		}
	}
	if jobID == "" {
		t.Fatalf("no delegation fact recorded: %v", facts)
	}
	root, err := ex.delegateRoot()
	if err != nil {
		t.Fatal(err)
	}
	job, err := jobs.Get(root, jobID)
	if err != nil {
		t.Fatal(err)
	}
	var spec jobs.SpawnSpec
	if err := json.Unmarshal(job.ExecutionEnvelope, &spec); err != nil {
		t.Fatal(err)
	}
	if spec.BrowserMode != BrowserExistingChrome {
		t.Fatalf("expected BrowserMode=%q, got %q — \"browser\" must default to existing_chrome, not ephemeral", BrowserExistingChrome, spec.BrowserMode)
	}
}

func TestExecutiveDelegatesToWorker(t *testing.T) {
	prov1 := &fakeProv{steps: []wire.Response{
		{StopReason: "tool_use", Blocks: []wire.Block{toolUse("t1", "delegate", map[string]any{
			"task": "look something up", "expected_output": "a fact", "completion_condition": "found it",
			"consequences": []string{"observe"},
		})}},
		{StopReason: "tool_use", Blocks: []wire.Block{toolUse("t2", "report", map[string]any{"summary": "delegated"})}},
	}}
	ex, st, home := newTestExecutive(t, prov1)
	ctx := context.Background()
	doc := DelegationPolicy{ObjectiveScope: "primary", ConsequenceClasses: []ConsequenceClass{Observe, LocalMutation}, MaxDelegationDepth: 1, MaxSeconds: 300, MaxActionsPerPeriod: 8}
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

	out, err := ex.RunOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" {
		t.Fatalf("status=%s report=%s", out.Status, out.Report)
	}

	facts, err := st.ListFacts(ctx, "primary")
	if err != nil {
		t.Fatal(err)
	}
	var jobID string
	for _, f := range facts {
		if strings.HasPrefix(f.Key, "delegation.") {
			jobID = strings.TrimPrefix(f.Key, "delegation.")
		}
	}
	if jobID == "" {
		t.Fatalf("no delegation fact recorded: %v", facts)
	}

	actions, err := st.ListActions(ctx, "primary", 10)
	if err != nil {
		t.Fatal(err)
	}
	var delegateAction Action
	for _, a := range actions {
		if a.Kind == "delegate" {
			delegateAction = a
		}
	}
	if delegateAction.ID == "" || delegateAction.Status != "running" {
		t.Fatalf("expected a running delegate action, got %+v", actions)
	}

	// Wait for the detached (test-binary) child to exit before checking on it.
	root, err := ex.delegateRoot()
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		job, err := jobs.Get(root, jobID)
		if err != nil {
			t.Fatal(err)
		}
		if job.Status != jobs.StatusRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("delegated job %s still running after 10s", jobID)
		}
		time.Sleep(50 * time.Millisecond)
	}

	prov2 := &fakeProv{steps: []wire.Response{
		{StopReason: "tool_use", Blocks: []wire.Block{toolUse("t3", "check_delegate", map[string]any{"job_id": jobID})}},
		{StopReason: "tool_use", Blocks: []wire.Block{toolUse("t4", "report", map[string]any{"summary": "checked"})}},
	}}
	ex2 := &Executive{Store: st, Home: home, AgentID: "tester", Objective: testObjective, Runner: llm.NewRunner(prov2)}
	out2, err := ex2.RunOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if out2.Status != "completed" {
		t.Fatalf("status=%s report=%s", out2.Status, out2.Report)
	}

	actions, err = st.ListActions(ctx, "primary", 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range actions {
		if a.Kind == "delegate" {
			delegateAction = a
		}
	}
	if delegateAction.Status == "running" {
		t.Fatalf("delegate action still running after check_delegate: %+v", delegateAction)
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
