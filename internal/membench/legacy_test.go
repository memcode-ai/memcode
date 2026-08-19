//go:build membench

package membench

import "testing"

// TestLegacyAdapterStaysBroken locks the documented "before" picture: the old
// substring scan cannot match paraphrased questions. Runs only with
// `-tags membench` — the real LegacyAdapter is gated there.
func TestLegacyAdapterStaysBroken(t *testing.T) {
	ds := fixtureDataset()
	work := t.TempDir()

	res, err := Run(ds, LegacyAdapter{}, work, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	// The old substring scan cannot match paraphrased questions; that is the
	// documented "before" picture and must stay reproducible.
	for _, qr := range res.PerQ {
		if qr.RecallAtK[10] != 0 {
			t.Fatalf("legacy adapter unexpectedly retrieves: %+v", qr)
		}
	}
}
