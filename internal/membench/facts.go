package membench

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Facts generation: the bench-side mirror of the product's post-session facts
// extraction (gateway "facts" mode). Real sessions get facts from the
// cognition loop; benchmark haystacks get them here, through the MEMBENCH_QA
// chat config, so the ingested logs carry the same KindFacts records the
// product path produces. One extension over the gateway contract: transcript
// lines are prefixed with their benchmark turn ids and each fact cites its
// source line, so a fact hit maps back to real evidence at turn granularity.

// Fact is one extracted atomic fact attached to a SessionDoc.
type Fact struct {
	Fact     string   `json:"fact"`
	Entities []string `json:"entities"`
	Source   string   `json:"source,omitempty"` // benchmark turn id the fact is drawn from
}

const factsSystem = `You extract atomic facts from one chat-session transcript, for later retrieval. Every transcript line starts with its turn id in [brackets].

Output ONLY a JSON array. Each element: {"fact","entities":["..."],"source":"<turn id>"}.
- fact = ONE self-contained third-person sentence stating something durable that
  happened or was established. Include concrete names, places, numbers, and dates
  VERBATIM — a fact must be findable later by the words in it.
- entities = 1-5 lowercase noun keys naming the things the fact is about
  (people, pets, places, activities, objects). Reuse identical keys for the same thing.
- source = the turn id (without brackets) of the line the fact is chiefly drawn from.
- 5 to 20 facts. Skip pleasantries and dead ends.
- No speculation: only what the transcript actually supports.`

const maxFactsTranscript = 24_000

// GenerateFacts extracts facts for every session in the first `limit`
// questions' haystacks (0 = all) and attaches them to the SessionDocs in
// place. Identical sessions across overlapping haystacks generate once;
// results persist in a content-addressed cache so reruns are free.
func GenerateFacts(ds *Dataset, limit int) error {
	cfg, err := chatConfigFromEnv("MEMBENCH_QA")
	if err != nil {
		return err
	}
	cfg.maxTok = 1500

	qs := ds.Questions
	if limit > 0 && limit < len(qs) {
		qs = qs[:limit]
	}
	type group struct {
		doc  *SessionDoc // representative (identical content across the group)
		docs []*SessionDoc
	}
	groups := map[string]*group{}
	var order []string
	for qi := range qs {
		for si := range qs[qi].Haystack {
			doc := &qs[qi].Haystack[si]
			if len(doc.Turns) == 0 {
				continue
			}
			key := factsKey(cfg.model, doc)
			g := groups[key]
			if g == nil {
				g = &group{doc: doc}
				groups[key] = g
				order = append(order, key)
			}
			g.docs = append(g.docs, doc)
		}
	}

	workers := 6
	if v := os.Getenv("MEMBENCH_FACTS_WORKERS"); v != "" {
		fmt.Sscanf(v, "%d", &workers)
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, max(1, workers))
	errs := make([]error, len(order))
	for i, key := range order {
		wg.Add(1)
		go func(i int, key string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			g := groups[key]
			facts, err := sessionFacts(cfg, key, g.doc)
			if err != nil {
				errs[i] = fmt.Errorf("facts %s: %w", g.doc.ID, err)
				return
			}
			for _, d := range g.docs {
				d.Facts = facts
			}
		}(i, key)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// factsKey content-addresses a session for the cache: model + every turn's id
// and text, so a model change or edited dataset can never serve stale facts.
func factsKey(model string, doc *SessionDoc) string {
	h := sha256.New()
	h.Write([]byte(model))
	for _, t := range doc.Turns {
		h.Write([]byte{0})
		h.Write([]byte(t.ID + "\x00" + t.Role + "\x00" + t.Text))
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

func sessionFacts(cfg chatConfig, key string, doc *SessionDoc) ([]Fact, error) {
	path := filepath.Join(CacheDir(), "facts", key+".json.gz")
	if facts, err := readFactsCache(path); err == nil {
		return facts, nil
	}

	var b strings.Builder
	for _, t := range doc.Turns {
		line := "[" + t.ID + "] " + strings.ToUpper(t.Role) + ": " + t.Text + "\n"
		if b.Len()+len(line) > maxFactsTranscript {
			break
		}
		b.WriteString(line)
	}
	transcript := strings.TrimSpace(b.String())
	if len(transcript) < 40 { // nothing to consolidate; also avoids empty-content 400s
		facts := []Fact{}
		_ = writeFactsCache(path, facts)
		return facts, nil
	}
	reply, err := cfg.complete(factsSystem, transcript)
	if err != nil {
		return nil, err
	}
	facts := parseFacts(reply)
	if len(facts) == 0 {
		// One retry for a malformed reply; a session that still yields nothing
		// extractable simply carries no facts — that's data, not failure.
		if reply, err = cfg.complete(factsSystem, transcript); err != nil {
			return nil, err
		}
		facts = parseFacts(reply)
	}
	if facts == nil {
		facts = []Fact{}
	}
	valid := map[string]bool{}
	for _, t := range doc.Turns {
		valid[t.ID] = true
	}
	for i := range facts {
		if !valid[facts[i].Source] {
			facts[i].Source = "" // hallucinated citation: keep the fact, drop the claim
		}
	}
	_ = writeFactsCache(path, facts)
	return facts, nil
}

// parseFacts parses the strict-JSON contract, tolerating code fences,
// mirroring the product parser in agent/runtime.
func parseFacts(text string) []Fact {
	text = strings.TrimSpace(text)
	if i := strings.Index(text, "["); i >= 0 {
		if j := strings.LastIndex(text, "]"); j > i {
			text = text[i : j+1]
		}
	}
	var facts []Fact
	if json.Unmarshal([]byte(text), &facts) != nil {
		return nil
	}
	out := facts[:0]
	for _, f := range facts {
		f.Fact = strings.TrimSpace(f.Fact)
		if f.Fact == "" {
			continue
		}
		for i, e := range f.Entities {
			f.Entities[i] = strings.ToLower(strings.TrimSpace(e))
		}
		out = append(out, f)
	}
	return out
}

func readFactsCache(path string) ([]Fact, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	var facts []Fact
	if err := json.NewDecoder(gz).Decode(&facts); err != nil {
		return nil, err
	}
	return facts, nil
}

func writeFactsCache(path string, facts []Fact) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	gz := gzip.NewWriter(f)
	if err := json.NewEncoder(gz).Encode(facts); err != nil {
		f.Close()
		return err
	}
	if err := gz.Close(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
