package runtime

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/agent/tools"
	"github.com/memcode-ai/memcode/internal/store"
	"github.com/memcode-ai/memcode/internal/wire"
)

// gateProvider scripts a turn where the model breaks a Go file, declares itself
// done, and only fixes it after the completion gate nudges it back.
type gateProvider struct {
	mu          sync.Mutex
	calls       int
	sawNudge    bool
	nudgeOnCall int
}

func (p *gateProvider) Complete(_ context.Context, r wire.Request) (wire.Response, error) {
	p.mu.Lock()
	p.calls++
	calls := p.calls
	// Detect the completion-gate nudge arriving as the latest user message.
	if last := r.Messages[len(r.Messages)-1]; last.Role == "user" {
		for _, b := range last.Blocks {
			if strings.Contains(b.Text, "no longer parses") {
				p.sawNudge = true
				p.nudgeOnCall = p.calls
			}
		}
	}
	sawNudge := p.sawNudge
	nudgeOnCall := p.nudgeOnCall
	p.mu.Unlock()
	switch {
	case calls == 1: // break x.go (drop the closing brace)
		return wire.Response{StopReason: "tool_use", Blocks: []wire.Block{
			{Type: "tool_use", ID: "e1", Name: tools.EditFile,
				Input: json.RawMessage(`{"path":"x.go","old_string":"func F() {}","new_string":"func F() {"}`)},
		}}, nil
	case !sawNudge: // model thinks it's done — but the gate hasn't nudged yet
		return wire.Response{StopReason: "end_turn", Blocks: []wire.Block{{Type: "text", Text: "all done!"}}}, nil
	case sawNudge && calls == nudgeOnCall: // nudged → fix it
		return wire.Response{StopReason: "tool_use", Blocks: []wire.Block{
			{Type: "tool_use", ID: "e2", Name: tools.EditFile,
				Input: json.RawMessage(`{"path":"x.go","old_string":"func F() {","new_string":"func F() {}"}`)},
		}}, nil
	default: // fixed → really done
		return wire.Response{StopReason: "end_turn", Blocks: []wire.Block{{Type: "text", Text: "fixed and done"}}}, nil
	}
}

// sawNudgeSync returns sawNudge under the lock, for test assertions that run
// after the distillLesson goroutine may have set it.
func (p *gateProvider) sawNudgeSync() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sawNudge
}

// TestCompletionGateBlocksDoneOnBrokenEdit is the integration proof for #5's
// completion gate: the model can't end the turn while a file it edited doesn't
// parse — the gate nudges it back, and the turn only completes once the file is
// fixed on disk.
func TestCompletionGateBlocksDoneOnBrokenEdit(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	xgo := filepath.Join(dir, "x.go")
	if err := os.WriteFile(xgo, []byte("package p\n\nfunc F() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, filepath.Join(dir, ".state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	prov := &gateProvider{}
	// allow-all so the edits apply without an approval prompt.
	s := newSess(st, prov, dir, "opus", permissions.ModeAllowAll, io.Discard)

	msgs := []wire.Message{{Role: "user", Blocks: []wire.Block{{Type: "text", Text: "tweak F"}}}}
	_, ok, err := s.runLoop(ctx, promptSpec{mode: "chat"}, &msgs)
	if err != nil || !ok {
		t.Fatalf("runLoop: ok=%v err=%v", ok, err)
	}

	if !prov.sawNudgeSync() {
		t.Fatal("completion gate never nudged the model about the broken edit")
	}
	// The turn must NOT have ended with x.go broken.
	final, _ := os.ReadFile(xgo)
	if w := validateEdit("x.go", string(final)); w != "" {
		t.Fatalf("turn completed with x.go still broken:\n%s\n---file---\n%s", w, final)
	}
	if !strings.Contains(string(final), "func F() {}") {
		t.Errorf("x.go not repaired: %q", final)
	}
}

// TestCompletionGateBoundedByHealRounds: an UNFIXABLE break must not spin forever
// — the gate gives up after maxHealRounds and lets the turn end.
func TestCompletionGateBoundedByHealRounds(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "y.go"), []byte("package p\n\nfunc G() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, filepath.Join(dir, ".state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// A stubborn provider: breaks y.go once, then keeps declaring "done" without
	// ever fixing it. The gate should nudge maxHealRounds times, then relent.
	nudges := 0
	prov := provFunc(func(r wire.Request) wire.Response {
		if last := r.Messages[len(r.Messages)-1]; last.Role == "user" {
			for _, b := range last.Blocks {
				if strings.Contains(b.Text, "no longer parses") {
					nudges++
				}
			}
		}
		if len(r.Messages) == 1 { // first turn → break it
			return wire.Response{StopReason: "tool_use", Blocks: []wire.Block{
				{Type: "tool_use", ID: "e", Name: tools.EditFile,
					Input: json.RawMessage(`{"path":"y.go","old_string":"func G() {}","new_string":"func G() {"}`)},
			}}
		}
		return wire.Response{StopReason: "end_turn", Blocks: []wire.Block{{Type: "text", Text: "done (ignoring the warning)"}}}
	})
	s := newSess(st, prov, dir, "opus", permissions.ModeAllowAll, io.Discard)
	msgs := []wire.Message{{Role: "user", Blocks: []wire.Block{{Type: "text", Text: "break G"}}}}
	if _, ok, err := s.runLoop(ctx, promptSpec{mode: "chat"}, &msgs); err != nil || !ok {
		t.Fatalf("runLoop: ok=%v err=%v", ok, err)
	}
	if nudges != maxHealRounds {
		t.Errorf("gate nudged %d times, want exactly maxHealRounds=%d (bounded)", nudges, maxHealRounds)
	}
}

// provFunc adapts a func to the ModelProvider interface for scripted tests.
type provFunc func(wire.Request) wire.Response

func (f provFunc) Complete(_ context.Context, r wire.Request) (wire.Response, error) {
	return f(r), nil
}
