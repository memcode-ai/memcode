package doctrine

import "testing"

// Ported verbatim from api/internal/server/prompts_size_test.go (one-wire
// Phase B) — same budgets, same inputs; the gateway copy keeps its own until
// Phase D. Doctrine rides every call in its mode — its size is baseline
// context. Measured 2026-07-13: chat ~13.0KB (~3.2k tokens), exec ~12.2KB,
// plan ~9.2KB, apply ~9.5KB. Ceilings ~20% above: hitting one means doctrine
// crept — tighten the prose or raise the ceiling consciously in the same
// commit (in BOTH copies while the gateway one exists).
//
// 2026-07-23: law 14 grew a confirmed-standing-preference exception (~275B,
// coreLaws — rides chat/exec/apply) so the agent stops re-asking about a
// follow-through the user already promoted to a standing preference. exec/chat
// still fit the existing ceiling; apply's was already near its cap, so its
// ceiling is raised here, consciously, in this same commit.
//
// 2026-07-25: law 15 added (secrets hygiene, ~430B, coreLaws — rides
// chat/exec/apply): never expose credentials in output, redact what surfaces,
// manage keys by reference. Ceilings for the three coreLaws modes raised
// consciously in this same commit.
func TestDoctrineBudgets(t *testing.T) {
	facts := map[string]string{
		"root": "/x", "platform": "darwin/arm64", "shell": "zsh",
		"overview": "Subsystems: a, b", "pack": "{}", "plan": "1. step",
	}
	budgets := map[string]int{
		// chat/exec/apply raised (2026-09-12) for core law 15, the standing-
		// responsibility judgement. It earns ~600B in all three because it is
		// the rule that decides whether a request produces work or produces an
		// automation, and getting that wrong is invisible: the user is answered,
		// and then re-asks the same thing next month. apply is the mode least
		// likely to need it, but the laws are shared on purpose — forking them
		// per mode is how two prompts drift into disagreeing about the rules.
		// chat raised 17_000 -> 17_700 (2026-09-12) for automationDoctrine.
		// chat only: it governs how the AUTONOMOUS WORK block is surfaced at
		// session start, and no other mode ever sees that block. Its length is
		// almost entirely the "then stop" half — mention it once and move on —
		// because the failure it prevents is a model that turns background
		// state into an interrogation at the top of every session.
		"chat": 17_700, "exec": 16_000, "plan": 11_100, "apply": 13_000,
		"review": 2_400, "compact": 1_700, "distill": 1_600,
		// task_shape is the largest judge on purpose: it makes TWO independent
		// judgements (is this automatable, and is there a causal reason it will
		// recur) and carries the recurrence taxonomy that keeps the second from
		// collapsing into "it looks scriptable". Raised deliberately from 1_500
		// when prospective reasoning was added; it sits just under review's 2_400.
		"task_shape": 2_800, "adhere": 1_100, "classify": 900, "extract": 900, "facts": 1_100,
		"turn_intent": 2_500,
	}
	for mode, budget := range budgets {
		stable, _, err := Compose(mode, facts, "", "gpt-5.6-sol", false)
		if err != nil {
			t.Errorf("Compose(%s): %v", mode, err)
			continue
		}
		if len(stable) > budget {
			t.Errorf("%s doctrine = %dB (~%d tokens) — over its %dB budget", mode, len(stable), len(stable)/4, budget)
		}
	}
}
