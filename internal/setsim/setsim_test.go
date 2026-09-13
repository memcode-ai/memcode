package setsim

import "testing"

func set(keys ...string) map[string]bool {
	m := map[string]bool{}
	for _, k := range keys {
		m[k] = true
	}
	return m
}

func structSet(keys ...string) map[string]struct{} {
	m := map[string]struct{}{}
	for _, k := range keys {
		m[k] = struct{}{}
	}
	return m
}

func TestJaccard(t *testing.T) {
	for _, tc := range []struct {
		name string
		a, b map[string]bool
		want float64
	}{
		{"identical", set("a", "b"), set("a", "b"), 1},
		{"disjoint", set("a"), set("b"), 0},
		{"half overlap", set("a", "b"), set("b", "c"), 1.0 / 3.0},
		{"subset", set("a"), set("a", "b"), 0.5},
		{"both empty", set(), set(), 0},
		{"one empty", set("a"), set(), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Jaccard(tc.a, tc.b); got != tc.want {
				t.Errorf("Jaccard = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestJaccardIsSymmetric: a similarity metric that depended on argument order
// would make clustering results depend on iteration order.
func TestJaccardIsSymmetric(t *testing.T) {
	a, b := set("x", "y", "z"), set("y", "q")
	if Jaccard(a, b) != Jaccard(b, a) {
		t.Error("Jaccard must be symmetric")
	}
}

// TestJaccardAcceptsEitherSetSpelling is the point of the generic: the three
// call sites this replaced used two different map shapes.
func TestJaccardAcceptsEitherSetSpelling(t *testing.T) {
	if got := Jaccard(structSet("a", "b"), structSet("b", "c")); got != 1.0/3.0 {
		t.Errorf("map[string]struct{} = %v, want 1/3", got)
	}
}
