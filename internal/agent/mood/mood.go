// Package mood detects interaction *friction* — how a user is engaging with the
// agent — from their terminal input, using a deterministic, dependency-free
// lexical heuristic. No model, no network, no surprise deps (consistent with
// memcode's local-first doctrine and the BM25-over-embeddings choice for recall).
//
// This is NOT a personality judgment about the user. It is a control signal: a
// rising friction reading (caps, expletives, "still broken", repeated
// corrections, interrupts, denials) usually means the agent is on the wrong
// track and should change strategy. And the *intensity* of a direction is part
// of its meaning — an instruction given with force ("do NOT add a paid vendor")
// is a stronger, more durable constraint than a calm aside, so memcode records
// the intensity alongside the direction and weighs it later.
package mood

import (
	"math"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// State is the interaction state inferred for a turn (or the running aggregate).
type State string

const (
	Calm        State = "calm"        // positive / satisfied
	Focused     State = "focused"     // neutral, working — the default
	Curious     State = "curious"     // exploratory: "what if", "I wonder", "how does X work"
	Confused    State = "confused"    // uncertainty, questions, "what / huh / unclear"
	Frustrated  State = "frustrated"  // negative, exasperated
	Angry       State = "angry"       // peak friction: caps + expletives + blame
	Urgent      State = "urgent"      // time pressure: now / asap / immediately
	Discouraged State = "discouraged" // low-energy defeat about the WORK ("stuck", "give up")
)

// NOTE: deliberately no "sad"/"depressed" state. memcode reads interaction
// friction about the task, not the person's emotional health — labeling a user
// "depressed" from terminal text would be an unreliable, out-of-scope judgment.
// "discouraged" captures the work-scoped morale signal that actually changes how
// the agent should respond (reduce scope, show progress, stop piling on).

// Reading is the friction charge of one piece of text (or a smoothed aggregate).
// Valence is -1..+1; Intensity and Frustration are 0..1.
type Reading struct {
	Valence     float64  `json:"valence"`
	Intensity   float64  `json:"intensity"`
	Frustration float64  `json:"frustration"`
	State       State    `json:"state"`
	Signals     []string `json:"signals,omitempty"`
}

// Friction collapses a state into the product-facing gauge level shown in the
// TUI: low (proceed normally), elevated (slow down / clarify), high (stop and
// repair). Deliberately not an emoji mood — it's about the work, not the person.
func Friction(s State) string {
	switch s {
	case Angry:
		return "high"
	case Frustrated, Confused, Urgent, Discouraged:
		return "elevated"
	default: // calm, focused, curious
		return "low"
	}
}

// Go's RE2 has no backreferences, so repeated-letter handling ("noooo",
// "ughhh") is done by hand in collapseRepeats / countLongRuns.

var expletives = map[string]float64{
	"fuck": 1, "fucking": 1, "fucked": 1, "fuckin": 1, "motherfucker": 1, "fck": 0.9, "fuk": 0.9,
	"shit": 0.8, "shitty": 0.8, "bullshit": 0.9, "wtf": 0.9, "wth": 0.7, "wtaf": 0.95,
	"ffs": 0.95, "stfu": 0.95, "omfg": 0.8, "jfc": 0.9, "smh": 0.5, "fml": 0.6, "istg": 0.6,
	"goddamn": 0.9, "goddammit": 0.95, "damn": 0.5, "damnit": 0.6, "dammit": 0.6,
	"hell": 0.4, "crap": 0.4, "ass": 0.5, "asshole": 0.95, "bastard": 0.8, "piss": 0.5, "pissed": 0.7,
	"stupid": 0.6, "idiot": 0.7, "idiotic": 0.7, "moron": 0.7, "useless": 0.6, "af": 0.4,
	"garbage": 0.6, "trash": 0.5, "suck": 0.6, "sucks": 0.6, "sucked": 0.6, "screwed": 0.55,
	"freaking": 0.45, "freakin": 0.45, "frickin": 0.5, "friggin": 0.5, "frigging": 0.5,
	"bloody": 0.4, "bollocks": 0.6, "jeez": 0.35, "christ": 0.4, "heck": 0.25,
}

var negative = map[string]float64{
	"broken": 0.5, "break": 0.35, "breaks": 0.4, "still": 0.35, "again": 0.4, "wrong": 0.5,
	"nope": 0.4, "no": 0.2, "ugh": 0.6, "argh": 0.6, "arg": 0.5, "ergh": 0.6, "grr": 0.7,
	"seriously": 0.5, "ridiculous": 0.7, "terrible": 0.6, "awful": 0.6, "hate": 0.7,
	"worse": 0.5, "annoying": 0.6, "annoyed": 0.6, "frustrating": 0.85, "frustrated": 0.85,
	"angry": 0.7, "fail": 0.4, "failed": 0.45, "failing": 0.45, "doesnt": 0.4, "dont": 0.3,
	"cant": 0.35, "wont": 0.4, "not": 0.15, "didnt": 0.3, "isnt": 0.3, "bad": 0.4,
	"horrible": 0.7, "dumb": 0.6, "pointless": 0.6, "wasting": 0.6, "wasted": 0.6,
	"enough": 0.4, "come on": 0.5, "infuriating": 0.9,
}

// directives that aren't negative on their own but are corrective/commanding.
var corrective = map[string]float64{
	"stop": 0.5, "undo": 0.5, "revert": 0.45, "no": 0.2, "wait": 0.3, "cancel": 0.4,
}

// confusion: genuine uncertainty only. Bare "what"/"how" are deliberately NOT
// here — in this workflow they're usually curiosity ("what if", "how might"),
// handled by the curiosity phrases below. Bare "understand" is excluded for the
// same reason: "trying to understand X", "help me understand the trade-off" are
// DELIBERATIVE, not confused — real confusion is the phrase "don't understand"
// (in the confusion-phrase loop in Score), not the lone word.
var confusion = map[string]float64{
	"confused": 0.7, "confusing": 0.7, "unclear": 0.55, "huh": 0.6, "lost": 0.4,
	"unsure": 0.5, "idk": 0.5, "confuses": 0.6,
}

var urgency = map[string]float64{
	"now": 0.4, "asap": 0.85, "urgent": 0.85, "urgently": 0.85, "quickly": 0.5,
	"hurry": 0.7, "immediately": 0.75, "fast": 0.4, "right now": 0.7, "rn": 0.4,
}

var positive = map[string]float64{
	"thanks": 0.6, "thank": 0.6, "thx": 0.5, "perfect": 0.85, "perfectly": 0.85,
	"great": 0.7, "awesome": 0.85, "amazing": 0.85, "nice": 0.5, "love": 0.75, "loving": 0.7,
	"beautiful": 0.75, "excellent": 0.85, "good": 0.4, "works": 0.5, "working": 0.4,
	"fixed": 0.5, "yes": 0.35, "yay": 0.75, "cool": 0.5, "clean": 0.4, "better": 0.45,
	"appreciate": 0.65, "brilliant": 0.8, "wonderful": 0.8, "sweet": 0.55, "lol": 0.4, "haha": 0.45,
}

var curiosity = map[string]float64{
	"curious": 0.7, "wonder": 0.6, "wondering": 0.6, "interesting": 0.6, "intrigued": 0.7,
	"explore": 0.4, "experiment": 0.4, "idea": 0.35, "hmm": 0.35, "tinker": 0.4, "maybe": 0.2,
	"possibly": 0.25, "thoughts": 0.3, "consider": 0.25, "imagine": 0.3,
}

var discouragement = map[string]float64{
	"stuck": 0.6, "hopeless": 0.85, "defeated": 0.8, "exhausted": 0.6, "overwhelmed": 0.7,
	"sigh": 0.5, "meh": 0.4, "whatever": 0.4, "impossible": 0.6, "struggling": 0.6,
	"burnt": 0.6, "burned": 0.5, "drained": 0.6, "demoralized": 0.85, "defeating": 0.7,
	"hopelessly": 0.85, "tired": 0.35, "behind": 0.3,
}

// secondPerson flags input directed/blaming at the agent ("why did you", "you keep").
var secondPerson = regexp.MustCompile(`\b(you|your|u|ur)\b`)
var blameRe = regexp.MustCompile(`\b(why did you|you keep|you always|you broke|you just|did you even|you never)\b`)

// interrobang matches mixed "?!"/"!?" runs — a strong anger/disbelief cue.
var interrobang = regexp.MustCompile(`\?!|!\?`)

// maxRun returns the length of the longest run of byte c in s ("!!!" → 3).
func maxRun(s string, c byte) int {
	best, cur := 0, 0
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			cur++
			if cur > best {
				best = cur
			}
		} else {
			cur = 0
		}
	}
	return best
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// Score reads a single turn of text.
func Score(text string) Reading {
	lower := strings.ToLower(text)
	words := strings.Fields(text)
	if len(words) == 0 {
		return Reading{State: Focused}
	}

	var capsWords, wordy int
	var explStrength, negStrength, posStrength, confStrength, urgStrength, curStrength, discStrength float64
	freq := map[string]int{}
	sig := map[string]bool{}

	for _, w := range words {
		if hasLetter(w) {
			wordy++
			if isShout(w) {
				capsWords++
			}
		}
		key := lexKey(w)
		if len(key) > 1 {
			freq[key]++
		}
		if v, ok := expletives[key]; ok {
			explStrength += v
			sig["expletive"] = true
		}
		if v, ok := negative[key]; ok {
			negStrength += v
			sig["negative-words"] = true
		}
		if v, ok := corrective[key]; ok {
			negStrength += v * 0.6
			sig["correction"] = true
		}
		if v, ok := confusion[key]; ok {
			confStrength += v
		}
		if v, ok := urgency[key]; ok {
			urgStrength += v
			sig["urgency"] = true
		}
		if v, ok := curiosity[key]; ok {
			curStrength += v
			sig["curiosity"] = true
		}
		if v, ok := discouragement[key]; ok {
			discStrength += v
			sig["discouraged"] = true
		}
		if v, ok := positive[key]; ok {
			posStrength += v
			sig["positive-words"] = true
		}
	}
	// multi-word phrases (apostrophes stripped so "fuck's sake" matches).
	clean := strings.ReplaceAll(lower, "'", "")
	for phrase, v := range map[string]float64{
		"what the fuck": 1.0, "what in the fuck": 1.0, "are you fucking": 0.95, "for fucks sake": 0.95,
		"what the hell": 0.75, "this is insane": 0.7, "this is ridiculous": 0.75,
	} {
		if strings.Contains(clean, phrase) {
			explStrength += v
			sig["expletive"] = true
		}
	}
	for phrase, v := range map[string]float64{"come on": 0.5, "right now": 0.7, "no no": 0.5, "are you kidding": 0.7, "are you serious": 0.6} {
		if strings.Contains(clean, phrase) {
			negStrength += v
		}
	}
	for phrase, v := range map[string]float64{
		"what if": 0.5, "could we": 0.4, "how does": 0.4, "how might": 0.6, "how could": 0.4,
		"i wonder": 0.6, "what about": 0.4, "is it possible": 0.5, "is there a way": 0.5,
	} {
		if strings.Contains(clean, phrase) {
			curStrength += v
			sig["curiosity"] = true
		}
	}
	for phrase, v := range map[string]float64{"give up": 0.7, "giving up": 0.7, "no point": 0.6, "this is hopeless": 0.9, "im stuck": 0.6, "i give up": 0.85, "nothing works": 0.7, "cant do this": 0.7} {
		if strings.Contains(clean, phrase) {
			discStrength += v
			sig["discouraged"] = true
		}
	}
	// Genuine confusion is a PHRASE ("I don't understand", "makes no sense"), not the bare
	// word "understand" (deliberative: "help me understand X") — see the confusion lexicon.
	for phrase, v := range map[string]float64{
		"dont understand": 0.6, "didnt understand": 0.6, "cant understand": 0.6, "not understanding": 0.6,
		"makes no sense": 0.65, "doesnt make sense": 0.65, "no idea what": 0.5, "what does that mean": 0.5,
	} {
		if strings.Contains(clean, phrase) {
			confStrength += v
		}
	}
	// Emphatic repetition = the same word fired BACK-TO-BACK ("no no no", "stop stop") — that's
	// real intensity. Counting raw frequency instead made any common word recurring across a
	// long message ("the", "not", "plan") look emphatic, so a long spec read as agitated.
	maxRepeat := 1
	for i, run := 1, 1; i < len(words); i++ {
		if k := lexKey(words[i]); k != "" && k == lexKey(words[i-1]) {
			run++
			if run > maxRepeat {
				maxRepeat = run
			}
		} else {
			run = 1
		}
	}
	if maxRepeat >= 3 {
		sig["repetition"] = true
	}
	directed := secondPerson.MatchString(lower)
	if directed {
		sig["directed-at-agent"] = true
	}
	if blameRe.MatchString(lower) {
		negStrength += 0.5
		sig["blame"] = true
	}

	capsRatio := 0.0
	if wordy > 0 {
		capsRatio = float64(capsWords) / float64(wordy)
	}
	// Shouting is MAJORITY caps, not a couple of emphasis words: ALL-CAPS emphasis sprinkled
	// through a long message (NOT, STOP, ONE…) is a writing convention, not yelling. Gate on
	// the ratio so a spec/paste with a few capitalized terms isn't read as a shout.
	if capsWords >= 2 && capsRatio >= 0.5 {
		sig["shouting"] = true
	}
	exclaims := strings.Count(text, "!")
	qmarks := strings.Count(text, "?")
	longRepeats := countLongRuns(text) // "noooo", "ughhh"
	if longRepeats > 0 {
		sig["drawn-out"] = true
	}

	// Punctuation heat: !!! / ??? / ?! escalate annoyance → anger; ellipsis and a
	// terse period ("no.", "fine.") read as exasperation. Longer runs = hotter.
	bangMax := maxRun(text, '!')
	qMax := maxRun(text, '?')
	mixedPunct := interrobang.MatchString(text)
	ellipsis := strings.Contains(text, "...") || strings.Contains(text, "…")
	tersePeriod := wordy <= 3 && exclaims == 0 && qmarks == 0 &&
		strings.HasSuffix(strings.TrimSpace(text), ".")
	punctHeat := clamp01(0.25*float64(bangMax) + 0.18*float64(qMax) +
		b2f(mixedPunct)*0.35 + b2f(ellipsis)*0.18 + b2f(tersePeriod)*0.2)
	if bangMax >= 2 || qMax >= 2 || mixedPunct {
		sig["punctuation"] = true
	}
	if ellipsis {
		discStrength += 0.2 // trailing off reads as exasperation
	}

	// Expletive count + density (per token) — "wtf wtf wtf" beats one "wtf".
	explCount := 0
	for k, n := range freq {
		if _, ok := expletives[k]; ok {
			explCount += n
		}
	}
	explDensity := 0.0
	if wordy > 0 {
		explDensity = float64(explCount) / float64(wordy)
	}

	// Long-form damping. A long, structured message (a spec, a pasted brief) piles up
	// negative-word MASS by sheer volume, not heat — enough scattered "wrong/still/again/no/
	// not/stop/cancel" to saturate neg even at low density. Absent ACUTE markers (expletives,
	// majority-caps shouting, agent-directed blame, hot punctuation, drawn-out vowels), scale
	// the ACCUMULATIVE negativity down by length so a constraint-dense brief doesn't read as a
	// frustrated user. Short messages (< longFormWords) are untouched; ratio/density features
	// (caps, expletive density) already self-normalize and aren't damped.
	negDamp := 1.0
	acute := explStrength > 0 || sig["blame"] || punctHeat >= 0.5 || longRepeats > 0 || (capsWords >= 2 && capsRatio >= 0.5)
	if wordy >= longFormWords && !acute {
		negDamp = clamp01(float64(longFormWords) / float64(wordy))
	}

	caps := clamp01(capsRatio)
	expl := clamp01(explStrength)
	neg := clamp01(negStrength * negDamp)
	pos := clamp01(posStrength)
	conf := clamp01(confStrength + 0.3*clamp01(float64(qmarks)/2))
	urg := clamp01(urgStrength)
	cur := clamp01(curStrength)
	disc := clamp01(discStrength * negDamp)
	exc := clamp01(float64(exclaims) / 3)
	rep := clamp01(float64(longRepeats) / 2)
	repWord := clamp01(float64(maxRepeat-2) / 2) // 3×→0.5, 4×→1.0
	// Density only escalates when expletives are *repeated/concentrated* — one
	// lone "wtf" is frustrated, not full rage.
	dens := 0.0
	if explCount >= 2 {
		dens = clamp01(explDensity * 2)
	}

	combo := 0.0 // caps + expletive together = peak anger
	if caps > 0 && expl > 0 {
		combo = W.Combo
	}
	dboost := 0.0 // negativity aimed at the agent stings more
	if directed && (neg > 0 || expl > 0) {
		dboost = W.Directed
	}

	frustration := clamp01(W.Caps*caps + W.Expl*expl + W.Neg*neg + W.Punct*punctHeat +
		W.Rep*rep + W.RepWord*repWord + W.Density*dens + combo + dboost - W.Pos*pos)

	// Short-sharp override: a curt "wtf", "no", "stop", "wrong" is high signal
	// even though it has few tokens to accumulate weight from.
	if wordy <= 3 && (expl > 0 || neg >= 0.3 || sig["correction"]) {
		if frustration < 0.5 {
			frustration = 0.5
		}
		sig["short-sharp"] = true
	}

	intensity := clamp01(0.5*caps + 0.55*expl + 0.4*neg + 0.3*exc + 0.35*rep +
		0.2*repWord + 0.45*pos + 0.4*urg + 0.25*cur + combo)
	valence := clampN(pos + 0.2*cur - (0.85*expl + 0.7*neg + 0.4*caps + 0.2*exc + 0.5*disc))

	return Reading{
		Valence:     round(valence),
		Intensity:   round(intensity),
		Frustration: round(frustration),
		State:       deriveState(frustration, valence, intensity, conf, urg, cur, disc),
		Signals:     keys(sig),
	}
}

