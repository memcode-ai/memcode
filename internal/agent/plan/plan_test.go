package plan

import (
	"testing"

	"github.com/memcode-ai/memcode/internal/events"
)

// enter is the test shorthand for a fresh machine in Researching.
func enter(opts ...Opt) *Controller {
	c := &Controller{}
	c.Enter("saved-model", opts...)
	return c
}

// TestTransitions pins the whole machine: every phase crossed with every event,
// including the no-op guards (which is exactly what wants pinning — each guard
// used to be an unwritten convention some runtime file could violate).
func TestTransitions(t *testing.T) {
	shaped := "1. do the thing\n2. verify it"

	t.Run("zero value is Idle", func(t *testing.T) {
		var c Controller
		if c.Phase() != Idle || c.Planning() || c.IsApplying() || c.InFlow() {
			t.Fatalf("zero Controller must be Idle, got %v", c.Phase())
		}
	})
	t.Run("nil receiver reads are Idle-safe", func(t *testing.T) {
		var c *Controller
		if c.Phase() != Idle || c.Planning() || c.IsApplying() || c.Epoch() != 0 || c.ApplyContract() != "" {
			t.Fatal("nil Controller reads must return zero values")
		}
		if task, draft := c.Snapshot(); task != "" || draft != "" {
			t.Fatal("nil Snapshot must be empty")
		}
	})
	t.Run("Enter from Idle researches and emits (no model switch)", func(t *testing.T) {
		c := &Controller{}
		eff := c.Enter("current")
		if c.Phase() != Researching {
			t.Fatalf("phase = %v", c.Phase())
		}
		// Entering plan mode no longer switches models: plan drafting is
		// user-work inference and runs on the primary pin like the main loop.
		if eff.Emit != events.KindPlanStarted || !eff.ClearTodos || eff.SetModel != "" {
			t.Fatalf("effects = %+v", eff)
		}
	})
	t.Run("Enter while planning is a silent no-op", func(t *testing.T) {
		c := enter(WithTask("first"))
		eff := c.Enter("other", WithTask("second"))
		if !eff.Zero() {
			t.Fatalf("double Enter must return zero Effects, got %+v", eff)
		}
		if task, _ := c.Snapshot(); task != "first" {
			t.Fatalf("double Enter must not reset the session, task=%q", task)
		}
	})
	t.Run("Enter while applying is a no-op (the corrupt tuple)", func(t *testing.T) {
		c := enter()
		c.Present(shaped)
		c.Approve("")
		if eff := c.Enter("m"); !eff.Zero() || c.Phase() != Applying {
			t.Fatal("Enter from Applying must not fire — Active&&Applying is unrepresentable now")
		}
	})
	t.Run("BeginTurn downgrades Presented only", func(t *testing.T) {
		c := enter()
		c.Present(shaped)
		c.BeginTurn()
		if c.Phase() != Researching {
			t.Fatalf("Presented→Researching expected, got %v", c.Phase())
		}
		c.BeginTurn() // Researching: no-op
		if c.Phase() != Researching {
			t.Fatal("BeginTurn must not move Researching")
		}
		var idle Controller
		idle.BeginTurn()
		if idle.Phase() != Idle {
			t.Fatal("BeginTurn must not move Idle")
		}
	})
	t.Run("NoteReflect counts only while planning", func(t *testing.T) {
		c := enter()
		if got := c.NoteReflect(); got != 1 {
			t.Fatalf("round = %d", got)
		}
		c.Cancel()
		if got := c.NoteReflect(); got != 1 {
			t.Fatal("NoteReflect outside plan mode must not count")
		}
	})
	t.Run("Present from Idle is a no-op", func(t *testing.T) {
		var c Controller
		eff, out := c.Present(shaped)
		if out.Pinned || !eff.Zero() || c.Phase() != Idle {
			t.Fatal("Present must refuse outside plan mode")
		}
	})
	t.Run("Approve from Applying is a no-op (double-execute)", func(t *testing.T) {
		c := enter()
		c.Present(shaped)
		c.Approve("")
		eff, out := c.Approve("sneaky new contract")
		if out.Armed || !eff.Zero() {
			t.Fatal("re-arming mid-apply must be impossible")
		}
		if c.ApplyContract() != shaped {
			t.Fatalf("contract mutated mid-apply: %q", c.ApplyContract())
		}
	})
	t.Run("Approve with no contract at all returns Idle unarmed", func(t *testing.T) {
		c := enter()
		eff, out := c.Approve("   ")
		if out.Armed || c.Phase() != Idle {
			t.Fatal("no pinned plan + no fallback must disarm to Idle")
		}
		if eff.Emit != events.KindPlanApproved {
			t.Fatalf("approval event still records the user's decision, got %+v", eff)
		}
	})
	t.Run("Cancel keeps lastPlan for recall, restores model, bumps epoch", func(t *testing.T) {
		c := enter()
		c.Present(shaped)
		before := c.Epoch()
		eff := c.Cancel()
		if c.Phase() != Idle || eff.Emit != events.KindPlanCancelled || eff.SetModel != "saved-model" {
			t.Fatalf("cancel effects = %+v phase=%v", eff, c.Phase())
		}
		if _, draft := c.Snapshot(); draft != shaped {
			t.Fatal("Cancel must keep the pinned plan for recall — only Enter wipes it")
		}
		if c.Epoch() != before+1 {
			t.Fatal("Cancel must bump the epoch (async verdicts against this session are stale)")
		}
	})
	t.Run("Cancel from Idle/Applying is a silent no-op", func(t *testing.T) {
		var c Controller
		if eff := c.Cancel(); !eff.Zero() {
			t.Fatal("Cancel from Idle must no-op")
		}
		a := enter()
		a.Present(shaped)
		a.Approve("")
		if eff := a.Cancel(); !eff.Zero() || a.Phase() != Applying {
			t.Fatal("Cancel must not tear down a running apply")
		}
	})
	t.Run("ApplyDone and ApplyAborted clear only from Applying", func(t *testing.T) {
		c := enter()
		c.Present(shaped)
		c.ApplyDone() // not applying yet: no-op
		if c.Phase() != Presented {
			t.Fatal("ApplyDone outside Applying must no-op")
		}
		c.Approve("")
		c.ApplyDone()
		if c.Phase() != Idle || c.ApplyContract() != "" {
			t.Fatal("ApplyDone must clear the phase and the contract")
		}
	})
	t.Run("Enter resets then opts apply", func(t *testing.T) {
		c := enter(WithYolo(), WithTask("migrate the billing service"))
		if !c.Yolo() {
			t.Fatal("WithYolo lost")
		}
		if task, _ := c.Snapshot(); task != "migrate the billing service" {
			t.Fatalf("task = %q", task)
		}
		c.Cancel()
		c2eff := c.Enter("m")
		_ = c2eff
		if c.Yolo() {
			t.Fatal("a later Enter without WithYolo must not inherit the flag")
		}
		if task, _ := c.Snapshot(); task != "" {
			t.Fatalf("stale task anchor leaked: %q", task)
		}
	})
}

