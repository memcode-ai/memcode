package structure

import "testing"

func TestAssignHotnessAndRanking(t *testing.T) {
	subs := []Subsystem{
		{Key: "web", Recent: 50, RecentChurn: 500, RecentDays: 20},     // many commits, modest churn
		{Key: "backend", Recent: 8, RecentChurn: 4000, RecentDays: 12}, // few commits, deep churn
		{Key: "idle", Commits: 99},                                     // no recent activity at all
	}
	assignHotness(subs)

	ranked := ByHotness(subs)
	// Deep-churn backend should outrank high-commit-count web (churn is weighted
	// highest), and the idle subsystem (hotness 0) ranks last despite all-time commits.
	if ranked[0].Key != "backend" {
		t.Fatalf("expected backend hottest, got %q (%.3f) vs web %.3f", ranked[0].Key, ranked[0].Hotness, subs[0].Hotness)
	}
	if ranked[len(ranked)-1].Key != "idle" {
		t.Fatalf("idle (no recent activity) should rank last, got %q", ranked[len(ranked)-1].Key)
	}
	if subs[2].Hotness != 0 {
		t.Errorf("idle hotness should be 0, got %f", subs[2].Hotness)
	}
}

func TestByHotnessFallsBackToCommits(t *testing.T) {
	// All cold (no recent activity): ranking falls back to all-time commits.
	subs := []Subsystem{{Key: "a", Commits: 3}, {Key: "b", Commits: 30}}
	assignHotness(subs)
	if ranked := ByHotness(subs); ranked[0].Key != "b" {
		t.Fatalf("cold repo should rank by all-time commits, got %q first", ranked[0].Key)
	}
}
