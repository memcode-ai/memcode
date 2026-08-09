package mood

import "testing"

// A single-letter token like "www" (apps/www) is an acronym/path, NOT drawn-out emphasis —
// miscounting it flipped a long calm paste to "acute", killing the long-form damping. Real
// stretched words still count.
func TestDrawnOutIgnoresAcronyms(t *testing.T) {
	if countLongRuns("clone it into apps/www and ~/www too") != 0 {
		t.Error("www / single-letter runs must not count as drawn-out")
	}
	for _, s := range []string{"noooo", "ughhh", "grrr", "yesss"} {
		if countLongRuns(s) != 1 {
			t.Errorf("%q is drawn-out emphasis and must count", s)
		}
	}
}

// A long, low-density technical paste (a /plan brief: caps acronyms, sparse "NOT/no/stop",
// "apps/www" paths, "you") must NOT read as an angry user — it's input, not a vent. Pre-fix,
// "www" flipped the message to "acute" and disabled the length damping, so the sparse
// negatives read as rage. Regression for the recurring bogus user_friction_high on a brief.
func TestLongTechnicalPasteIsNotAngry(t *testing.T) {
	// Realistic length + density: mostly neutral build instructions, a few directives.
	brief := `Goal: ship the web app in apps/www end to end by cloning the existing web app as a template.
Keep its generic machinery, rebrand it, and build the app around the installed skills under apps/www.
The full path: marketing to pricing to sign up and log in with Google or GitHub to a forced choose a plan
step to using the app and then downloading the CLI and logging in so the account subscription is verified.
Reference (read only, do not modify): mirror its structure, the supabase auth, the account and org model,
the stripe integration, the CLI login flow, the shadcn UI, and the layout. Target this repo: apps/www for
the web, the CLI for login, and a new supabase schema as migrations. The supabase CLI is linked to the
project, so author schema as migrations and apply them; the stripe CLI is authenticated, so create the two
prices and wire the webhook. Architecture: Google and GitHub OAuth only, no email or password. All supabase
access goes through Next.js API routes; the routes own the supabase client. CLI login copies the existing
flow: the CLI opens the browser, the API mints a token, and the token verifies the org subscription on use.
Approach: clone the template, then copy, adapt, gut, and add. Copy the account and org model, the workspace
schema, the auth, the CLI login, the UI, the sidebar, and the feature areas. Gut the domain intelligence and
the contact sales flow. Adapt the landing page, the copy, the logos, and the metadata. Add the billing schema,
per seat billing, self serve pricing, and usage limits. Decisions: reuse the stripe account, two per seat
tiers, orgs and teams with invite codes, and a required subscription. You should verify the full path end to
end before declaring it done, then stop for review. Build it phase by phase rather than in one pass.`
	if r := Score(brief); Friction(r.State) != "low" {
		t.Errorf("a long technical paste must not escalate friction, got state=%s friction=%s frus=%.2f signals=%v",
			r.State, Friction(r.State), r.Frustration, r.Signals)
	}
}

func TestScoreStates(t *testing.T) {
	cases := []struct {
		text    string
		want    State
		minFrus float64 // -1 = don't check
	}{
		{"WHAT THE FUCK WHY DID YOU DO THAT??", Angry, 0.75},
		{"WHY THE FUCK IS THIS STILL BROKEN???", Angry, 0.75},
		{"wtf", Frustrated, 0.5}, // short-sharp override
		{"no no no stop", Frustrated, 0.5},
		{"this is still wrong, doesn't work again", Frustrated, 0.5},
		{"what if we build a graph compiler?", Curious, -1},
		{"how might we make this work?", Curious, -1},
		{"huh I don't get this", Confused, -1},
		{"I don't understand why this fails", Confused, -1}, // genuine confusion: the PHRASE
		// Deliberative architecture question — "trying to understand … should I go with A or B?"
		// is NOT confusion just because it contains "understand" + a "?". Regression: this used to
		// score Confused → elevated friction → a bogus user_friction_high hint on turn one.
		{"hey, I'm trying to understand... should I go with supabase or just stick it in gcp cloud sql?", Focused, -1},
		{"help me understand the routing trade-off here", Focused, -1},
		{"ugh this is hopeless", Discouraged, -1},
		{"please fix this asap", Urgent, -1},
		{"great this works", Calm, -1},
		{"ok thanks, that works perfectly", Calm, -1},
		{"add a helper to parse the config file", Focused, -1},
	}
	for _, c := range cases {
		got := Score(c.text)
		if got.State != c.want {
			t.Errorf("Score(%q).State = %q (frus %.2f), want %q\n  signals=%v",
				c.text, got.State, got.Frustration, c.want, got.Signals)
		}
		if c.minFrus >= 0 && got.Frustration < c.minFrus {
			t.Errorf("Score(%q).Frustration = %.2f, want >= %.2f", c.text, got.Frustration, c.minFrus)
		}
	}
}