// longFormWords is the length past which a message is treated as deliberate prose (a spec or
// paste) rather than a vent: above it, accumulative negativity is damped by length unless acute
// markers say otherwise. Real frustration is dense; a long calm document is not.
const longFormWords = 45

// W is the tunable feature-weight table for friction scoring — kept in one place
// (not scattered through conditionals) so the model can be calibrated easily.
var W = struct {
	Caps, Expl, Neg, Punct, Rep, RepWord, Density, Combo, Directed, Pos float64
}{
	Caps: 0.5, Expl: 0.65, Neg: 0.55, Punct: 0.4, Rep: 0.3, RepWord: 0.25,
	Density: 0.2, Combo: 0.25, Directed: 0.1, Pos: 0.7,
}

func deriveState(frustration, valence, intensity, confusion, urgency, curiosity, discouragement float64) State {
	switch {
	case frustration >= 0.75:
		return Angry
	case discouragement >= 0.5 && frustration < 0.5:
		return Discouraged
	case frustration >= 0.5:
		return Frustrated
	case urgency >= 0.5 && urgency >= confusion:
		return Urgent
	case confusion >= 0.5 && confusion >= curiosity:
		return Confused
	case curiosity >= 0.5:
		return Curious
	case valence >= 0.4:
		return Calm
	default:
		return Focused
	}
}

