package membench

import (
	"os"
	"testing"
	"time"
)

func fixtureDataset() *Dataset {
	mk := func(id string, ts time.Time, texts ...string) SessionDoc {
		doc := SessionDoc{ID: id, TS: ts}
		for i, t := range texts {
			role := "user"
			if i%2 == 1 {
				role = "assistant"
			}
			doc.Turns = append(doc.Turns, Turn{Role: role, Text: t, ID: turnID(id, i)})
		}
		return doc
	}
	base := time.Date(2023, 5, 1, 12, 0, 0, 0, time.UTC)
	haystack := []SessionDoc{
		mk("sessA", base,
			"I adopted a golden retriever puppy named Biscuit last weekend.",
			"Congratulations, Biscuit sounds adorable."),
		mk("sessB", base.AddDate(0, 1, 0),
			"My favorite programming language is Python these days.",
			"Python is a great choice for most work."),
		mk("sessC", base.AddDate(0, 2, 0),
			"Actually I have switched my favorite language to Go now.",
			"Go it is, noted."),
	}
	return &Dataset{
		Name: "fixture",
		Gran: BySession,
		Questions: []Question{
			{
				ID:       "q-extract",
				Type:     "single-session-user",
				Text:     "What is the name of the puppy the user adopted?",
				Gold:     map[string]bool{"sessA": true},
				Haystack: haystack,
			},
			{
				ID:       "q-update",
				Type:     "knowledge-update",
				Text:     "What is the user's favorite programming language?",
				Gold:     map[string]bool{"sessC": true},
				Haystack: haystack,
			},
			{
				ID:       "q-abs",
				Type:     "abstention",
				Text:     "What car does the user drive?",
				Gold:     map[string]bool{},
				Haystack: haystack,
			},
		},
	}
}

func turnID(sess string, i int) string {
	return sess + "#" + string(rune('0'+i))
}

func TestIngestAndBM25Retrieval(t *testing.T) {
	ds := fixtureDataset()
	work := t.TempDir()

	res, err := Run(ds, BM25Adapter{}, work, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Skipped != 1 {
		t.Fatalf("abstention question should be skipped, got %d", res.Skipped)
	}
	if res.Questions != 2 {
		t.Fatalf("want 2 scored questions, got %d", res.Questions)
	}
	// The puppy question must retrieve sessA at rank 1: it is the only
	// session sharing the terms.
	for _, qr := range res.PerQ {
		if qr.QuestionID == "q-extract" {
			if len(qr.Ranked) == 0 || qr.Ranked[0] != "sessA" {
				t.Fatalf("q-extract ranked = %v, want sessA first", qr.Ranked)
			}
			if qr.RecallAtK[1] != 1.0 {
				t.Fatalf("q-extract recall@1 = %v, want 1.0", qr.RecallAtK[1])
			}
		}
	}
}

func TestTimeAwareKnowledgeUpdate(t *testing.T) {
	ds := fixtureDataset()
	work := t.TempDir()

	res, err := Run(ds, BM25Adapter{TimeAware: true}, work, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, qr := range res.PerQ {
		if qr.QuestionID == "q-update" {
			if len(qr.Ranked) == 0 || qr.Ranked[0] != "sessC" {
				t.Fatalf("knowledge-update ranked = %v, want the LATEST fact (sessC) first", qr.Ranked)
			}
		}
	}
}

func TestProductAdapterRankedSearch(t *testing.T) {
	ds := fixtureDataset()
	work := t.TempDir()

	res, err := Run(ds, ProductAdapter{}, work, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Questions != 2 {
		t.Fatalf("want 2 scored questions, got %d", res.Questions)
	}
	// Post-promotion the product path is ranked: the paraphrased puppy
	// question must now retrieve sessA even though no substring matches.
	for _, qr := range res.PerQ {
		if qr.QuestionID == "q-extract" {
			if len(qr.Ranked) == 0 || qr.Ranked[0] != "sessA" {
				t.Fatalf("product ranked = %v, want sessA first", qr.Ranked)
			}
		}
	}
}

func TestExtractWindow(t *testing.T) {
	qd := time.Date(2023, 8, 1, 0, 0, 0, 0, time.UTC)
	w := extractWindow("What did I say in May 2023 about the trip?", qd)
	if w == nil || w.from.Month() != time.May || w.from.Year() != 2023 {
		t.Fatalf("month+year window wrong: %+v", w)
	}
	w = extractWindow("What happened in 2022?", qd)
	if w == nil || w.from.Year() != 2022 {
		t.Fatalf("year window wrong: %+v", w)
	}
	// Bare month later than the question month resolves to last year.
	w = extractWindow("the December party", qd)
	if w == nil || w.from.Year() != 2022 || w.from.Month() != time.December {
		t.Fatalf("bare-month window wrong: %+v", w)
	}
	if extractWindow("no dates here", qd) != nil {
		t.Fatal("window from dateless question should be nil")
	}
}

func TestParseWhenLayouts(t *testing.T) {
	cases := map[string]bool{
		"2023/05/20 (Sat) 02:21": true,
		"1:56 pm on 8 May, 2023": true,
		"totally not a date":     false,
	}
	for s, ok := range cases {
		got := parseWhen(s)
		if ok && got.IsZero() {
			t.Errorf("parseWhen(%q) failed, want success", s)
		}
		if !ok && !got.IsZero() {
			t.Errorf("parseWhen(%q) = %v, want zero", s, got)
		}
	}
}

func TestMain(m *testing.M) { os.Exit(m.Run()) }
