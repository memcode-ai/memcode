package permissions

import "testing"

// RiskHead must name the sub-command that DROVE the escalation, not the leading token.
// The motivating bug: a read-only compound prompted, and the approval card + saved rule
// keyed on the leading `echo` — a useless, mildly-unsafe "echo *" rule. RiskHead returns
// the highest-risk head instead, so the card shows (and the rule keys on) the real cause.
func TestRiskHead(t *testing.T) {
	cases := []struct {
		cmd  string
		want string
	}{
		// Safe noise (echo/cat/ls) around one genuinely-unknown binary → the unknown.
		{`echo "=== status ===" && cat x; ls -la; mycli deploy`, "mycli"},
		// Leading unknown, trailing safe — still the unknown.
		{`mycli deploy && echo done`, "mycli"},
		// All-safe compound → falls back to the leading head (its own binary).
		{`echo hi && cat a.txt && ls`, "echo"},
		// cd is now a safe builtin, so a cd-led read compound keys on its leading head,
		// not cd — and (see TestSafeBuiltinsDoNotEscalate) doesn't prompt at all.
		{`echo hi && cd apps/www && ls`, "echo"},
		// Single command → its own head.
		{`mycli status`, "mycli"},
		// Env-assignment prefix is stripped, same as CommandHead.
		{`FOO=bar mycli status`, "mycli"},
		// Path basename, not the full path.
		{`echo hi && /usr/local/bin/mycli status`, "mycli"},
		// Pipeline: the escalating stage wins over a safe head.
		{`cat log | weirdtool --parse`, "weirdtool"},
		// Known write head wins over safe heads.
		{`echo hi && npm install`, "npm"},
	}
	for _, c := range cases {
		if got := RiskHead(c.cmd); got != c.want {
			t.Errorf("RiskHead(%q) = %q, want %q", c.cmd, got, c.want)
		}
	}
}

// The actual papercut: a read-only compound whose only unrecognized token was the `cd`
// builtin must NOT prompt. supabase status is a known cloud read; echo/cat/ls/head are
// reads; cd is navigation. The whole thing is Safe now.
func TestSafeBuiltinsDoNotEscalate(t *testing.T) {
	cmd := `echo "=== supabase link status ===" && (cd apps/www 2>/dev/null; supabase status 2>&1 | head -20); echo "---"; cat supabase/.gitignore 2>/dev/null; ls -la supabase/`
	if r, _ := ClassifyBash(cmd); r != Safe {
		t.Fatalf("ClassifyBash(real compound) = %v, want Safe — cd should not escalate a read-only compound", r)
	}
	for _, b := range []string{"cd apps/www", "pushd /tmp", "popd", "true", "false", ":"} {
		if r, _ := ClassifyBash(b); r != Safe {
			t.Errorf("ClassifyBash(%q) = %v, want Safe", b, r)
		}
	}
}