// Behavior is the recommended agent strategy for a state — the whole point of
// reading friction: change approach before the user has to escalate further.
func Behavior(s State) string {
	switch s {
	case Angry:
		return "stop broad changes; show the current diff/status; acknowledge the correction and confirm before continuing"
	case Frustrated:
		return "slow down; pause speculative edits; summarize what happened and ask before the next step"
	case Discouraged:
		return "reduce scope; show concrete progress; propose one small next step"
	case Confused:
		return "explain briefly; show the plan or map before acting"
	case Curious:
		return "explore openly; reason through the options"
	case Urgent:
		return "minimize narration; act and verify quickly"
	default:
		return "proceed normally"
	}
}

// Friction returns the gauge level for r's state, and Behavior the strategy.
func (r Reading) FrictionLevel() string { return Friction(r.State) }
func (r Reading) Behavior() string      { return Behavior(r.State) }

// Tracker keeps a smoothed running friction across a session so the gauge
// reflects the general interaction rather than one stray message, and detects
// repeated corrections (the same complaint twice = the agent really isn't
// listening, which should raise friction).
type Tracker struct {
	mu     sync.Mutex // the engine goroutine and the TUI both touch the tracker
	alpha  float64
	cur    Reading
	seen   bool
	recent []map[string]struct{} // token sets of the last few turns
}