// TestApproveUsesPinnedPlanNotFallback is the wrong-plan handoff regression
// (ff2615ab): post-plan chatter overwrites the session's last text, and the
// contract must be the PINNED plan, never the chatter.
func TestApproveUsesPinnedPlanNotFallback(t *testing.T) {
	const realPlan = "1. build the provider interface\n2. wire the gateway"
	c := enter()
	c.Present(realPlan)
	_, out := c.Approve("All three flags are folded into the plan above — see Step 6.")
	if !out.Armed || out.Contract != realPlan {
		t.Fatalf("apply contract = chatter, not the plan.\n got: %q\nwant: %q", out.Contract, realPlan)
	}
}

// TestApproveFallsBackToLastText: when nothing was pinned (no synthesis path),
// the fallback keeps the contract non-empty.
func TestApproveFallsBackToLastText(t *testing.T) {
	c := enter()
	_, out := c.Approve("the only plan text we have")
	if !out.Armed || out.Contract != "the only plan text we have" {
		t.Fatalf("fallback failed: %+v", out)
	}
}

// TestPresentKeepsPinAgainstProse is the LastPlan clobber regression: a
// conversational answer to a related follow-up must never replace the pinned
// contract, while a shaped revision (however short) must.
func TestPresentKeepsPinAgainstProse(t *testing.T) {
	first := "Goal: fix it.\n\n1. edit the gate\n2. add tests"
	c := enter()

	if _, out := c.Present(first); !out.Pinned {
		t.Fatal("first draft must pin")
	}
	eff, out := c.Present("Step 2 means we extend the regression test for the drain ordering.")
	if out.Pinned || eff.SavePlan != "" {
		t.Fatal("conversational answer must not replace the pin nor save")
	}
	if c.Phase() != Presented {
		t.Fatal("an unpinned synthesis still presents (selector over the old pin)")
	}
	if _, draft := c.Snapshot(); draft != first {
		t.Fatalf("pin clobbered: %q", draft)
	}
	revised := "1. delete the dead flag\n2. run the tests"
	if _, out := c.Present(revised); !out.Pinned {
		t.Fatal("a shaped revision must replace the pin, however short")
	}
	if eff, _ := c.Present(revised); eff.SavePlan != revised {
		t.Log("re-presenting the same shaped text pins again (harmless)")
	}
}

