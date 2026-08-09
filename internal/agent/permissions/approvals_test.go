package permissions

import (
	"testing"
	"time"
)

func TestGlobMatch(t *testing.T) {
	cases := []struct {
		pattern, s string
		want       bool
	}{
		{"go test ./...", "go test ./...", true},
		{"go test ./...", "go test ./pkg", false},
		{"npm i *", "npm i zod", true},
		{"npm i *", "npm install zod", false},
		{"go *", "go build ./...", true},
		{"*test*", "cd x && go test", true},
		{"git *", "rm -rf /", false},
		{"*", "anything at all", true},
	}
	for _, c := range cases {
		if got := globMatch(c.pattern, c.s); got != c.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", c.pattern, c.s, got, c.want)
		}
	}
}

func TestMatch(t *testing.T) {
	now := time.Now()
	past := now.Add(-time.Hour)
	future := now.Add(time.Hour)

	approvals := []Approval{
		{ID: "a", Pattern: "go test ./...", Cwd: ""},
		{ID: "b", Pattern: "npm i *", Cwd: "apps/www"},
		{ID: "c", Pattern: "expired", ExpiresAt: &past},
		{ID: "d", Pattern: "live", ExpiresAt: &future},
		{ID: "e", Pattern: "rm -rf *", Trusted: true},
	}

	if a, ok := Match(approvals, "go test ./...", "", false, now); !ok || a.ID != "a" {
		t.Errorf("expected match a, got %v %v", a, ok)
	}
	// cwd-scoped rule only matches its cwd.
	if _, ok := Match(approvals, "npm i zod", "cli", false, now); ok {
		t.Error("cwd-scoped rule should not match a different cwd")
	}
	if _, ok := Match(approvals, "npm i zod", "apps/www", false, now); !ok {
		t.Error("cwd-scoped rule should match its cwd")
	}
	// expiry honored.
	if _, ok := Match(approvals, "expired", "", false, now); ok {
		t.Error("expired rule should not match")
	}
	if _, ok := Match(approvals, "live", "", false, now); !ok {
		t.Error("unexpired rule should match")
	}
	// catastrophic only matches a trusted rule.
	if _, ok := Match(approvals, "rm -rf /tmp/x", "", true, now); !ok {
		t.Error("trusted rule should match a catastrophic command")
	}
	if _, ok := Match([]Approval{{Pattern: "rm -rf *"}}, "rm -rf /", "", true, now); ok {
		t.Error("untrusted rule must not match a catastrophic command")
	}
}

// TestMatchCompoundRiskHead is the regression for the "it forgot I just approved"
// bug: a rule saved from a compound keys on the escalating sub-command (rememberPattern
// → RiskHead), so Match must fire it even though the line doesn't START with that binary.
func TestMatchCompoundRiskHead(t *testing.T) {
	now := time.Now()
	rules := []Approval{{Pattern: "supabase *"}}

	// The exact shape that re-prompted: echo header + cd + the real command.
	cmd := `echo "=== tables ===" && cd /repo/apps/www && supabase db query --sql "select 1"`
	if _, ok := Match(rules, cmd, "", false, now); !ok {
		t.Errorf("rule %q should match compound whose risk head is supabase: %q", rules[0].Pattern, cmd)
	}

	// Safety: a higher-risk uncovered sibling must still re-prompt — RiskSegment anchors
	// at the riskiest call (rm -rf, catastrophic here), which the untrusted rule can't match.
	cat := `rm -rf /tmp/x && supabase status`
	if _, ok := Match(rules, cat, "", true, now); ok {
		t.Errorf("untrusted %q must not auto-approve a compound containing a catastrophic command: %q", rules[0].Pattern, cat)
	}
}