// NewTracker returns a Tracker with sensible smoothing.
func NewTracker() *Tracker { return &Tracker{alpha: 0.5, cur: Reading{State: Focused}} }

// Observe folds a turn into the running aggregate. raw is the original text, used
// to detect repeated corrections. Returns the smoothed reading.
func (t *Tracker) Observe(r Reading, raw string) Reading {
	t.mu.Lock()
	defer t.mu.Unlock()
	toks := tokenSet(raw)
	if r.Frustration >= 0.3 && t.repeatedNegative(toks) {
		r.Frustration = clamp01(r.Frustration + 0.2)
		r.State = deriveState(r.Frustration, r.Valence, r.Intensity, 0, 0, 0, 0)
		r.Signals = appendUniq(r.Signals, "repeated-correction")
	}
	t.remember(toks)

	if !t.seen {
		t.cur, t.seen = r, true
		return t.cur
	}
	a := t.alpha
	t.cur.Valence = round(a*r.Valence + (1-a)*t.cur.Valence)
	t.cur.Intensity = round(a*r.Intensity + (1-a)*t.cur.Intensity)
	t.cur.Frustration = round(a*r.Frustration + (1-a)*t.cur.Frustration)
	// Smoothed frustration governs the negative end; otherwise the gauge reflects
	// this turn's qualitative state (curious / confused / discouraged / calm).
	if t.cur.Frustration >= 0.5 {
		t.cur.State = deriveState(t.cur.Frustration, t.cur.Valence, t.cur.Intensity, 0, 0, 0, 0)
	} else {
		t.cur.State = r.State
	}
	t.cur.Signals = r.Signals
	return t.cur
}

