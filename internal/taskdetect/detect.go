package taskdetect

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Thresholds. Three paths answer three different questions, so they have three
// different bars.
const (
	// prospectiveConfidence is what a CAUSAL claim must reach to be offered on
	// first contact. The claim is "the world will make this necessary again",
	// and it is worth interrupting for only when the model can say why.
	prospectiveConfidence = 0.75
	// shapedConfidence is the separate bar for "this work is well-formed enough
	// to hand to a machine". Both must clear for a first-contact offer: work can
	// be obviously recurring and still too vague to automate.
	shapedConfidence = 0.6

	// repeatWeight and repeatSessions are the accumulation bar. Weight is
	// confidence summed with age decay; sessions counts DISTINCT conversations,
	// which is the part that carries meaning — three signals inside one session
	// is somebody repeating themselves, not a pattern across time.
	//
	// At a typical weak confidence of ~0.6 these fire on the third occasion:
	// two are 1.2 and not yet a habit, three are 1.8 and worth mentioning.
	repeatWeight   = 1.5
	repeatSessions = 2

	// recordConfidence is the floor for keeping a signal at all. Below it the
	// classifier is guessing, and storing guesses would let noise accumulate
	// into a confident-looking cluster.
	recordConfidence = 0.35
)

// Kind is which evidence path produced an offer. They read differently to the
// user because they mean different things.
type Kind string

const (
	// KindNone: nothing to offer.
	KindNone Kind = ""
	// KindProspective: the work will be needed again for a reason that exists in
	// the world, said on FIRST contact. The primary path — memcode understanding
	// the work rather than waiting to watch someone repeat it.
	KindProspective Kind = "prospective"
	// KindRepeated: recurrence learned from behaviour instead of inferred. The
	// fallback, and the right answer when the work has no inherent cadence but
	// this particular person keeps wanting it.
	KindRepeated Kind = "repeated"
	// KindExplicit: the user asked for it to be automated. No inference needed.
	KindExplicit Kind = "explicit"
)

// Decision is the outcome of evaluating a turn.
type Decision struct {
	Kind     Kind
	Proposal Proposal
	// Sessions and Occasions describe the evidence behind a repeated offer.
	Sessions  int
	Occasions int
	// Why explains a decision NOT to offer, for the debug surface. Silence that
	// cannot be explained is indistinguishable from a broken detector.
	Why string
}

// Classifier turns a finished turn into a proposal. Injectable: the real one
// costs a model call, and the pipeline around it has to be testable without one.
type Classifier interface {
	Classify(ctx context.Context, in Turn) (Proposal, bool, error)
}

// Turn is what the detector sees after work completes.
type Turn struct {
	SessionID string
	Project   string
	// Request is what the user asked for, Summary what was done.
	Request string
	Summary string
	// Changed reports whether the turn actually altered the repository — weak
	// but real evidence that the work was substantive.
	Changed bool
}

// Detector evaluates finished turns and decides whether to offer anything.
type Detector struct {
	Store      *Store
	Classifier Classifier
}

