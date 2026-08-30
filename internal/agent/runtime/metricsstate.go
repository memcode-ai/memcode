package runtime

// metricsState is the session-scoped accounting counters — tool calls, reads, edit/verify
// sequencing for the completion gate. Carved off the Session god object; mutations stay guarded
// by s.mu (concurrent tool execution). Held as a VALUE (`s.metrics`). Distinct from the llm.Ledger
// (model cost/tokens) — these are task-progress/efficiency metrics, a different domain.
type metricsState struct {
	toolCalls       int
	toolErrors      int               // failed tool calls ("wrong turns")
	filesRead       int               // files read this session
	didEdit         bool              // any file changed this session
	didVerify       bool              // any build/test command run this session
	lastEditSeq     int               // tool-call seq of the most recent edit
	lastVerifyOKSeq int               // tool-call seq of the most recent passing verification
	blockerSeq      int               // failed deliverable tool that must be resolved before a todo is done
	blockerLabel    string            // user-facing name of the blocked deliverable (e.g. GitHub create PR)
	blockerDetail   string            // first useful error line to feed back on refused todo completion
	readHashes      map[string]string // path → content hash when last read/wrote (stale-edit guard; lazily inited)
	reportsSpilled  int               // sub-agent reports written to .memcode/sessions/<id>/reports/ (names the files)
}
