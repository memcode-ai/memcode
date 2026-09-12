// Package taskdetect notices when work the user just did should become a
// durable capability, and proposes the task that would do it.
//
// Two independent kinds of evidence feed one proposal system:
//
//	a STRONG single turn   this piece of work is plainly repeatable
//	REPEATED weak signals  the same capability keeps being asked for
//
// The second is the one that matters. Anyone can suggest cron. Recognising
// "you have asked me to keep provider catalogs current four times across three
// sessions, shall I just keep them current" is memcode learning a capability
// from working with someone, and it is a different message with a different
// weight.
//
// The detector's output is a PROPOSED TASK, never a boolean. Interrupting
// somebody to ask "want to automate that?" and then making them answer a
// configuration questionnaire is worse than not asking: if there is enough
// confidence to interrupt, there is enough to propose cadence, authority,
// verification and runtime, and let them accept it whole.
package taskdetect

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/memcode-ai/memcode/internal/task"
)

// Effect says what a run of this work does to the repository.
type Effect string

const (
	// EffectNone reads and reports; it changes nothing.
	EffectNone Effect = "none"
	// EffectPossible may change code, depending on what it finds. The common
	// case for maintenance: usually nothing to do, sometimes a real diff.
	EffectPossible Effect = "possible"
	// EffectExpected changes code essentially every run.
	EffectExpected Effect = "expected"
)

// Proposal is a task memcode believes it could take over, with everything
// needed to create it.
type Proposal struct {
	// Family is the SEMANTIC identity of the capability, normalized. It is what
	// lets three differently worded requests accumulate into one recognised
	// thing, so it names a capability rather than an instance:
	// "provider-model-catalog-maintenance", not "update-anthropic-models".
	Family string `json:"task_family"`
	// Operation and Target are the parts Family is built from, kept separately
	// because they are what a human reads when asked to confirm the grouping.
	Operation string `json:"operation"`
	Target    string `json:"target"`
	// Scope and Project bound where the capability applies.
	Scope   string `json:"scope"`
	Project string `json:"project,omitempty"`
	// Constraints are the conditions that must hold for the work to be correct
	// ("verify prices against the vendor's page"). Part of identity: the same
	// operation under materially different constraints is different work.
	Constraints []string `json:"constraints,omitempty"`

	Name         string   `json:"name"`
	Reason       string   `json:"reason"`
	Instructions string   `json:"instructions"`
	Verify       []string `json:"verify,omitempty"`

	// SuggestedEvery is a Go duration ("168h") and SuggestedCron a 5-field
	// expression; at most one is set.
	SuggestedEvery string `json:"suggested_every,omitempty"`
	SuggestedCron  string `json:"suggested_cron,omitempty"`

	Effect     Effect  `json:"code_changes"`
	Confidence float64 `json:"confidence"`

	// The four eligibility gates. Repetition alone is NOT enough: a password
	// reset recurs, a vague "make this better" recurs, an emotionally repetitive
	// question recurs, and none of them is a capability worth handing to an
	// unattended machine.
	Bounded        bool `json:"bounded"`
	Reproducible   bool `json:"reproducible"`
	UnattendedSafe bool `json:"unattended_safe"`
	Evaluable      bool `json:"evaluable"`
}

// Eligible reports whether the work qualifies at all, independent of how
// confident or how often it has been seen.
func (p Proposal) Eligible() bool {
	return p.Bounded && p.Reproducible && p.UnattendedSafe && p.Evaluable
}

// IneligibleReason names the first failing gate, for a log line.
func (p Proposal) IneligibleReason() string {
	switch {
	case !p.Bounded:
		return "not bounded — it has no clear finish"
	case !p.Reproducible:
		return "not reproducible — it depended on this moment"
	case !p.UnattendedSafe:
		return "not safe unattended — it needs a human in the loop"
	case !p.Evaluable:
		return "not evaluable — success cannot be checked"
	}
	return ""
}

var nonIdent = regexp.MustCompile(`[^a-z0-9]+`)

// slug normalizes free text into an identifier fragment.
func slug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = nonIdent.ReplaceAllString(s, "-")
	return strings.Trim(s, "-")
}

