package membench

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/memcode-ai/memcode/internal/sessionlog"
)

func TestGenerateFactsCacheAndIngest(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // keep the facts cache off the real ~/.cache

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		reply := `[{"fact":"Angela adopted a retriever named Biscuit.","entities":["Angela","biscuit"],"source":"s1#0"},` +
			`{"fact":"The vet visit is on Friday.","entities":["vet"],"source":"bogus#9"}]`
		json.NewEncoder(w).Encode(map[string]any{
			"content": []map[string]string{{"type": "text", "text": reply}},
		})
	}))
	defer srv.Close()
	t.Setenv("MEMBENCH_QA_PROVIDER", "anthropic")
	t.Setenv("MEMBENCH_QA_BASE", srv.URL)
	t.Setenv("MEMBENCH_QA_KEY", "stub")
	t.Setenv("MEMBENCH_QA_MODEL", "stub-facts")

	session := SessionDoc{
		ID: "s1", TS: time.Date(2023, 5, 8, 0, 0, 0, 0, time.UTC),
		Turns: []Turn{
			{Role: "user", Text: "we adopted a golden retriever, her name is Biscuit", ID: "s1#0"},
			{Role: "assistant", Text: "congrats!", ID: "s1#1"},
		},
	}
	// The same session appears in two haystacks: one generation, one cache entry.
	ds := &Dataset{Name: "t", Questions: []Question{
		{ID: "q1", Haystack: []SessionDoc{session}},
		{ID: "q2", Haystack: []SessionDoc{session}},
	}}
	if err := GenerateFacts(ds, 0); err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("identical sessions must generate once, got %d calls", got)
	}
	for qi := range ds.Questions {
		facts := ds.Questions[qi].Haystack[0].Facts
		if len(facts) != 2 {
			t.Fatalf("q%d: want 2 facts, got %+v", qi, facts)
		}
		if facts[0].Source != "s1#0" || facts[0].Entities[0] != "angela" {
			t.Fatalf("valid source/entities mangled: %+v", facts[0])
		}
		if facts[1].Source != "" {
			t.Fatalf("hallucinated source must drop, got %q", facts[1].Source)
		}
	}

	// Rerun hits the disk cache: no new API calls even on a fresh Dataset.
	ds2 := &Dataset{Name: "t", Questions: []Question{{ID: "q1", Haystack: []SessionDoc{session}}}}
	if err := GenerateFacts(ds2, 0); err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("second run must be cache-only, got %d calls", got)
	}

	// Ingest lands them as KindFacts records with the source as Slug.
	root := t.TempDir()
	if err := Ingest(root, ds.Questions[0].Haystack); err != nil {
		t.Fatal(err)
	}
	recs, err := sessionlog.Recent(root, "s1", 0)
	if err != nil {
		t.Fatal(err)
	}
	var facts []sessionlog.Record
	for _, r := range recs {
		if r.Kind == sessionlog.KindFacts {
			facts = append(facts, r)
		}
	}
	if len(facts) != 2 || facts[0].Slug != "s1#0" || len(facts[0].Entities) != 2 {
		t.Fatalf("ingested facts records wrong: %+v", facts)
	}
}

func TestSplitQuestions(t *testing.T) {
	ds := &Dataset{Name: "d", Questions: []Question{{ID: "a"}, {ID: "b"}, {ID: "c"}}}
	even, err := SplitQuestions(ds, "even")
	if err != nil || len(even.Questions) != 2 || even.Questions[1].ID != "c" {
		t.Fatalf("even split wrong: %+v err=%v", even, err)
	}
	odd, _ := SplitQuestions(ds, "odd")
	if len(odd.Questions) != 1 || odd.Questions[0].ID != "b" {
		t.Fatalf("odd split wrong: %+v", odd)
	}
	if _, err := SplitQuestions(ds, "banana"); err == nil {
		t.Fatal("bad split name must error")
	}
}
