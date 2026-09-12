package taskdetect

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/memcode-ai/memcode/internal/task"
)

var start = time.Date(2026, 9, 12, 9, 0, 0, 0, time.UTC)

func store(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "signals.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// fake is a scripted classifier: the pipeline must be provable without a model
// call, and the real classifier's job (reading a turn) is separable from the
// pipeline's job (accumulating, recognising, offering).
type fake struct{ next map[string]Proposal }

func (f *fake) Classify(_ context.Context, in Turn) (Proposal, bool, error) {
	p, ok := f.next[in.Request]
	return p, ok, nil
}

// catalogProposal is what a good classifier should return for any of the three
// differently-worded provider-catalog requests: DIFFERENT wording, SAME
// capability.
func catalogProposal(name, target string, conf float64) Proposal {
	return Proposal{
		Family: "provider-model-catalog-maintenance", Operation: "refresh",
		Target: "provider model catalogs", Scope: "repository", Project: "/repo",
		Name: name, Reason: "Provider model catalogs drift as vendors ship and retire models.",
		Instructions:   "Check each configured provider for model additions, removals or id changes, and update catalog/models.json where the vendor's published data disagrees with ours.",
		Verify:         []string{"go test ./catalog/"},
		SuggestedEvery: "168h", Effect: EffectPossible, Confidence: conf,
		Bounded: true, Reproducible: true, UnattendedSafe: true, Evaluable: true,
	}
}

// THE MILESTONE. Three differently worded provider-catalog requests, across
// three separate sessions, must accumulate into one recognised capability and
// produce an acceptable proposal — and accepting it must yield a task the
// existing engine can actually run.
func TestRecognisesACapabilityAcrossThreeSessions(t *testing.T) {
	ctx := context.Background()
	s := store(t)

	// Three asks. No useful wording in common; the classifier reads meaning.
	asks := []string{
		"update our Anthropic models",
		"OpenAI added models, update ours",
		"is the Fireworks catalog stale again?",
	}
	f := &fake{next: map[string]Proposal{
		asks[0]: catalogProposal("update-anthropic-models", "anthropic", 0.6),
		asks[1]: catalogProposal("refresh-openai-model-ids", "openai", 0.6),
		// A classifier naming the same capability slightly differently must
		// still accumulate rather than starting a new pile.
		asks[2]: func() Proposal {
			p := catalogProposal("check-fireworks-catalog", "fireworks", 0.6)
			p.Family = "provider-catalog-maintenance"
			return p
		}(),
	}}
	d := &Detector{Store: s, Classifier: f}

	var last Decision
	for i, ask := range asks {
		// A separate session, days apart — this is cross-session evidence, not
		// one person repeating themselves in a single conversation.
		when := start.Add(time.Duration(i) * 48 * time.Hour)
		dec, err := d.Evaluate(ctx, Turn{
			SessionID: "sess_" + string(rune('a'+i)), Project: "/repo",
			Request: ask, Summary: "updated the catalog", Changed: true,
		}, when)
		if err != nil {
			t.Fatalf("ask %d: %v", i, err)
		}
		if i < 2 && dec.Kind != KindNone {
			t.Fatalf("ask %d offered too early (%s) — two occasions is not yet a habit", i, dec.Kind)
		}
		last = dec
	}

	// The third ask is the moment.
	if last.Kind != KindRepeated {
		t.Fatalf("third ask produced %q (%s), want a repeated-behaviour offer", last.Kind, last.Why)
	}
	if last.Sessions < 2 || last.Occasions < 3 {
		t.Errorf("evidence = %d occasions / %d sessions, want all three counted",
			last.Occasions, last.Sessions)
	}
	if !strings.Contains(last.Proposal.Family, "catalog") {
		t.Errorf("family = %q, want the provider-catalog capability", last.Proposal.Family)
	}

	// The offer says memcode LEARNED something, not that it spotted a cron
	// candidate.
	msg := Message(last)
	if !strings.Contains(msg.Headline, "You've asked me to") {
		t.Errorf("a repeated offer must name the habit:\n%s", msg.Headline)
	}
	if !strings.Contains(msg.Headline, "autonomous task") {
		t.Errorf("the offer should say what it would do:\n%s", msg.Headline)
	}
	if len(msg.Options) != 4 {
		t.Fatalf("got %d options, want Create/Customize/Not now/Never", len(msg.Options))
	}

	// ACCEPTING must produce a task the existing engine can run — no wizard,
	// no gaps, valid on the first try.
	tk, err := last.Proposal.ToTask(start)
	if err != nil {
		t.Fatalf("the proposal must convert to a valid task: %v", err)
	}
	if tk.Name == "" || tk.Instructions == "" {
		t.Error("the created task needs a name and instructions")
	}
	if len(tk.Triggers) == 0 {
		t.Error("a task accepted from 'keep it current' must actually have a cadence")
	}
	if tk.Autonomy.Level != task.LevelBranch {
		t.Errorf("autonomy = %q, want branch for work that may change code", tk.Autonomy.Level)
	}
	if len(tk.Verify.Commands) == 0 {
		t.Error("the proposal's verification must carry into the task")
	}
	if err := tk.Validate(start); err != nil {
		t.Errorf("the task must be valid without further editing: %v", err)
	}
}

// A single strong turn is offered immediately, with different wording.
func TestStrongSingleTurnOffersAtOnce(t *testing.T) {
	ctx := context.Background()
	s := store(t)
	p := catalogProposal("refresh-catalogs", "all", 0.93)
	d := &Detector{Store: s, Classifier: &fake{next: map[string]Proposal{"do it": p}}}

	dec, err := d.Evaluate(ctx, Turn{SessionID: "s1", Project: "/repo", Request: "do it"}, start)
	if err != nil {
		t.Fatal(err)
	}
	if dec.Kind != KindSingleTurn {
		t.Fatalf("kind = %q (%s), want a single-turn offer", dec.Kind, dec.Why)
	}
	msg := Message(dec)
	if strings.Contains(msg.Headline, "You've asked me") {
		t.Errorf("a single-turn offer must not claim a history it does not have:\n%s", msg.Headline)
	}
	if !strings.Contains(msg.Headline, "on its own") {
		t.Errorf("headline = %q", msg.Headline)
	}
}

// THE GUARD. Repetition alone is not a qualification. Things recur that should
// never be handed to an unattended machine.
func TestGatesRejectRepeatedButUnsuitableWork(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		mut  func(*Proposal)
		want string
	}{
		{"vague", func(p *Proposal) { p.Bounded = false }, "not bounded"},
		{"one-off debugging", func(p *Proposal) { p.Reproducible = false }, "not reproducible"},
		{"needs a human", func(p *Proposal) { p.UnattendedSafe = false }, "not safe unattended"},
		{"no way to check", func(p *Proposal) { p.Evaluable = false }, "not evaluable"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := store(t)
			p := catalogProposal("x", "y", 0.99) // maximum confidence
			c.mut(&p)
			d := &Detector{Store: s, Classifier: &fake{next: map[string]Proposal{"q": p}}}

			// Even asked ten times across ten sessions, it never becomes an offer.
			for i := 0; i < 10; i++ {
				dec, err := d.Evaluate(ctx, Turn{
					SessionID: "s" + string(rune('a'+i)), Project: "/repo", Request: "q",
				}, start.Add(time.Duration(i)*time.Hour))
				if err != nil {
					t.Fatal(err)
				}
				if dec.Kind != KindNone {
					t.Fatalf("offered ineligible work on repeat %d", i)
				}
				if !strings.Contains(dec.Why, c.want) {
					t.Errorf("why = %q, want %q", dec.Why, c.want)
				}
			}
			// And nothing was recorded, so it cannot accumulate later.
			cl, _ := s.Clusters(ctx, "/repo", start)
			if len(cl) != 0 {
				t.Errorf("ineligible work must leave no evidence, got %d cluster(s)", len(cl))
			}
		})
	}
}