// Fingerprint is the durable identity signals accumulate against.
//
// Built from MEANING — operation, target, scope, project, constraints — and not
// from wording. "Update our Anthropic models", "OpenAI added models, update
// ours" and "is the Fireworks catalog stale again?" share no useful phrasing
// and are the same capability; a fingerprint over words would file them as
// three unrelated things and never notice the pattern.
func (p Proposal) Fingerprint() string {
	cons := append([]string(nil), p.Constraints...)
	sort.Strings(cons)
	parts := []string{
		slug(p.Family),
		slug(p.Operation),
		slug(p.Target),
		slug(p.Scope),
		slug(p.Project),
		slug(strings.Join(cons, " ")),
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return hex.EncodeToString(sum[:8])
}

// FamilyKey is the coarser identity used for SUPPRESSION and for grouping
// signals: family plus scope plus project, without the constraint detail.
//
// Coarser on purpose. Someone who says "don't suggest this kind" means the kind,
// not that exact phrasing with that exact constraint list — and an offer that
// returns next week because one condition differed is the behaviour they were
// trying to stop.
func (p Proposal) FamilyKey() string {
	return strings.Join([]string{slug(p.Family), slug(p.Scope), slug(p.Project)}, "|")
}

// familyTokens are the meaningful words of a family name, for the SUPPORTING
// similarity check.
func familyTokens(family string) map[string]bool {
	stop := map[string]bool{"the": true, "a": true, "of": true, "and": true, "for": true, "to": true}
	out := map[string]bool{}
	for _, w := range strings.Split(slug(family), "-") {
		if w != "" && !stop[w] {
			out[w] = true
		}
	}
	return out
}

// familySimilarity is lexical overlap between two family names, 0..1.
//
// SUPPORTING evidence only. It exists to merge a classifier that said
// "provider-model-catalog-maintenance" one week and "provider-catalog-
// maintenance" the next — the same capability named slightly differently. It
// never establishes identity on its own, because word overlap is exactly the
// signal that fails on the cases this package cares about.
func familySimilarity(a, b string) float64 {
	ta, tb := familyTokens(a), familyTokens(b)
	if len(ta) == 0 || len(tb) == 0 {
		return 0
	}
	inter := 0
	for w := range ta {
		if tb[w] {
			inter++
		}
	}
	union := len(ta) + len(tb) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// mergeSimilarity is how alike two family names must be before they are treated
// as the same capability under a different name. High, because a wrong merge
// silently pools unrelated work under one proposal.
const mergeSimilarity = 0.6

// SameCapability reports whether two proposals describe one capability: the
// same family key, or near-identical family names within the same scope and
// project.
func (p Proposal) SameCapability(q Proposal) bool {
	if p.FamilyKey() == q.FamilyKey() {
		return true
	}
	if slug(p.Scope) != slug(q.Scope) || slug(p.Project) != slug(q.Project) {
		return false
	}
	return familySimilarity(p.Family, q.Family) >= mergeSimilarity
}

// ToTask converts a proposal into a real task definition.
//
// Every field is filled from the proposal or from a documented default, so
// accepting an offer writes a COMPLETE, valid task and never opens a
// questionnaire. Anything the proposal did not determine falls to the same
// defaults a hand-written file would get.
func (p Proposal) ToTask(now time.Time) (task.Task, error) {
	t := task.Task{
		Version:      task.Version,
		Name:         taskName(p),
		Description:  strings.TrimSpace(p.Reason),
		Project:      p.Project,
		Instructions: strings.TrimSpace(p.Instructions),
	}
	if tr, ok := p.trigger(); ok {
		t.Triggers = []task.Trigger{tr}
	}
	if len(p.Verify) > 0 {
		t.Verify.Commands = append([]string(nil), p.Verify...)
	}
	// Authority follows the EFFECT the proposal predicted. Work that only reads
	// gets read-only; work that may change code gets the branch tier, which
	// prepares a change for review and can go no further.
	if p.Effect == EffectNone {
		t.Autonomy.Level = task.LevelReadOnly
	} else {
		t.Autonomy.Level = task.LevelBranch
	}
	t.ApplyDefaults()
	if err := t.Validate(now); err != nil {
		return task.Task{}, fmt.Errorf("the proposed task is not valid: %w", err)
	}
	return t, nil
}

// taskName produces a usable task name from the proposal.
func taskName(p Proposal) string {
	n := slug(p.Name)
	if n == "" {
		n = slug(p.Family)
	}
	if n == "" {
		n = "autonomous-task"
	}
	// Task names become filenames, branch names and lock keys; keep them short.
	if len(n) > 48 {
		n = strings.Trim(n[:48], "-")
	}
	return n
}

// trigger renders the suggested cadence, defaulting to weekly.
//
// A proposal with no cadence still gets one: a task created from an offer and
// left manual-only would never run, which is not what the user agreed to when
// they said yes to "keep them current automatically".
func (p Proposal) trigger() (task.Trigger, bool) {
	switch {
	case strings.TrimSpace(p.SuggestedCron) != "":
		return task.Trigger{Cron: strings.TrimSpace(p.SuggestedCron), Missed: task.MissedRunOnce}, true
	case strings.TrimSpace(p.SuggestedEvery) != "":
		return task.Trigger{Every: strings.TrimSpace(p.SuggestedEvery), Missed: task.MissedRunOnce}, true
	}
	return task.Trigger{Every: "168h", Missed: task.MissedRunOnce}, true
}
