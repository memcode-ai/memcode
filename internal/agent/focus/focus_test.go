package focus

import (
	"strings"
	"testing"

	"github.com/memcode-ai/memcode/internal/objectives"
	"github.com/memcode-ai/memcode/internal/sessionlog"
)

func ask(text string) sessionlog.Record {
	return sessionlog.Record{Kind: sessionlog.KindUserMessage, Text: text}
}
func commit() sessionlog.Record {
	return sessionlog.Record{Kind: sessionlog.KindToolCall, Tool: "bash", Input: `git commit -m "x"`}
}

// Reduce derives soft attention states from deterministic signals: a commit
// completes the active strand, a new ask pauses the prior one, and terse
// done/forget/park language reclassifies — long messages that merely contain those
// words are real asks, not signals.
func TestReduceTransitions(t *testing.T) {
	recs := []sessionlog.Record{
		ask("fix the paste bug"),    // strand A (active)
		commit(),                    // A → completed (milestone)
		ask("add the $ shell lane"), // strand B (active)
		ask("park it, let's do the gitignore thing"), // long ask → NOT a park signal; new strand C, B paused
		ask("forget that"),                           // terse drop → C dropped
		ask("now model the focus axis"),              // strand D (active = current focus)
	}
	st := Reduce(recs, nil)

	if st.Current != "now model the focus axis" {
		t.Errorf("Current = %q, want the last active ask", st.Current)
	}
	if !contains(st.Completed, "fix the paste bug") {
		t.Errorf("a commit should complete the active strand; Completed=%v", st.Completed)
	}
	if !contains(st.Paused, "add the $ shell lane") {
		t.Errorf("a new ask should pause the prior strand; Paused=%v", st.Paused)
	}
	if !contains(st.Dropped, "park it, let's do the gitignore thing") {
		t.Errorf("terse 'forget that' should drop the prior strand; Dropped=%v", st.Dropped)
	}
}

// "done" inside a long message must NOT complete a strand (only terse signals do).
func TestReduceLongMessageNotASignal(t *testing.T) {
	recs := []sessionlog.Record{
		ask("implement the reducer"),
		ask("when you're done with the reducer also wire it into predict and overview please"),
	}
	st := Reduce(recs, nil)
	if len(st.Completed) != 0 {
		t.Errorf("a long message containing 'done' must not complete anything; Completed=%v", st.Completed)
	}
	if !strings.HasPrefix(st.Current, "when you're done with the reducer") {
		t.Errorf("the latest substantive ask should be the current focus; Current=%q", st.Current)
	}
}

// A terse instruction that merely CONTAINS a transition phrase is a real ask, not a
// transition command: "use sqlite for now" (has "for now") must NOT park+vanish, and
// "have you done the tests?" (has "done", is a question) must NOT complete the strand.
func TestReduceTerseInstructionNotATransition(t *testing.T) {
	st := Reduce([]sessionlog.Record{
		ask("implement the store"),
		ask("use sqlite for now"),
	}, nil)
	// The instruction must become the current focus (not vanish as a park). Pausing the
	// prior strand is correct — a new substantive ask supersedes it. Pre-fix it parked the
	// active strand and dropped the instruction entirely, so Current was not "use sqlite".
	if !strings.HasPrefix(st.Current, "use sqlite") {
		t.Errorf("the instruction must become the current focus, not vanish; Current=%q", st.Current)
	}
	if len(st.Dropped)+len(st.Completed) != 0 {
		t.Errorf("the instruction must not be dropped/completed; state=%+v", st)
	}

	st2 := Reduce([]sessionlog.Record{
		ask("build the parser"),
		ask("have you done the tests?"),
	}, nil)
	if len(st2.Completed) != 0 {
		t.Errorf("a question containing 'done' must not complete anything; Completed=%v", st2.Completed)
	}
}