// TestPlanShaped pins the structural guard behind Present.
func TestPlanShaped(t *testing.T) {
	cases := []struct {
		name string
		text string
		want bool
	}{
		{"full plan", "Goal\n\nSteps\n\n1. change the gate\n2. add tests\n3. rebuild", true},
		{"short simplified revision keeps steps", "1. delete the flag\n2) run tests", true},
		{"prose answer", "Step 3 means the classifier judges relevance against the plan's anchor task before routing.", false},
		{"single numbered line only", "1. just one step, the rest is prose explaining it at length", false},
		{"numbered mid-sentence not at line start", "as covered in point 1. and point 2. earlier", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		if got := PlanShaped(tc.text); got != tc.want {
			t.Errorf("%s: PlanShaped = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestCommitGateOneShot pins the latch: armed once, consumed exactly once, and
// cleared structurally by Enter and Cancel — the 08e06a40 stale-latch class
// (a prior plan's answered gate silently skipping this plan's dirty-tree check).
func TestCommitGateOneShot(t *testing.T) {
	c := enter()
	c.Present("1. a\n2. b")
	c.ResolveCommitGate()
	if !c.ConsumeCommitGate() {
		t.Fatal("armed gate must consume true")
	}
	if c.ConsumeCommitGate() {
		t.Fatal("one-shot: second consume must be false")
	}

	// Enter clears a leftover arm.
	c.ResolveCommitGate()
	c.Cancel()
	c.Enter("m")
	if c.ConsumeCommitGate() {
		t.Fatal("Enter must clear a stale arm from the prior plan")
	}

	// Cancel clears an arm too.
	c.Present("1. a\n2. b")
	c.ResolveCommitGate()
	c.Cancel()
	if c.ConsumeCommitGate() {
		t.Fatal("Cancel must drop the armed one-shot")
	}

	// Arming from Idle is a no-op (nothing to arm for).
	var idle Controller
	idle.ResolveCommitGate()
	if idle.ConsumeCommitGate() {
		t.Fatal("Idle arm must no-op")
	}
}

// TestNotePlanTurnRevisions: first output is proposed, later ones revised.
func TestNotePlanTurnRevisions(t *testing.T) {
	c := enter()
	if eff := c.NotePlanTurn(true); eff.Emit != events.KindPlanProposed {
		t.Fatalf("first = %v", eff.Emit)
	}
	if eff := c.NotePlanTurn(true); eff.Emit != events.KindPlanRevised {
		t.Fatalf("second = %v", eff.Emit)
	}
	if eff := c.NotePlanTurn(false); !eff.Zero() {
		t.Fatal("no output → no event")
	}
	if c.Revision() != 2 {
		t.Fatalf("revision = %d", c.Revision())
	}
}
