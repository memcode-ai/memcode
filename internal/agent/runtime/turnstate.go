package runtime

// turnState is the per-turn loop state that runLoop resets at the start of every turn — bundled
// into one struct so a fresh turn is `s.turn = newTurnState()` instead of hand-zeroing each field
// (forgetting one was a cross-turn-leak bug-farm). Carved off the Session god object; mirrors the
// gatherState idiom (a per-turn sub-state with its helpers on Session).
//
// turnEffort / turnHighRisk are NOT here — they're turn INPUTS computed by the caller (chat/Run)
// BEFORE the loop runs, so they live on Session; turnState holds only what runLoop owns + resets.
type turnState struct {
	healRounds     int             // completion-gate fix nudges issued this turn (bounded)
	stallRounds    int             // empty-turn-with-pending-work resume nudges this turn (bounded)
	applyConts     int             // NO-PROGRESS apply continuations since the todo state last changed (bounded; progress resets)
	lastTodoSig    string          // todo-state fingerprint at the last apply continuation (progress detector)
	toolLeakRounds int             // re-prompts after a tool call leaked as TEXT this turn (bounded; see toolleak.go)
	gather         *gatherState    // per-turn read-only-gathering budget + repetition tracker (gather.go)
	editedPaths    map[string]bool // files edited this turn (for the completion gate)
	servedLine     string          // last printed "⇄ served by …" line — dedup once/turn
	interrupted    bool            // the user chose to stop this turn
	firstBreak     string          // the FIRST broken-edit nudge this turn — the failure evidence for lesson distillation
	lessonDone     bool            // a lesson was already distilled this turn (fire once)
	billingCredits bool            // user consented to serve THIS turn on memcode credits after a BYOK key failure
}

// newTurnState returns a fresh per-turn state (with an initialized gather tracker).
func newTurnState() *turnState {
	return &turnState{gather: newGatherState()}
}
