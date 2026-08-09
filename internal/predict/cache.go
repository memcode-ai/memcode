package predict

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os/exec"
	"strings"
	"time"

	"github.com/memcode-ai/memcode/internal/agent/focus"
	"github.com/memcode-ai/memcode/internal/store"
)

// Prediction is a cached synthesis, stored in current_state. Unlike the overview
// (keyed by HEAD alone), a prediction also depends on the UNCOMMITTED working tree
// — so it's keyed by HEAD plus a fingerprint of the dirty files + diff. Within an
// unchanged (commit, working-tree) state, repeat `predict` calls are free.
type Prediction struct {
	Text        string    `json:"text"`
	HeadSHA     string    `json:"head_sha"`
	Fingerprint string    `json:"fingerprint"`
	GeneratedAt time.Time `json:"generated_at"`
}

const (
	stateScope = "repo"
	stateLayer = "predict"
)

// Fingerprint hashes ALL the state a prediction depends on, so the cache invalidates the
// moment any of it changes. That's HEAD + the dirty files/diff AND the episodic evidence:
// objectives and the focus projection (current/open/paused threads). Without the latter, a
// clean tree that abandoned a plan, added an objective, or opened a thread kept serving the
// pre-session cached prediction until some FILE changed — stale against fresh memory.
func Fingerprint(ctx context.Context, root string, ev Evidence) string {
	h := sha256.New()
	sep := func() { h.Write([]byte{0}) }
	h.Write([]byte(Head(ctx, root)))
	sep()
	h.Write([]byte(strings.Join(ev.DirtyFiles, "\n")))
	sep()
	h.Write([]byte(ev.Diff))
	sep()
	h.Write([]byte(strings.Join(ev.Objectives, "\n")))
	sep()
	h.Write([]byte(focusSignature(ev.Focus)))
	return hex.EncodeToString(h.Sum(nil))
}

// focusSignature is a stable digest of the focus projection the prediction reasons about:
// the current thread plus the titles of open and paused (unfinished) work. Completed/
// dropped don't drive the next-step prediction, so they're omitted to avoid churn.
func focusSignature(f focus.State) string {
	var b strings.Builder
	b.WriteString(f.Current)
	for _, s := range f.Open {
		b.WriteByte('\n')
		b.WriteString(s.Title)
	}
	for _, s := range f.Paused {
		b.WriteByte('\n')
		b.WriteString(s.Title)
	}
	return b.String()
}

// LoadCached returns the cached prediction and whether it matches the current
// (HEAD, working-tree) state — i.e. can be served without a model call.
func LoadCached(ctx context.Context, st store.Store, root string, fingerprint string) (Prediction, bool) {
	state, ok, err := st.GetState(ctx, stateScope, stateLayer)
	if err != nil || !ok || len(state.Body) == 0 {
		return Prediction{}, false
	}
	var p Prediction
	if json.Unmarshal(state.Body, &p) != nil {
		return Prediction{}, false
	}
	fresh := p.Fingerprint != "" && p.Fingerprint == fingerprint && p.HeadSHA == Head(ctx, root)
	return p, fresh
}

// StoreCached persists a freshly synthesized prediction.
func StoreCached(ctx context.Context, st store.Store, root, fingerprint, text string) {
	p := Prediction{
		Text:        text,
		HeadSHA:     Head(ctx, root),
		Fingerprint: fingerprint,
		GeneratedAt: time.Now().UTC(),
	}
	body, _ := json.Marshal(p)
	_ = st.PutState(ctx, store.State{Scope: stateScope, Layer: stateLayer, Body: body, RefreshedAt: p.GeneratedAt})
}

// Head returns the current HEAD SHA (empty outside a repo).
func Head(ctx context.Context, root string) string {
	out, err := exec.CommandContext(ctx, "git", "-C", root, "rev-parse", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
