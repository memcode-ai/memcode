package input

import "testing"

// TestRouteClassification locks the regex fixes: "next…"/"then…" markers need a word
// boundary (so nextjs/thenable aren't queued), and "no,"/"nope" interrupt.
func TestRouteClassification(t *testing.T) {
	cwd := t.TempDir()
	cases := []struct {
		line string
		want Route
	}{
		{"nextjs bug is failing", Steer},    // was wrongly Queue (next[,:]?)
		{"nextauth route is broken", Steer}, // was wrongly Queue
		{"next, fix the build", Queue},      // real queue marker
		{"then run the tests", Queue},       // real queue marker
		{"thenable promise issue", Steer},   // not a queue marker
		{"no, that is wrong", Interrupt},    // was missed by nope?
		{"nope", Interrupt},                 //
		{"now do the thing", Steer},         // must NOT interrupt (no(?:pe)? \b)
		{"nonsense output", Steer},          // must NOT interrupt
		{"stop that", Interrupt},            //
		{"$ /tmp/foo.png", Shell},           // shell lane: never image-extract
	}
	for _, c := range cases {
		if got := Parse(c.line, cwd).Route; got != c.want {
			t.Errorf("Parse(%q) route = %q, want %q", c.line, got, c.want)
		}
	}
}