// A search whose ARGUMENT contains "git commit" is not a commit — the strand must stay
// active (the phantom-milestone bug from substring matching).
func TestReduceRipgrepIsNotACommit(t *testing.T) {
	st := Reduce([]sessionlog.Record{
		ask("wire up the gateway"),
		{Kind: sessionlog.KindToolCall, Tool: "bash", Input: `rg "git commit" internal/`},
	}, nil)
	if len(st.Completed) != 0 {
		t.Errorf("rg of the phrase 'git commit' must not complete the strand; Completed=%v", st.Completed)
	}
	if !strings.HasPrefix(st.Current, "wire up the gateway") {
		t.Errorf("the strand must stay active; Current=%q", st.Current)
	}
	// A REAL commit still completes it.
	st2 := Reduce([]sessionlog.Record{ask("wire up the gateway"), commit()}, nil)
	if len(st2.Completed) != 1 {
		t.Errorf("a real git commit must complete the strand; Completed=%v", st2.Completed)
	}
}

// Active objectives form the durable layer; the render surfaces them.
func TestReduceObjectivesAndRender(t *testing.T) {
	objs := []objectives.Objective{{Title: "ship FocusState", Status: objectives.StatusActive}}
	st := Reduce([]sessionlog.Record{ask("draft the projection")}, objs)
	if !containsStr(st.Decisions, "ship FocusState") {
		t.Errorf("active objectives should be durable decisions; Decisions=%v", st.Decisions)
	}
	out := Render(st)
	if !strings.Contains(out, `1. "draft the projection"`) || !strings.Contains(out, `durable: "ship FocusState"`) {
		t.Errorf("render missing sections:\n%s", out)
	}
	if Render(State{}) != "" {
		t.Error("an empty state should render nothing")
	}
}

func contains(xs []Strand, want string) bool {
	for _, x := range xs {
		if strings.Contains(x.label(), want) {
			return true
		}
	}
	return false
}

