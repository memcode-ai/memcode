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
	"github.com/memcode-ai/memcode/internal/config"
	"github.com/memcode-ai/memcode/internal/store"
	"github.com/memcode-ai/memcode/internal/wire"
)

// recordingProv captures the model on every request so a test can see which
// model each delegated worker actually ran on.
type recordingProv struct{ models []string }

func (p *recordingProv) Complete(_ context.Context, r wire.Request) (wire.Response, error) {
	p.models = append(p.models, r.Pin)
	return wire.Response{StopReason: "end_turn", Model: r.Pin,
		Blocks: []wire.Block{wire.TextBlock("done")}, InputTokens: 1, OutputTokens: 1}, nil
}

func (p *recordingProv) Stream(ctx context.Context, r wire.Request, _ wire.StreamHandler) (wire.Response, error) {
	return p.Complete(ctx, r)
}

func delegatedSession(t *testing.T, prov *recordingProv) *Session {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	s := newSess(st, prov, t.TempDir(), "opus", permissions.ModeAuto, io.Discard)
	s.SetPin("opus", 1_000_000)
	return s
}

// (1) With no delegated preference, a spawned worker runs on the primary pin.
func TestSpawnedWorkerInheritsPrimaryByDefault(t *testing.T) {
	prov := &recordingProv{}
	s := delegatedSession(t, prov)
	if _, err := s.spawnAgent(context.Background(), AgentSpec{Task: "look at the repo", ReadOnly: true}); err != nil {
		t.Fatal(err)
	}
	if len(prov.models) == 0 {
		t.Fatal("the worker made no model call")
	}
	for i, m := range prov.models {
		if m != "opus" {
			t.Fatalf("worker call %d ran on %q, want the primary (opus); all: %v", i, m, prov.models)
		}
	}
}

// (2) With a delegated preference set, EVERY delegated worker uses it — and
// (6) two workers in the same session cannot differ, because nothing about the
// invocation can name a model.
func TestEveryDelegatedWorkerUsesTheDelegatedPin(t *testing.T) {
	prov := &recordingProv{}
	s := delegatedSession(t, prov)
	s.SetDelegatedPin("haiku", 200000)

	for _, spec := range []AgentSpec{
		{Task: "one", ReadOnly: true},
		{Task: "two"},
		{Task: "three", ReadOnly: true, Scope: "billing"},
	} {
		if _, err := s.spawnAgent(context.Background(), spec); err != nil {
			t.Fatalf("%s: %v", spec.Task, err)
		}
	}
	for i, m := range prov.models {
		if m != "haiku" {
			t.Fatalf("delegated call %d ran on %q, want haiku; all: %v", i, m, prov.models)
		}
	}
}

// (6) The agent tool exposes NO way to name a model. This is the guard against
// re-growing per-invocation routing under a new field name: the previous
// attempt was called `tier`.
func TestAgentToolCannotNameAModel(t *testing.T) {
	var agentDef *wire.ToolDef
	for _, d := range tools.Defs() {
		if d.Name == tools.Agent {
			def := d
			agentDef = &def
			break
		}
	}
	if agentDef == nil {
		t.Fatal("the agent tool is not registered")
	}
	raw, err := json.Marshal(agentDef.InputSchema)
	if err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{"model", "tier", "lane", "pin"} {
		if strings.Contains(strings.ToLower(string(raw)), `"`+banned+`"`) {
			t.Errorf("the agent tool schema exposes %q — a delegated worker's model is a PIN, "+
				"set once by the user, never chosen per invocation. Schema: %s", banned, raw)
		}
	}
}

// The tool records an explicit instruction and the runtime then uses it. This
// is the whole contract: instruction -> tool -> persisted pin -> deterministic
// use. The tool never runs a model, and never picks one.
func TestModelPreferenceToolSetsThePinAndPersists(t *testing.T) {
	prov := &recordingProv{}
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	root := t.TempDir()
	if _, err := config.Init(root, false); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	s := newSess(st, prov, root, "opus", permissions.ModeAuto, io.Discard)
	s.SetPin("opus", 1_000_000)

	res := s.modelPreferenceTool(context.Background(), json.RawMessage(`{"model":"haiku","scope":"workspace"}`))
	if res.isError {
		t.Fatalf("tool returned an error: %s", resText(res))
	}
	if s.DelegatedPin() != "haiku" {
		t.Fatalf("session delegated pin = %q, want haiku", s.DelegatedPin())
	}
	cfg, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DelegatedModel != "haiku" {
		t.Fatalf("workspace config delegated model = %q, want haiku", cfg.DelegatedModel)
	}
	// The user's own model is untouched.
	if s.Pin() != "opus" {
		t.Fatalf("primary pin = %q — the tool must never change the model the user talks to", s.Pin())
	}

	// (5) "inherit" resets.
	res = s.modelPreferenceTool(context.Background(), json.RawMessage(`{"model":"inherit","scope":"workspace"}`))
	if res.isError {
		t.Fatalf("reset returned an error: %s", resText(res))
	}
	if s.DelegatedPin() != "" {
		t.Fatalf("after inherit the delegated pin = %q, want empty", s.DelegatedPin())
	}
}

// An unknown or unpinnable model is refused rather than silently ignored or
// half-applied.
func TestModelPreferenceToolRejectsUnknownModels(t *testing.T) {
	prov := &recordingProv{}
	s := delegatedSession(t, prov)
	for _, in := range []string{`{"model":"model-nobody-added"}`, `{"model":"haiku","scope":"galaxy"}`, `{"model":""}`} {
		if res := s.modelPreferenceTool(context.Background(), json.RawMessage(in)); !res.isError {
			t.Errorf("%s should be refused, got %q", in, resText(res))
		}
	}
	if s.DelegatedPin() != "" {
		t.Fatalf("a refused call must not set anything, got %q", s.DelegatedPin())
	}
}

// resText flattens a toolResult's blocks for assertion messages.
func resText(r toolResult) string {
	var b strings.Builder
	for _, blk := range r.blocks {
		b.WriteString(blk.Text)
	}
	return b.String()
}
