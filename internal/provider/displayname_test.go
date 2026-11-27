package provider

import "testing"

// DisplayName shows the model's real name from the catalog, by id or label,
// and falls back to the short id for models the catalog doesn't name.
func TestDisplayName(t *testing.T) {
	for in, want := range map[string]string{
		"gemini-4-carbon":                   "Gemini 4 Carbon",
		"gemini-pro":                        "Gemini 4 Carbon",
		"claude-opus-5-5":                   "Opus 5.5",
		"grok-4.7":                          "Grok 4.7",
		"accounts/fireworks/models/glm-5p3": "GLM-5.3",
		"org/some-unknown-model":            "some-unknown-model",
	} {
		if got := DisplayName(in); got != want {
			t.Errorf("DisplayName(%q) = %q, want %q", in, got, want)
		}
	}
}
