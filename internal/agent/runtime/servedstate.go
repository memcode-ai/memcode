package runtime

import "github.com/memcode-ai/memcode/internal/wire"

// isCheapLane reports whether a wire Backend value is the cheap lane. The
// gateway sanitizes the lane's internal backend to the vendor-neutral wire
// value "cheap" (since 2026-07-14; the inference vendor never rides the wire);
// "vllm" is the legacy tag an un-redeployed gateway still sends — accept both
// so lane-gated behaviors (budget learning, plan review, ctx meter) survive
// the transition.
func isCheapLane(backend string) bool {
	return backend == "cheap" || backend == "vllm"
}

// servedState is the backend/routing telemetry from the gateway-tagged response — who served the
// last main call, at what window, and the cheap lane's learned budget. Carved off the Session god
// object; written in complete() (loop.go), read by the ServedBy/footer getters. Held as a VALUE
// (`s.served`). Distinct from s.model (the session's requested model): served.model is what the
// gateway ACTUALLY ran (≠ requested when a turn escalates, e.g. apply→Opus).
type servedState struct {
	backend     string // last serving backend (cheap | anthropic | openai | …) — for the ⇄ switch line
	pool        string // last serving cheap-lane model's short name (e.g. glm-5p1); "" on Anthropic
	model       string // model that ACTUALLY served the last main call — for an honest footer
	byok        bool   // last main call served on the USER's own provider key — strictly per-turn (a non-BYOK turn clears it)
	ctxTokens   int    // input tokens of the latest MAIN conversation call ≈ current context fill
	ctxWindow   int    // serving backend's real window (gateway-tagged); 0 ⇒ fall back to model default
	inputBudget int    // smallest input budget seen on the current lane — the target compaction aims under (was cheapBudget; every lane stamps it now)
}

// recordServed mutates the serving telemetry under dispMu. The engine calls this (in
// complete()) while the TUI render goroutine reads s.served every frame, so every write
// goes through here.
func (s *Session) recordServed(fn func(v *servedState)) {
	s.dispMu.Lock()
	defer s.dispMu.Unlock()
	fn(&s.served)
}

// servedSnapshot returns a copy of the serving telemetry under dispMu. Readers (the footer
// accessors, compaction) take a snapshot once and compute from it — never read s.served
// field-by-field under the lock, which would risk nested locking across getters.
func (s *Session) servedSnapshot() servedState {
	s.dispMu.Lock()
	defer s.dispMu.Unlock()
	return s.served
}

// SetServingDefault records the gateway's everyday (cheap-lane) model so the footer/banner
// show what will actually serve — instead of the CLI's bootstrap identity — before any turn
// has run. Set once at startup (after asking the gateway /v1/models); read under dispMu.
func (s *Session) SetServingDefault(model string) {
	s.dispMu.Lock()
	defer s.dispMu.Unlock()
	s.servingDefault = model
}

func (s *Session) servingDefaultSnapshot() string {
	s.dispMu.Lock()
	defer s.dispMu.Unlock()
	return s.servingDefault
}

// resetServedBudget clears the learned lane budget + window. Called on a /model pin
// change: the learned floor belongs to the OLD lane (a 200K haiku budget must not
// throttle a fresh 1M sol pin) — the next serve re-teaches both.
func (s *Session) resetServedBudget() {
	s.dispMu.Lock()
	defer s.dispMu.Unlock()
	s.served.inputBudget = 0
	s.served.ctxWindow = 0
}

// setTurnEffort sets this turn's thinking effort under dispMu (the footer reads it live).
func (s *Session) setTurnEffort(e wire.Effort) {
	s.dispMu.Lock()
	defer s.dispMu.Unlock()
	s.turnEffort = e
}