// Bump nudges friction up for a non-textual signal the runtime observes (an
// interrupt route, an approval denial) and returns the updated aggregate.
func (t *Tracker) Bump(amount float64, signal string) Reading {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.cur.Frustration = round(clamp01(t.cur.Frustration + amount))
	t.cur.State = deriveState(t.cur.Frustration, t.cur.Valence, t.cur.Intensity, 0, 0, 0, 0)
	t.cur.Signals = appendUniq(t.cur.Signals, signal)
	t.seen = true
	return t.cur
}

// Current returns the smoothed reading.
func (t *Tracker) Current() Reading {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.cur
}

func (t *Tracker) repeatedNegative(toks map[string]struct{}) bool {
	for _, prev := range t.recent {
		if jaccard(toks, prev) >= 0.5 {
			return true
		}
	}
	return false
}

func (t *Tracker) remember(toks map[string]struct{}) {
	t.recent = append(t.recent, toks)
	if len(t.recent) > 4 {
		t.recent = t.recent[len(t.recent)-4:]
	}
}

// --- helpers ---

func lexKey(w string) string {
	w = strings.ToLower(strings.Trim(w, ".,;:!?\"'()[]{}*`~"))
	w = strings.ReplaceAll(w, "'", "")
	return collapseRepeats(w)
}