// Evaluate classifies a turn, records what it learned, and reports whether to
// make an offer.
//
// Recording happens even when no offer follows: today's weak signal is what
// makes next month's recognition possible, and a detector that only remembers
// what it already acted on can never learn anything.
func (d *Detector) Evaluate(ctx context.Context, in Turn, now time.Time) (Decision, error) {
	p, ok, err := d.Classifier.Classify(ctx, in)
	if err != nil {
		return Decision{}, err
	}
	if !ok {
		return Decision{Why: "the work does not look like a standing job"}, nil
	}
	p.Project = firstNonEmpty(p.Project, in.Project)

	// THE FOUR GATES, before anything else. Repetition is not a qualification:
	// a password reset recurs, "make this better" recurs, and an anxious
	// question recurs. None of them is a capability to hand to a machine that
	// runs at 3am with nobody watching.
	if !p.Eligible() {
		return Decision{Why: p.IneligibleReason()}, nil
	}
	if p.Confidence < recordConfidence {
		return Decision{Why: fmt.Sprintf("too uncertain to keep (%.2f)", p.Confidence)}, nil
	}

	suppressed, err := d.Store.Suppressed(ctx, p)
	if err != nil {
		return Decision{}, err
	}
	if suppressed {
		// Still not recorded: continuing to accumulate evidence for something
		// explicitly refused is how a "never" becomes a "later".
		return Decision{Why: "this kind of task was declined durably"}, nil
	}

	// A ONE-OFF is not recorded at all. Renaming the product passes every
	// shape gate and will never be wanted again; keeping evidence for it would
	// let a genuinely unrepeatable job accumulate into a confident-looking
	// cluster purely by being asked about twice.
	if p.Recurrence.Kind == RecurrenceOneOff {
		return Decision{Why: oneOffReason(p)}, nil
	}

	if _, err := d.Store.Record(ctx, in.SessionID, p, clip(in.Request, 200), now); err != nil {
		return Decision{}, err
	}

	// PATH 1 — the user simply asked for it. No inference required, and
	// second-guessing an explicit request is just being difficult.
	if p.ExplicitRequest {
		return Decision{Kind: KindExplicit, Proposal: p}, nil
	}

	// PATH 2 — PROSPECTIVE. There is a concrete reason to expect this work
	// again, so it is offered after the first completed instance. This is the
	// primary path: provider catalogs drift, dependencies release, advisories
	// appear, certificates expire — all knowable from understanding the work,
	// none of it requiring anyone to have repeated themselves.
	if p.Recurrence.Inherent() &&
		p.Recurrence.Confidence >= prospectiveConfidence &&
		p.Confidence >= shapedConfidence &&
		strings.TrimSpace(p.Recurrence.Cause) != "" {
		return Decision{Kind: KindProspective, Proposal: p}, nil
	}

	// PATH 3 — HISTORICAL. The causal claim was weak or absent, so evidence
	// accumulates until this person's behaviour supplies what inference could
	// not. This is where "clean up the stale TODOs" eventually qualifies: not
	// inherently recurring, but plainly something they keep wanting.
	clusters, err := d.Store.Clusters(ctx, p.Project, now)
	if err != nil {
		return Decision{}, err
	}
	for _, c := range clusters {
		if !c.Latest.SameCapability(p) && c.FamilyKey != p.FamilyKey() {
			continue
		}
		if c.Sessions >= repeatSessions && c.Weight >= repeatWeight {
			return Decision{
				Kind: KindRepeated, Proposal: c.Capability(),
				Sessions: c.Sessions, Occasions: c.Signals,
			}, nil
		}
		return Decision{Why: fmt.Sprintf(
			"%s; not yet a pattern either (%d occasion(s) across %d session(s), weight %.2f)",
			prospectiveShortfall(p), c.Signals, c.Sessions, c.Weight)}, nil
	}
	return Decision{Why: prospectiveShortfall(p) + "; first time seeing it"}, nil
}

// oneOffReason explains a refusal to treat unrepeatable work as a standing job.
func oneOffReason(p Proposal) string {
	if c := strings.TrimSpace(p.Recurrence.Cause); c != "" {
		return "one-off — " + c
	}
	return "one-off — no reason to expect it again"
}

// prospectiveShortfall says which half of the first-contact bar was missed, so
// silence is explainable rather than mysterious.
func prospectiveShortfall(p Proposal) string {
	switch {
	case p.Recurrence.Kind == RecurrenceUserPattern:
		return "recurs only if this user keeps asking"
	case !p.Recurrence.Inherent():
		return "no concrete reason it must happen again"
	case strings.TrimSpace(p.Recurrence.Cause) == "":
		return "claimed to recur but gave no cause"
	case p.Recurrence.Confidence < prospectiveConfidence:
		return fmt.Sprintf("recurrence only %.2f confident", p.Recurrence.Confidence)
	case p.Confidence < shapedConfidence:
		return fmt.Sprintf("shape only %.2f confident", p.Confidence)
	}
	return "below the first-contact bar"
}

// WouldOfferOnFirstContact reports whether a proposal clears the first-contact
// bar, without touching any store. Exported so the detector evaluation measures
// the REAL rule rather than a paraphrase of it that can drift.
func WouldOfferOnFirstContact(p Proposal) bool {
	if p.Recurrence.Kind == RecurrenceOneOff {
		return false
	}
	if p.ExplicitRequest {
		return true
	}
	return p.Recurrence.Inherent() &&
		p.Recurrence.Confidence >= prospectiveConfidence &&
		p.Confidence >= shapedConfidence &&
		strings.TrimSpace(p.Recurrence.Cause) != ""
}

// Ready reports whether a cluster is worth offering, by EITHER path.
//
// Both are checked here because a prospective offer is made in the middle of a
// session, where nothing may interrupt — so it waits with the historical ones,
// and would be silently lost if this only knew about accumulated evidence.
func Ready(c Cluster) bool {
	if !c.Latest.Eligible() {
		return false
	}
	if WouldOfferOnFirstContact(c.Latest) {
		return true
	}
	return c.Sessions >= repeatSessions && c.Weight >= repeatWeight
}