// A long, constraint-dense spec/brief is NOT frustration: it piles up scattered negative
// words ("wrong", "not", "stop", "cancel", "avoid", "never") by sheer volume, but it isn't a
// vent. It must not read as Frustrated/Angry (which would put memcode into repair mode and
// escalate the turn). The fix is length-aware damping of accumulative negativity.
func TestLongSpecIsNotFrustration(t *testing.T) {
	brief := "Plan a new yolo command, an autonomous plan-gated build for tasks I am happy to walk away " +
		"from. The shape is clarify then plan then permission-gate then execute. The plan is our drift guard, " +
		"since a hands-off run with no contract is where a long loop wanders off and does the wrong thing. " +
		"It should not be a blind goal. Build it as a thin variant of the plan flow we already have, and do " +
		"not build a new subsystem. Reuse the whole pipeline: research scouts, the reflect gate, one batched " +
		"clarifying round through the ask card, synthesis on the planner model, the tooled cross-model " +
		"reviewer that audits the plan against the code, the approval selector, and apply turns on the " +
		"relaxed apply doctrine that already waits out a vllm boot so execution runs on the cheap lane. The " +
		"deltas are a yolo-flavored planning prompt that front-loads the constraints, a permission-mode " +
		"selector at plan-ready, and a run-scoped mode override that restores my normal mode afterward. The " +
		"selector must still stop for dangerous operations and never let the catastrophic floor be bypassed; " +
		"cancel simply leaves it. Do not make allow-all the sticky session default. Use exactly one batched " +
		"clarifying round, then commit, with no drip of follow-up questions, so the build does not stall " +
		"waiting on me. Report only the build facts we actually track, and have the plan name the tests it " +
		"will add so the unattended build proves itself before it finishes."
	r := Score(brief)
	if r.State == Frustrated || r.State == Angry {
		t.Errorf("a long calm spec must not read as %q (frus %.2f, signals %v)", r.State, r.Frustration, r.Signals)
	}
	if r.Frustration >= 0.5 {
		t.Errorf("long-spec frustration = %.2f, want < 0.5 (damped)", r.Frustration)
	}
	// ALL-CAPS emphasis words scattered in the long spec must not flag shouting.
	shout := Score(brief + " This is NOT optional and you must STOP at the catastrophic floor EVERY time.")
	for _, s := range shout.Signals {
		if s == "shouting" {
			t.Errorf("a few emphasis-caps in a long message must not flag shouting: %v", shout.Signals)
		}
	}
}

// Damping must NOT rescue a genuinely heated long message: expletives/blame keep it acute.
func TestLongRantStillScores(t *testing.T) {
	rant := "this is so fucking broken, why did you do that, it is still wrong and it keeps failing " +
		"every single time, this whole approach is garbage and I am so frustrated with all of this nonsense " +
		"that keeps wasting my time over and over again here"
	r := Score(rant)
	if r.State != Frustrated && r.State != Angry {
		t.Errorf("a long ACUTE rant must still score frustrated/angry, got %q (frus %.2f)", r.State, r.Frustration)
	}
}

func TestPositiveIsNotNegative(t *testing.T) {
	r := Score("this is great, love it, thank you")
	if r.Frustration > 0.2 {
		t.Errorf("positive text scored frustrated: %.2f (%v)", r.Frustration, r.Signals)
	}
	if r.Valence <= 0 {
		t.Errorf("positive text valence = %.2f, want > 0", r.Valence)
	}
}

func TestPunctuationEscalates(t *testing.T) {
	calm := Score("its broken").Frustration
	hot := Score("its broken!!!").Frustration
	hotter := Score("its broken?!?!").Frustration
	if !(hot > calm) {
		t.Errorf("!!! did not escalate: %.2f vs %.2f", hot, calm)
	}
	if !(hotter >= hot) {
		t.Errorf("?!?! did not escalate: %.2f vs %.2f", hotter, hot)
	}
}

func TestFrictionLevels(t *testing.T) {
	if Friction(Angry) != "high" {
		t.Error("angry should be high friction")
	}
	if Friction(Frustrated) != "elevated" || Friction(Discouraged) != "elevated" {
		t.Error("frustrated/discouraged should be elevated")
	}
	if Friction(Calm) != "low" || Friction(Curious) != "low" || Friction(Focused) != "low" {
		t.Error("calm/curious/focused should be low friction")
	}
}

func TestTrackerSmoothsAndSpikes(t *testing.T) {
	tr := NewTracker()
	// A calm baseline...
	tr.Observe(Score("add a function please"), "add a function please")
	// ...then a sudden expletive should move the aggregate up but the running
	// value stays below the single-turn spike (smoothing).
	turn := Score("WHAT THE FUCK")
	agg := tr.Observe(turn, "WHAT THE FUCK")
	if agg.Frustration <= 0.1 {
		t.Errorf("aggregate did not react to anger: %.2f", agg.Frustration)
	}
	if agg.Frustration >= turn.Frustration {
		t.Errorf("aggregate %.2f should be smoothed below the spike %.2f", agg.Frustration, turn.Frustration)
	}
}

func TestRepeatedCorrectionRaisesFriction(t *testing.T) {
	tr := NewTracker()
	first := Score("that's wrong, fix the parser")
	tr.Observe(first, "that's wrong, fix the parser")
	// Same complaint again → repeated-correction bump.
	second := Score("still wrong, fix the parser")
	out := tr.Observe(second, "still wrong, fix the parser")
	found := false
	for _, s := range out.Signals {
		if s == "repeated-correction" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected repeated-correction signal, got %v", out.Signals)
	}
}

func TestBump(t *testing.T) {
	tr := NewTracker()
	tr.Observe(Score("ok"), "ok")
	before := tr.Current().Frustration
	after := tr.Bump(0.3, "interrupt").Frustration
	if after <= before {
		t.Errorf("Bump did not raise friction: %.2f -> %.2f", before, after)
	}
}
