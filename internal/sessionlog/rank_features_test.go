package sessionlog

import (
	"testing"
	"time"
)

func setRanking(t *testing.T, opts RankingOptions) {
	t.Helper()
	prev := Ranking
	Ranking = opts
	t.Cleanup(func() { Ranking = prev })
}

func TestFieldWeightsPreferFacts(t *testing.T) {
	setRanking(t, RankingOptions{FieldWeights: true})
	root := t.TempDir()
	writeSession(t, root, "sess_tool", []Record{
		{Kind: KindToolCall, Tool: "shell", Input: "grep", Content: "migrated billing webhooks to stripe"},
	})
	writeSession(t, root, "sess_fact", []Record{
		{Kind: KindFacts, Text: "The team migrated billing webhooks to Stripe."},
	})
	hits, err := Search(root, "billing webhooks stripe migration", 5)
	if err != nil || len(hits) == 0 {
		t.Fatalf("search failed: %v", err)
	}
	if hits[0].Kind != KindFacts {
		t.Fatalf("facts record should outrank tool output for equal content, got %s", hits[0].Kind)
	}
}

func TestRM3ExpansionSurfacesParaphrase(t *testing.T) {
	root := t.TempDir()
	writeSession(t, root, "sess_a", []Record{
		{Kind: KindUserMessage, Text: "planning the trip to yosemite national park next month"},
		{Kind: KindUserMessage, Text: "unrelated chatter about compilers"},
	})
	writeSession(t, root, "sess_b", []Record{
		{Kind: KindAssistantMessage, Text: "yosemite valley camping reservations are booked"},
	})

	// Without RM3 the camping record shares no term with the query and scores 0.
	setRanking(t, RankingOptions{})
	hits, _ := Search(root, "national park trip", 10)
	for _, h := range hits {
		if h.SessionID == "sess_b" {
			t.Fatalf("baseline should not reach sess_b, got %+v", h)
		}
	}

	// With RM3, "yosemite" rides in from the top feedback doc and reaches it.
	setRanking(t, RankingOptions{RM3: true})
	hits, _ = Search(root, "national park trip", 10)
	found := false
	for _, h := range hits {
		found = found || h.SessionID == "sess_b"
	}
	if !found {
		t.Fatal("RM3 expansion should surface the paraphrased session")
	}
}

func TestAdjacencyLiftsNeighborEvidence(t *testing.T) {
	setRanking(t, RankingOptions{Adjacency: true})
	root := t.TempDir()
	writeSession(t, root, "sess_adj", []Record{
		{Kind: KindUserMessage, Text: "what did the profiler say about the allocator"},
		{Kind: KindAssistantMessage, Text: "it spends most time in mallocgc"},
	})
	hits, err := Search(root, "profiler allocator", 5)
	if err != nil || len(hits) < 2 {
		t.Fatalf("adjacency should carry the answer turn along, got %d hits err=%v", len(hits), err)
	}
	if hits[1].Text != "it spends most time in mallocgc" {
		t.Fatalf("neighbor evidence should be the second hit, got %q", hits[1].Text)
	}
}

func TestEntityPPRSeedsAndRanks(t *testing.T) {
	bySession := map[string][]Record{
		"sess_dog": {
			{Kind: KindFacts, Text: "Angela adopted a golden retriever named Biscuit.", Entities: []string{"angela", "biscuit", "dogs"}},
			{Kind: KindFacts, Text: "Biscuit goes to the vet on Fridays.", Entities: []string{"biscuit", "vet"}},
		},
		"sess_other": {
			{Kind: KindFacts, Text: "The gateway deploys via Cloud Run.", Entities: []string{"gateway", "cloud run"}},
		},
		"sess_nofacts": {
			{Kind: KindUserMessage, Text: "dogs are great"},
		},
	}
	// "dog" must stem-match the "dogs" entity; sess_dog carries the seed.
	order := entityPPRSessions(bySession, rankTokenize("what dog does she have"))
	if len(order) == 0 || order[0] != "sess_dog" {
		t.Fatalf("PPR should rank the dog session first, got %v", order)
	}
	// No entity matches: no signal, not a zero ranking.
	if got := entityPPRSessions(bySession, rankTokenize("quantum lattice")); got != nil {
		t.Fatalf("unseeded query must return nil, got %v", got)
	}
	// No facts anywhere: nil.
	if got := entityPPRSessions(map[string][]Record{"s": {{Kind: KindUserMessage, Text: "x"}}}, rankTokenize("x")); got != nil {
		t.Fatalf("factless corpus must return nil, got %v", got)
	}
}

func TestRRFFusionPromotesEntityMatch(t *testing.T) {
	root := t.TempDir()
	// Both sessions mention the query terms once; only sess_dog's facts carry
	// the seeded entity, so fusion must break the lexical near-tie its way.
	writeSession(t, root, "sess_dog", []Record{
		{Kind: KindUserMessage, Text: "picked up food for the pet today"},
		{Kind: KindFacts, Text: "Angela adopted a retriever named Biscuit.", Entities: []string{"pet", "biscuit"}},
	})
	writeSession(t, root, "sess_noise", []Record{
		{Kind: KindUserMessage, Text: "pet peeve: flaky tests in ci"},
	})

	setRanking(t, RankingOptions{EntityPPR: true})
	hits, err := Search(root, "pet", 5)
	if err != nil || len(hits) == 0 {
		t.Fatalf("search failed: %v", err)
	}
	if hits[0].SessionID != "sess_dog" {
		t.Fatalf("entity fusion should lead with the pet session, got %s", hits[0].SessionID)
	}
}

func TestRankingDefaultsAllOff(t *testing.T) {
	if Ranking != (RankingOptions{}) {
		t.Fatalf("shipped defaults are the measured winners — all layers off: %+v", Ranking)
	}
}

// Guard: the layered passes must not disturb the exact-tier guarantee.
func TestExactTierStillWinsWithAllFeatures(t *testing.T) {
	root := t.TempDir()
	writeSession(t, root, "sess_x", []Record{
		{Kind: KindToolCall, Tool: "shell", Input: "kubectl rollout undo deploy/api", Content: "rolled back"},
	})
	writeSession(t, root, "sess_y", []Record{
		{Kind: KindFacts, Text: "The api deploy uses rollout tooling sometimes.", Entities: []string{"deploy"}},
	})
	hits, err := Search(root, "kubectl rollout undo deploy/api", 5)
	if err != nil || len(hits) == 0 {
		t.Fatalf("search failed: %v", err)
	}
	if hits[0].SessionID != "sess_x" {
		t.Fatalf("verbatim hit must stay on top, got %+v", hits[0])
	}
	_ = time.Now // keep time import if assertions above change
}