func containsStr(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// A cancelled plan is abandoned work, not erased work: the strand must surface
// as PAUSED so the next session's orientation still carries the open thread
// (the exit_plan-prior incident: a cancelled GPT-5.6 migration plan vanished).
func TestPlanCancelledPausesStrand(t *testing.T) {
	recs := []sessionlog.Record{
		{Kind: sessionlog.KindUserMessage, Text: "update to gpt 5.6 and rewire the model tiers"},
		{Kind: sessionlog.KindPlanCancelled, Text: "update to gpt 5.6 and rewire the model tiers"},
		{Kind: sessionlog.KindUserMessage, Text: "fix the themes alias"},
	}
	st := Reduce(recs, nil)
	if st.Current != "fix the themes alias" {
		t.Fatalf("current = %q", st.Current)
	}
	found := false
	for _, p := range st.Paused {
		if strings.Contains(p.Title, "gpt 5.6") {
			found = true
		}
	}
	if !found {
		t.Fatalf("cancelled plan's ask must surface as paused, got paused=%v open=%v dropped=%v", st.Paused, st.Open, st.Dropped)
	}
}

// The "13 paused items" bug: conversational glue and meta-queries drowned the
// real work threads, so orientation buried the abandoned GPT-5.6 migration.
// Glue must be transparent; real asks must surface, newest first.
func TestGlueDoesNotDrownWork(t *testing.T) {
	mk := func(txt string) sessionlog.Record {
		return sessionlog.Record{Kind: sessionlog.KindUserMessage, Text: txt}
	}
	recs := []sessionlog.Record{
		mk("update to gpt 5.6 and rewire the model tiers"),
		mk("yes"),
		mk("thanks"),
		mk("[background shell finished] a command completed"), // runtime injection
		mk("fix the themes alias"),
		mk("rebuilt cli?"),            // short non-ask question
		mk("what are we working on?"), // meta query — not a strand, not current
	}
	st := Reduce(recs, nil)

	// Meta query is transparent → the last REAL ask stays current.
	if st.Current != "fix the themes alias" {
		t.Fatalf("meta query should not become current: got %q", st.Current)
	}
	// The GPT-5.6 thread survived as paused work.
	joined := strings.Join(labels(st.Paused), " | ")
	if !strings.Contains(joined, "gpt 5.6") {
		t.Fatalf("the abandoned migration must surface as paused: %v", st.Paused)
	}
	// None of the glue became a strand anywhere.
	all := strings.Join(append(append(append([]string{st.Current}, labels(st.Open)...), labels(st.Paused)...), labels(st.Completed)...), " | ")
	for _, glue := range []string{"yes", "thanks", "rebuilt cli", "background shell", "what are we working on"} {
		if strings.Contains(strings.ToLower(all), glue) {
			t.Fatalf("glue %q leaked into a strand: %s", glue, all)
		}
	}
}

// Paused reads newest-first and is capped so a long burst can't bury the signal.
func TestPausedNewestFirstAndCapped(t *testing.T) {
	var recs []sessionlog.Record
	for i := 0; i < 10; i++ {
		recs = append(recs, sessionlog.Record{Kind: sessionlog.KindUserMessage,
			Text: "work item " + string(rune('a'+i))})
	}
	st := Reduce(recs, nil)
	// item j (the last) is current; earlier items pause, newest of them first.
	if st.Current != "work item j" {
		t.Fatalf("current = %q", st.Current)
	}
	if len(st.Paused) == 0 || !strings.Contains(st.Paused[0].Title, "item i") {
		t.Fatalf("paused must be newest-first: %v", st.Paused)
	}
	if len(st.Paused) > maxPerBucket {
		t.Fatalf("paused not capped: %d items", len(st.Paused))
	}
	// The cut is a COUNT rendered as prose — never a phantom "(+N more)" strand
	// the model would try to "account for" as if it were work.
	if st.Elided == 0 {
		t.Fatal("elided count must record the cut")
	}
	if out := Render(st); !strings.Contains(out, "older thread") {
		t.Fatalf("truncation must be visible as prose in the digest:\n%s", out)
	}
	for _, p := range st.Paused {
		if strings.Contains(p.Title, "more") {
			t.Fatalf("no phantom list entries: %v", st.Paused)
		}
	}
}

func TestAckAndMetaHelpers(t *testing.T) {
	for _, a := range []string{"yes", "Thanks.", "ok", "go ahead", "done?", "rebuilt cli?"} {
		if !ackOnly(strings.ToLower(a)) {
			t.Errorf("ackOnly(%q) should be true", a)
		}
	}
	for _, w := range []string{"update to gpt 5.6", "fix the parser bug"} {
		if ackOnly(strings.ToLower(w)) {
			t.Errorf("ackOnly(%q) should be false — real work", w)
		}
	}
	for _, m := range []string{"what are we working on?", "where were we", "catch me up"} {
		if !metaAsk(strings.ToLower(m)) {
			t.Errorf("metaAsk(%q) should be true", m)
		}
	}
}

// The truncation fix: a long multi-part ask must survive WHOLE onto the strand's
// Full (acting on the 100-char Title amputated a real spec), and the thread must
// name its source session. AskLine renders the verbatim ask, not the summary.
func TestStrandCarriesFullAskAndSession(t *testing.T) {
	long := "update to gpt 5.6 and look up the model ids and tiers. " +
		"use opus as the advisor, switch anthropic opus to the highest 5.6, " +
		"and switch sonnet to the mid 5.6 — do not change the standard lane."
	recs := []sessionlog.Record{
		{Kind: sessionlog.KindUserMessage, Text: long, SessionID: "sess_orig"},
		{Kind: sessionlog.KindUserMessage, Text: "unrelated newer ask", SessionID: "sess_new"},
	}
	st := Reduce(recs, nil)

	// The long ask is now paused (superseded); find it.
	var found *Strand
	for i := range st.Paused {
		if strings.Contains(st.Paused[i].Full, "advisor") {
			found = &st.Paused[i]
		}
	}
	if found == nil {
		t.Fatalf("long ask missing from paused: %+v", st.Paused)
	}
	// Title is the capped summary; Full is the whole thing — they MUST diverge here.
	if len(found.Title) >= len(found.Full) {
		t.Fatalf("Title should be a shorter summary than Full; title=%q full=%q", found.Title, found.Full)
	}
	for _, must := range []string{"advisor", "highest 5.6", "mid 5.6", "do not change the standard lane"} {
		if !strings.Contains(found.Full, must) {
			t.Fatalf("Full lost %q: %s", must, found.Full)
		}
	}
	if found.Session != "sess_orig" {
		t.Fatalf("thread must name its source session, got %q", found.Session)
	}
	if !strings.Contains(found.AskLine(), "[from sess_orig]") {
		t.Fatalf("AskLine must name the session: %s", found.AskLine())
	}
}
