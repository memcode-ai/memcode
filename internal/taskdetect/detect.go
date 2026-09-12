package taskdetect

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Thresholds. The two paths answer different questions, so they have different
// bars.
const (
	// strongConfidence is what one turn must reach to be offered on its own.
	// High: interrupting someone about a single piece of work is only worth it
	// when the work is obviously a standing job, and the cost of being wrong is
	// that the feature reads as noise.
	strongConfidence = 0.85

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
	// KindSingleTurn: this one piece of work is plainly a standing job.
	KindSingleTurn Kind = "single_turn"
	// KindRepeated: the same capability has been asked for across sessions.
	// The interesting one — memcode recognising something it learned from
	// working with someone, rather than spotting a candidate for cron.
	KindRepeated Kind = "repeated"
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

	if _, err := d.Store.Record(ctx, in.SessionID, p, clip(in.Request, 200), now); err != nil {
		return Decision{}, err
	}

	// PATH 1 — one turn, strong enough on its own.
	if p.Confidence >= strongConfidence {
		return Decision{Kind: KindSingleTurn, Proposal: p}, nil
	}

	// PATH 2 — accumulated across sessions.
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
			"not yet a pattern (%d occasion(s) across %d session(s), weight %.2f)",
			c.Signals, c.Sessions, c.Weight)}, nil
	}
	return Decision{Why: "first time seeing this"}, nil
}

// Ready reports whether a cluster has enough evidence to be worth offering.
// The same bar Evaluate applies, exported so the suggestion surface and the
// detector cannot drift into disagreeing about what counts.
func Ready(c Cluster) bool {
	return c.Latest.Eligible() && c.Sessions >= repeatSessions && c.Weight >= repeatWeight
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
	case KindRepeated:
		o.Headline = fmt.Sprintf(
			"You've asked me to %s %s %s. I can turn that into an autonomous task and keep it current.",
			lower(p.Operation), lower(p.Target), occasions(d.Occasions, d.Sessions))
	default:
		o.Headline = "This looks like something memcode could run on its own."
	}
	o.Detail = summary(p)
	return o
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
