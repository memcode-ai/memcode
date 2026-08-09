package runtime

import (
	"reflect"
	"testing"
)

func opts(labels ...string) []AskOption {
	out := make([]AskOption, len(labels))
	for i, l := range labels {
		out[i] = AskOption{Label: l}
	}
	return out
}

func TestPruneEscapeOptions(t *testing.T) {
	in := opts(
		"Fix the broken datasets",
		"Unify the two loading paths",
		"Something else (describe)",
		"Other",
		"None of the above",
		"Let me type my own",
	)
	got := pruneEscapeOptions(in)
	want := opts("Fix the broken datasets", "Unify the two loading paths")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("pruneEscapeOptions = %q, want %q", got, want)
	}
	// Pruning matches on the Label only; an option's description never triggers a prune.
	if g := pruneEscapeOptions([]AskOption{{Label: "Keep auth", Description: "something else entirely"}}); len(g) != 1 {
		t.Fatalf("must prune on Label only, not Description: %+v", g)
	}
	// Legitimate options with these words elsewhere are kept.
	keep := opts("Other integrations first", "Describe the schema in the runbook")
	if g := pruneEscapeOptions(keep); !reflect.DeepEqual(g, keep) {
		t.Fatalf("over-pruned legit options: %q", g)
	}
}
