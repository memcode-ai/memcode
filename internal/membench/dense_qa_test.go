package membench

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// stubEmbedServer embeds into a tiny concept space: synonym groups share a
// dimension, so cosine ranking behaves like a real embedder (paraphrases
// meet) for fixture-scale tests. Token match, not substring.
func stubEmbedServer(t *testing.T) *httptest.Server {
	concepts := [][]string{
		{"puppy", "dog", "pet", "adopted", "adopt", "biscuit", "retriever"},
		{"python", "go", "language", "programming", "favorite"},
	}
	embed := func(text string) []float32 {
		v := make([]float32, len(concepts)+1)
		for _, tok := range strings.Fields(strings.ToLower(strings.NewReplacer("?", "", ".", "", ",", "").Replace(text))) {
			for ci, group := range concepts {
				for _, w := range group {
					if tok == w {
						v[ci]++
					}
				}
			}
		}
		v[len(concepts)] = 0.01 // never a zero vector
		return v
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("stub embed decode: %v", err)
		}
		type item struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		}
		out := struct {
			Data []item `json:"data"`
		}{}
		for i, s := range req.Input {
			out.Data = append(out.Data, item{i, embed(s)})
		}
		json.NewEncoder(w).Encode(out)
	}))
}

func TestHybridAdapterWithStubEmbedder(t *testing.T) {
	srv := stubEmbedServer(t)
	defer srv.Close()
	t.Setenv("MEMBENCH_EMBED_BASE", srv.URL)
	t.Setenv("MEMBENCH_EMBED_KEY", "stub")
	t.Setenv("MEMBENCH_EMBED_MODEL", "stub-model-"+t.Name())

	h, err := NewHybridAdapter()
	if err != nil {
		t.Fatal(err)
	}
	ds := fixtureDataset()
	// A paraphrase question with zero lexical overlap on the key noun:
	// "dog"/"pet" only meet "puppy" through the embedding space.
	ds.Questions = append(ds.Questions, Question{
		ID:       "q-paraphrase",
		Type:     "single-session-user",
		Text:     "What pet dog did they get?",
		Gold:     map[string]bool{"sessA": true},
		Haystack: ds.Questions[0].Haystack,
	})

	res, err := Run(ds, h, t.TempDir(), 0, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, qr := range res.PerQ {
		if qr.QuestionID == "q-paraphrase" {
			if len(qr.Ranked) == 0 || qr.Ranked[0] != "sessA" {
				t.Fatalf("hybrid should rank sessA first on paraphrase, got %v", qr.Ranked)
			}
		}
	}
}

// stubChatServer answers deterministically and judges by containment.
func stubChatServer(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("stub chat decode: %v", err)
		}
		user := req.Messages[len(req.Messages)-1].Content
		system := req.Messages[0].Content
		reply := "Biscuit"
		if strings.Contains(system, "grading") {
			// Judge: correct iff the proposed answer appears in the reference.
			reply = "no"
			var ref, prop string
			for _, line := range strings.Split(user, "\n") {
				if strings.HasPrefix(line, "Reference answer: ") {
					ref = strings.TrimPrefix(line, "Reference answer: ")
				}
				if strings.HasPrefix(line, "Proposed answer: ") {
					prop = strings.TrimPrefix(line, "Proposed answer: ")
				}
			}
			if prop != "" && strings.Contains(strings.ToLower(ref), strings.ToLower(prop)) {
				reply = "yes"
			}
		}
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": reply}}},
		})
	}))
}

func TestRunQAWithStubModel(t *testing.T) {
	srv := stubChatServer(t)
	defer srv.Close()
	t.Setenv("MEMBENCH_QA_PROVIDER", "openai")
	t.Setenv("MEMBENCH_QA_BASE", srv.URL)
	t.Setenv("MEMBENCH_QA_KEY", "stub")
	t.Setenv("MEMBENCH_QA_MODEL", "stub-chat")

	ds := fixtureDataset()
	ds.Questions[0].Answer = "The puppy is named Biscuit."
	ds.Questions[1].Answer = "Go"
	ds.Questions = ds.Questions[:2] // drop the abstention fixture (no answer set)

	res, err := RunQA(ds, BM25Adapter{V2: true}, t.TempDir(), 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 2 {
		t.Fatalf("want 2 judged questions, got %d", res.Total)
	}
	// The stub always answers "Biscuit": question 1 judged correct
	// (contained in the reference), question 2 judged incorrect.
	if res.Correct != 1 {
		t.Fatalf("want exactly 1 correct under the stub, got %d (%+v)", res.Correct, res.ByType)
	}
}
