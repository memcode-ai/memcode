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
		"chat": 16_100, "exec": 15_100, "plan": 11_100, "apply": 12_300,
		"review": 2_400, "compact": 1_700, "distill": 1_600, "adhere": 1_100, "classify": 900, "extract": 900, "facts": 1_100,
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