// ClusterDecision renders a waiting cluster as the offer it should become.
//
// Accumulated evidence wins when it exists: "you have asked me this three
// times" is a stronger and more personal claim than "this kind of thing
// recurs", and having earned it, memcode should say it.
func ClusterDecision(c Cluster) Decision {
	if c.Sessions >= repeatSessions && c.Weight >= repeatWeight {
		return Decision{Kind: KindRepeated, Proposal: c.Capability(),
			Sessions: c.Sessions, Occasions: c.Signals}
	}
	if c.Latest.ExplicitRequest {
		return Decision{Kind: KindExplicit, Proposal: c.Capability()}
	}
	return Decision{Kind: KindProspective, Proposal: c.Capability()}
}

// Offer is the message and choices shown to the user.
type Offer struct {
	Headline string
	Detail   string
	Options  []Option
}

// Option is one action.
type Option struct {
	Key         string
	Label       string
	Description string
}

// The four actions. Deliberately not Yes/No: the difference between "not right
// now" and "never this kind" is the difference between a system that learns and
// one that nags.
const (
	ActionCreate    = "create"
	ActionCustomize = "customize"
	ActionNotNow    = "not_now"
	ActionNever     = "never"
)

// Message renders the offer for a decision.
//
// The two paths read differently ON PURPOSE. A single-turn offer is a
// suggestion about the work just done. A repeated offer is memcode saying it
// noticed a habit and can take it over — which is a materially bigger claim,
// and flattening both into one generic automation prompt throws away the only
// part that is interesting.
func Message(d Decision) Offer {
	p := d.Proposal
	o := Offer{Options: []Option{
		{ActionCreate, "Create", "Write the task as proposed and start running it"},
		{ActionCustomize, "Customize", "Adjust the cadence, authority or verification first"},
		{ActionNotNow, "Not now", "Skip this offer; keep watching for the pattern"},
		{ActionNever, "Don't suggest this kind", "Stop offering this kind of task"},
	}}
	switch d.Kind {
	case KindProspective:
		// Lead with the CAUSE. The claim being made is about the world, not
		// about the user's habits, and stating it lets them judge the reasoning
		// rather than just the verdict — including when the reasoning is wrong.
		o.Headline = fmt.Sprintf("%s I can %s %s and open a PR whenever ours drifts. Create that task?",
			sentence(p.Recurrence.Cause), checkVerb(p), cadence(p))
	case KindRepeated:
		o.Headline = fmt.Sprintf(
			"You've asked me to %s %s %s. I can turn that into an autonomous task and keep it current.",
			lower(p.Operation), lower(p.Target), occasions(d.Occasions, d.Sessions))
	case KindExplicit:
		o.Headline = fmt.Sprintf("Here's the task for that: %s %s, %s.",
			lower(p.Operation), lower(p.Target), cadence(p))
	default:
		o.Headline = "This looks like something memcode could run on its own."
	}
	o.Detail = summary(p)
	return o
}

// checkVerb renders how the task would keep watch.
func checkVerb(p Proposal) string {
	if p.Effect == EffectNone {
		return "check"
	}
	return "check " + lower(p.Target)
}

// sentence tidies the model's causal claim into one.
func sentence(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "This kind of work comes round again."
	}
	s = strings.ToUpper(s[:1]) + s[1:]
	if !strings.HasSuffix(s, ".") && !strings.HasSuffix(s, "!") {
		s += "."
	}
	return s
}

// occasions renders the evidence phrase.
func occasions(signals, sessions int) string {
	switch {
	case signals <= 2:
		return "more than once"
	case sessions >= 2:
		return fmt.Sprintf("%d times across %d sessions", signals, sessions)
	default:
		return fmt.Sprintf("%d times", signals)
	}
}

// summary is the one-line shape of the proposed task, so accepting is an
// informed choice rather than a leap.
func summary(p Proposal) string {
	parts := []string{cadence(p)}
	switch p.Effect {
	case EffectNone:
		parts = append(parts, "read-only")
	default:
		parts = append(parts, "opens a PR when it changes code")
	}
	if len(p.Verify) > 0 {
		parts = append(parts, "verified with "+strings.Join(p.Verify, ", "))
	}
	return strings.Join(parts, " · ")
}

func cadence(p Proposal) string {
	switch {
	case p.SuggestedCron != "":
		return p.SuggestedCron
	case p.SuggestedEvery != "":
		if d, err := time.ParseDuration(p.SuggestedEvery); err == nil {
			switch {
			case d >= 7*24*time.Hour:
				return fmt.Sprintf("every %d days", int(d.Hours()/24))
			case d >= 24*time.Hour:
				return "daily"
			}
			return "every " + p.SuggestedEvery
		}
		return "every " + p.SuggestedEvery
	}
	return "weekly"
}

func lower(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