// collapseRepeats shrinks any run of 3+ identical characters to one ("noooo" →
// "no", "ughhh" → "ugh"), leaving normal doubles ("still") intact, so lexicon
// lookups match drawn-out spellings.
func collapseRepeats(s string) string {
	runes := []rune(s)
	var b strings.Builder
	for i := 0; i < len(runes); {
		j := i
		for j < len(runes) && runes[j] == runes[i] {
			j++
		}
		if j-i >= 3 {
			b.WriteRune(runes[i])
		} else {
			for k := i; k < j; k++ {
				b.WriteRune(runes[k])
			}
		}
		i = j
	}
	return b.String()
}

// countLongRuns counts DRAWN-OUT emphasis runs — a letter repeated 3+ times as a stretched
// word ("noooo", "ughhh", "grrr", "yesss"). It counts a run ONLY inside a token that has ≥2
// distinct letters: a single-letter token like "www" (apps/www), "aaa", or "lll" is an
// acronym/path/handle, NOT emphasis — and miscounting it flipped a long technical paste into
// "acute", disabling the long-form damping and reading a calm brief as an angry rant.
func countLongRuns(s string) int {
	isLetter := func(r rune) bool { return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') }
	n := 0
	for _, tok := range strings.FieldsFunc(s, func(r rune) bool { return !isLetter(r) }) {
		runes := []rune(strings.ToLower(tok))
		distinct := map[rune]bool{}
		hasRun := false
		for i := 0; i < len(runes); {
			j := i
			for j < len(runes) && runes[j] == runes[i] {
				j++
			}
			if j-i >= 3 {
				hasRun = true
			}
			distinct[runes[i]] = true
			i = j
		}
		if hasRun && len(distinct) >= 2 { // stretched word, not a single-letter acronym
			n++
		}
	}
	return n
}

func isShout(w string) bool {
	letters, upper := 0, 0
	for _, r := range w {
		switch {
		case r >= 'a' && r <= 'z':
			letters++
		case r >= 'A' && r <= 'Z':
			letters++
			upper++
		}
	}
	return letters >= 2 && upper == letters
}

func hasLetter(w string) bool {
	for _, r := range w {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			return true
		}
	}
	return false
}

func tokenSet(text string) map[string]struct{} {
	m := map[string]struct{}{}
	for _, w := range strings.Fields(text) {
		if k := lexKey(w); len(k) > 1 {
			m[k] = struct{}{}
		}
	}
	return m
}

func jaccard(a, b map[string]struct{}) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	inter := 0
	for k := range a {
		if _, ok := b[k]; ok {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

func appendUniq(s []string, v string) []string {
	for _, x := range s {
		if x == v {
			return s
		}
	}
	return append(s, v)
}

func keys(m map[string]bool) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out) // deterministic order for events + tests
	return out
}

func clamp01(x float64) float64 {
	if x < 0 {
		return 0
	}
	if x > 1 {
		return 1
	}
	return x
}

func clampN(x float64) float64 {
	if x < -1 {
		return -1
	}
	if x > 1 {
		return 1
	}
	return x
}

func round(x float64) float64 { return math.Round(x*100) / 100 }
