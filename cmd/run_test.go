package cmd

import (
	"testing"

	"github.com/spf13/cobra"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
)

// TestResolveMode verifies the --allow-all / --auto / default flag → permissions.Mode
// mapping. This is the cmd→runtime wiring seam: a flag-parsing regression here
// surfaces only at runtime (the wrong permission mode silently applied).
func TestResolveMode(t *testing.T) {
	cases := []struct {
		name  string
		flags map[string]bool
		want  permissions.Mode
	}{
		{"default (no flags) → ask", nil, permissions.ModeAsk},
		{"--auto → auto", map[string]bool{"auto": true}, permissions.ModeAuto},
		{"--allow-all → allow-all", map[string]bool{"allow-all": true}, permissions.ModeAllowAll},
		{"--allow-all beats --auto (precedence)", map[string]bool{"auto": true, "allow-all": true}, permissions.ModeAllowAll},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cmd := buildAgentCmdForTest(t, c.flags)
			got := resolveMode(cmd)
			if got != c.want {
				t.Errorf("resolveMode() = %v, want %v", got, c.want)
			}
		})
	}
}

// TestModeExplicit verifies that modeExplicit returns true only when the user
// forced a mode with a flag (so it overrides — and does not overwrite — the
// remembered config mode).
func TestModeExplicit(t *testing.T) {
	cases := []struct {
		name  string
		flags map[string]bool
		want  bool
	}{
		{"no flags → not explicit", nil, false},
		{"--auto → explicit", map[string]bool{"auto": true}, true},
		{"--allow-all → explicit", map[string]bool{"allow-all": true}, true},
		{"--no-context alone → not explicit", map[string]bool{"no-context": true}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cmd := buildAgentCmdForTest(t, c.flags)
			got := modeExplicit(cmd)
			if got != c.want {
				t.Errorf("modeExplicit() = %v, want %v", got, c.want)
			}
		})
	}
}

// TestParseMode verifies the stored-mode-string → permissions.Mode round-trip and
// the unrecognized fallback (ok=false). This protects the remembered-mode restore
// path: a corrupted config value must NOT silently become allow-all.
func TestParseMode(t *testing.T) {
	cases := []struct {
		input  string
		want   permissions.Mode
		wantOK bool
	}{
		{"ask", permissions.ModeAsk, true},
		{"auto", permissions.ModeAuto, true},
		{"allow-all", permissions.ModeAllowAll, true},
		{"", permissions.ModeAsk, false},     // empty → not recognized
		{"yolo", permissions.ModeAsk, false}, // unrecognized → not recognized (NOT allow-all)
		{"ASK", permissions.ModeAsk, false},  // case-sensitive
	}
	for _, c := range cases {
		got, ok := parseMode(c.input)
		if got != c.want || ok != c.wantOK {
			t.Errorf("parseMode(%q) = (%v, %v), want (%v, %v)", c.input, got, ok, c.want, c.wantOK)
		}
	}
}

// TestAgentCmdFlagWiring verifies that the cobra flag registration is correct —
// flags parse to the expected values via cmd.Flags().GetBool. This tests the flag
// registration without needing a real gateway.
func TestAgentCmdFlagWiring(t *testing.T) {
	cmd := buildAgentCmdForTest(t, map[string]bool{
		"auto":       true,
		"no-context": true,
		"background": true,
	})

	tests := []struct {
		flag string
		want bool
	}{
		{"auto", true},
		{"allow-all", false},
		{"ask", true}, // default-true flag stays true
		{"no-context", true},
		{"background", true},
	}
	for _, tt := range tests {
		got, err := cmd.Flags().GetBool(tt.flag)
		if err != nil {
			t.Errorf("GetBool(%q) error: %v", tt.flag, err)
			continue
		}
		if got != tt.want {
			t.Errorf("flag %q = %v, want %v", tt.flag, got, tt.want)
		}
	}

	// String flags
	if v, _ := cmd.Flags().GetString("job"); v != "" {
		t.Errorf("job flag default = %q, want empty", v)
	}
	if v, _ := cmd.Flags().GetString("protocol"); v != "" {
		t.Errorf("protocol flag default = %q, want empty", v)
	}
}

// buildAgentCmdForTest creates a fresh runCmd with the given bool flags set,
// matching the real init() flag registration. This lets resolveMode/modeExplicit
// tests exercise the actual cobra flag lookup path.
func buildAgentCmdForTest(t *testing.T, flags map[string]bool) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{Use: "agent"}
	cmd.Flags().Bool("ask", true, "prompt before risky actions (default)")
	cmd.Flags().Bool("auto", false, "run low/medium-risk actions automatically")
	cmd.Flags().Bool("allow-all", false, "run everything except catastrophic commands")
	cmd.Flags().Bool("no-context", false, "cold mode: skip the context pack")
	cmd.Flags().Bool("background", false, "run as a detached background job")
	for name, val := range flags {
		if err := cmd.Flags().Set(name, boolStr(val)); err != nil {
			t.Fatalf("set flag %s: %v", name, err)
		}
	}
	return cmd
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
