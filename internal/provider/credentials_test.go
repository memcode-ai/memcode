package provider

import (
	"reflect"
	"testing"
)

// C1: MEMCODE_CREDENTIALS is an ordered consent list; the old single-value
// MEMCODE_CREDENTIAL_SOURCE reads as a one-item list until migrated. These
// tests are the contract — the auth CLI and lane construction both build on
// AttachedSources.
func TestAttachedSourcesGrammar(t *testing.T) {
	cases := []struct {
		name   string
		creds  string // MEMCODE_CREDENTIALS
		legacy string // MEMCODE_CREDENTIAL_SOURCE
		want   []string
	}{
		{"empty", "", "", nil},
		{"single", "claude", "", []string{"claude"}},
		{"list", "claude,codex", "", []string{"claude", "codex"}},
		{"spaces and case", " Claude , CODEX ", "", []string{"claude", "codex"}},
		{"aliases canonicalize", "claude-sub,anthropic-sub,grok-sub,xai-sub", "", []string{"claude", "grok"}},
		{"dedupe first wins", "codex,claude,codex", "", []string{"codex", "claude"}},
		{"unknown tokens dropped", "claude,frobnitz,codex", "", []string{"claude", "codex"}},
		{"legacy only reads as one-item list", "", "claude", []string{"claude"}},
		{"legacy alias canonicalizes", "", "anthropic-sub", []string{"claude"}},
		{"new var wins over legacy", "codex", "claude", []string{"codex"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvCredentials, tc.creds)
			t.Setenv(EnvCredentialSource, tc.legacy)
			got := AttachedSources()
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("AttachedSources() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSourceVendor(t *testing.T) {
	cases := map[string]string{"claude": "anthropic", "codex": "openai", "copilot": "openai", "grok": "grok"}
	for src, want := range cases {
		if got := SourceVendor(src); got != want {
			t.Errorf("SourceVendor(%q) = %q, want %q", src, got, want)
		}
	}
	if SourceVendor("frobnitz") != "" {
		t.Error("unknown source must have no vendor")
	}
}

func TestSelectedSourcesUnresolvedWhenSignedOut(t *testing.T) {
	// No live logins exist in the test env, so every attached source is
	// unresolved by construction — the boot warning must name each one.
	t.Setenv(EnvCredentials, "claude,codex")
	t.Setenv(EnvCredentialSource, "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	got := SelectedSourcesUnresolved()
	if !reflect.DeepEqual(got, []string{"claude", "codex"}) {
		t.Fatalf("SelectedSourcesUnresolved() = %v, want [claude codex]", got)
	}
	t.Setenv(EnvCredentials, "")
	if got := SelectedSourcesUnresolved(); got != nil {
		t.Fatalf("no sources attached but unresolved = %v", got)
	}
}