// Three signals in ONE session is somebody repeating themselves, not a habit.
func TestOneSessionIsNotAPattern(t *testing.T) {
	ctx := context.Background()
	s := store(t)
	p := catalogProposal("x", "y", 0.6)
	d := &Detector{Store: s, Classifier: &fake{next: map[string]Proposal{"q": p}}}
	for i := 0; i < 5; i++ {
		dec, err := d.Evaluate(ctx, Turn{SessionID: "same", Project: "/repo", Request: "q"},
			start.Add(time.Duration(i)*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if dec.Kind != KindNone {
			t.Fatalf("offered after %d asks in one session", i+1)
		}
	}
}

// "Not now" is not negative training: evidence keeps accumulating and a later
// offer is allowed. "Don't suggest this kind" is durable.
func TestNotNowIsNotSuppression(t *testing.T) {
	ctx := context.Background()
	s := store(t)
	p := catalogProposal("x", "y", 0.6)
	d := &Detector{Store: s, Classifier: &fake{next: map[string]Proposal{"q": p}}}

	// Two sessions, declined with "not now" — which records nothing.
	for i := 0; i < 2; i++ {
		if _, err := d.Evaluate(ctx, Turn{SessionID: "s" + string(rune('a'+i)), Project: "/repo", Request: "q"},
			start.Add(time.Duration(i)*24*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	// A third occasion still offers: declining once did not teach it "never".
	dec, err := d.Evaluate(ctx, Turn{SessionID: "sc", Project: "/repo", Request: "q"},
		start.Add(48*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if dec.Kind != KindRepeated {
		t.Fatalf("kind = %q (%s) — 'not now' must not stop future offers", dec.Kind, dec.Why)
	}

	// Now suppress durably.
	if err := s.Suppress(ctx, dec.Proposal, start); err != nil {
		t.Fatal(err)
	}
	after, err := d.Evaluate(ctx, Turn{SessionID: "sd", Project: "/repo", Request: "q"},
		start.Add(72*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if after.Kind != KindNone {
		t.Error("a durably suppressed capability must stop being offered")
	}
	if !strings.Contains(after.Why, "declined durably") {
		t.Errorf("why = %q", after.Why)
	}
}

// Suppression attaches to the CAPABILITY, not to automation in general.
// Refusing catalog maintenance says nothing about dependency audits.
func TestSuppressionIsSemanticallyScoped(t *testing.T) {
	ctx := context.Background()
	s := store(t)

	catalogs := catalogProposal("x", "y", 0.9)
	if err := s.Suppress(ctx, catalogs, start); err != nil {
		t.Fatal(err)
	}

	// The same capability under a slightly different name is still suppressed:
	// a classifier's naming drifting must not resurrect a refused offer.
	renamed := catalogs
	renamed.Family = "provider-catalog-maintenance"
	if ok, _ := s.Suppressed(ctx, renamed); !ok {
		t.Error("a near-identical family name must stay suppressed")
	}

	// An unrelated capability is untouched.
	deps := Proposal{
		Family: "dependency-security-audit", Operation: "audit", Target: "dependencies",
		Scope: "repository", Project: "/repo",
	}
	if ok, _ := s.Suppressed(ctx, deps); ok {
		t.Error("refusing one kind of task must not suppress an unrelated one")
	}

	// And a different project is a different thing.
	elsewhere := catalogs
	elsewhere.Project = "/other"
	if ok, _ := s.Suppressed(ctx, elsewhere); ok {
		t.Error("suppression is scoped to its project")
	}
}

// Identity is semantic. Wording that shares nothing must still group; wording
// that looks alike but means different things must not.
func TestIdentityIsSemanticNotLexical(t *testing.T) {
	a := catalogProposal("update-anthropic-models", "anthropic", 0.6)
	b := catalogProposal("refresh-openai-model-ids", "openai", 0.6)
	if !a.SameCapability(b) {
		t.Error("differently worded asks for one capability must group")
	}
	if a.FamilyKey() != b.FamilyKey() {
		t.Error("the family key must not depend on the instance name")
	}

	// Same words, different capability: updating a catalog and deleting one are
	// not the same job however alike they read.
	del := catalogProposal("x", "y", 0.6)
	del.Family = "provider-model-catalog-deletion"
	del.Operation = "delete"
	if del.FamilyKey() == a.FamilyKey() {
		t.Error("a different operation is a different capability")
	}

	// Scope separates otherwise identical work.
	other := a
	other.Project = "/somewhere-else"
	if a.SameCapability(other) {
		t.Error("the same capability in another project is a separate thing")
	}
}

// Old evidence decays out rather than accumulating forever.
func TestEvidenceDecays(t *testing.T) {
	ctx := context.Background()
	s := store(t)
	p := catalogProposal("x", "y", 0.6)

	// Two occasions, a year ago.
	for i := 0; i < 2; i++ {
		if _, err := s.Record(ctx, "s"+string(rune('a'+i)), p, "old", start.Add(-365*24*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	cl, err := s.Clusters(ctx, "/repo", start)
	if err != nil {
		t.Fatal(err)
	}
	if len(cl) != 1 {
		t.Fatalf("got %d clusters", len(cl))
	}
	if cl[0].Weight > 0.01 {
		t.Errorf("year-old evidence still weighs %.4f — it should have decayed away", cl[0].Weight)
	}
	if cl[0].Signals != 2 {
		t.Errorf("the occasions are still counted (%d), only their weight decays", cl[0].Signals)
	}
}

// Creating a task clears its evidence, so the task does not keep proposing
// itself.
func TestForgetClearsEvidence(t *testing.T) {
	ctx := context.Background()
	s := store(t)
	p := catalogProposal("x", "y", 0.6)
	for i := 0; i < 3; i++ {
		if _, err := s.Record(ctx, "s"+string(rune('a'+i)), p, "e", start); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Forget(ctx, p); err != nil {
		t.Fatal(err)
	}
	cl, _ := s.Clusters(ctx, "/repo", start)
	if len(cl) != 0 {
		t.Errorf("evidence should be cleared once the task exists, got %d cluster(s)", len(cl))
	}
}

// A read-only proposal becomes a read-only task: authority follows the
// predicted effect rather than defaulting to the more capable tier.
func TestAuthorityFollowsPredictedEffect(t *testing.T) {
	p := catalogProposal("audit", "x", 0.9)
	p.Effect = EffectNone
	tk, err := p.ToTask(start)
	if err != nil {
		t.Fatal(err)
	}
	if tk.Autonomy.Level != task.LevelReadOnly {
		t.Errorf("autonomy = %q, want read_only for work that changes nothing", tk.Autonomy.Level)
	}
	if tk.Git.PullRequest != task.PRNever {
		t.Errorf("a read-only task should open no pull requests, got %q", tk.Git.PullRequest)
	}
}

// A proposal that names no cadence still gets one — a task created from "keep
// it current" that never runs is not what anyone agreed to.
func TestProposalWithoutCadenceStillSchedules(t *testing.T) {
	p := catalogProposal("x", "y", 0.9)
	p.SuggestedEvery, p.SuggestedCron = "", ""
	tk, err := p.ToTask(start)
	if err != nil {
		t.Fatal(err)
	}
	if len(tk.Triggers) == 0 || tk.Manual() {
		t.Error("an accepted offer must produce a task that actually runs")
	}
}

func TestUnsuppressReopensOffers(t *testing.T) {
	ctx := context.Background()
	s := store(t)
	p := catalogProposal("x", "y", 0.9)
	if err := s.Suppress(ctx, p, start); err != nil {
		t.Fatal(err)
	}
	n, err := s.Unsuppress(ctx, p.Family)
	if err != nil || n != 1 {
		t.Fatalf("Unsuppress = %d, %v", n, err)
	}
	if ok, _ := s.Suppressed(ctx, p); ok {
		t.Error("lifting a suppression must allow offers again")
	}
}

// Accepting a recognised capability names the task for the CAPABILITY, not for
// whichever instance was seen last. "provider-catalog-maintenance", not
// "check-fireworks-catalog" because Fireworks happened to come up most recently.
func TestAcceptedTaskIsNamedForTheCapability(t *testing.T) {
	ctx := context.Background()
	s := store(t)
	for i, name := range []string{"update-anthropic-models", "refresh-openai-ids", "check-fireworks-catalog"} {
		p := catalogProposal(name, "x", 0.6)
		if _, err := s.Record(ctx, "s"+string(rune('a'+i)), p, name,
			start.Add(time.Duration(i)*24*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	cl, err := s.Clusters(ctx, "/repo", start.Add(72*time.Hour))
	if err != nil || len(cl) != 1 {
		t.Fatalf("clusters = %d, %v", len(cl), err)
	}
	tk, err := cl[0].Capability().ToTask(start)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(tk.Name, "fireworks") {
		t.Errorf("name = %q — named after the last instance rather than the capability", tk.Name)
	}
	if !strings.Contains(tk.Name, "catalog") {
		t.Errorf("name = %q, want the capability name", tk.Name)
	}
}
