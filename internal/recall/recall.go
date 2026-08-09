// Package recall answers natural-language questions about a repository's prose
// memory — "where did we decide X?", "what's our policy on Y?" — by ranking the
// corpus of source docs, adjudicated claims, and human decisions against the
// question with BM25 plus memcode-aware boosts (claim status, recency, scope,
// source kind, exact phrase). It is deliberately NOT embeddings: lexical search
// over prose is cheap, offline, deterministic, and "good enough" until a
// measured recall eval proves otherwise. Code questions ("where is this
// handler/symbol/test?") are answered by exact tools instead — `memcode explore`
// and the agent's rg/read/git tools — not here.
//
// The ranking respects memcode doctrine: current beats stale, scope matters,
// recent reality matters, evidence is not truth — but a query that explicitly
// asks about history/conflicts surfaces stale and conflicted material instead of
// burying it.
package recall

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/memcode-ai/memcode/internal/sources"
	"github.com/memcode-ai/memcode/internal/store"
)

const (
	maxChunkChars = 900       // target chunk size for source docs
	maxFileBytes  = 64 * 1024 // cap how much of any one doc we ingest

	bm25K1 = 1.5  // BM25 term-frequency saturation
	bm25B  = 0.75 // BM25 length normalization

	phraseBonus = 2.5 // additive bump when the whole query appears verbatim
)

// Category groups a chunk by its source kind, which drives weighting.
type Category string

const (
	CatDoc   Category = "doc"   // a source document (CLAUDE.md, README, AGENTS.md, docs)
	CatClaim Category = "claim" // an adjudicated claim
	CatEvent Category = "event" // a human decision/assertion/note
	CatRule  Category = "rule"  // a promoted standing rule (confirmed preference / distilled lesson)
)

// Chunk is one unit of prose with its provenance and ranking signals.
type Chunk struct {
	ID     string
	Text   string
	Source string // human-readable provenance

	Cat    Category
	Kind   string    // doc: claude/readme/docs/…; claim: claim type; event: decision/…
	Status string    // claim status (current|candidate|stale|conflicted)
	Stale  bool      // source doc is stale relative to the code it governs
	Scope  string    // dir it governs ("." = repo, "" = none)
	When   time.Time // recency signal (doc git date / event timestamp)
}

// Hit is a ranked chunk.
type Hit struct {
	Chunk Chunk
	Score float64
}

// Recall ranks the prose corpus against query and returns the top k hits with a
// positive score. scopePath (may be "") boosts chunks governing that path.
func Recall(ctx context.Context, st store.Store, root, query, scopePath string, k int) ([]Hit, error) {
	chunks, err := Corpus(ctx, st, root)
	if err != nil {
		return nil, err
	}
	if len(chunks) == 0 {
		return nil, nil
	}
	return rankBM25(chunks, query, scopePath, k), nil
}

// Corpus assembles the prose corpus from source docs, adjudicated claims, and
// decision-bearing events, each tagged with its ranking signals.
func Corpus(ctx context.Context, st store.Store, root string) ([]Chunk, error) {
	var chunks []Chunk

	// 1. Source documents (CLAUDE.md, READMEs, AGENTS.md, docs, ADRs …), chunked.
	srcs, err := sources.Discover(ctx, root)
	if err != nil {
		return nil, err
	}
	for _, s := range srcs {
		data, err := os.ReadFile(filepath.Join(root, s.Path))
		if err != nil {
			continue
		}
		if len(data) > maxFileBytes {
			data = data[:maxFileBytes]
		}
		for i, part := range chunkProse(string(data)) {
			chunks = append(chunks, Chunk{
				ID: fmt.Sprintf("%s#%d", s.Path, i), Text: part, Source: s.Path,
				Cat: CatDoc, Kind: s.Kind, Stale: s.Stale, Scope: s.Scope, When: parseDay(s.GitDate),
			})
		}
	}

	// 2. Adjudicated claims (rejected dropped; stale/conflicted kept, down-weighted).
	claims, err := st.ListClaims(ctx)
	if err != nil {
		return nil, err
	}
	for _, c := range claims {
		if c.Status == "rejected" {
			continue
		}
		text := c.Text
		if c.Evidence != "" {
			text += " — " + c.Evidence
		}
		label := fmt.Sprintf("claim/%s [%s]", c.Type, c.Status)
		if c.Scope != "" && c.Scope != "." {
			label += " @ " + c.Scope
		}
		chunks = append(chunks, Chunk{
			ID: c.ID, Text: text, Source: label,
			Cat: CatClaim, Kind: c.Type, Status: c.Status, Scope: c.Scope,
		})
	}

	// 3. Human decisions / assertions / notes / frustrations — the project's
	// voice — plus session outcomes (git's verdict on the agent's work), so
	// "what happened to my last change" recalls the acceptance trail.
	evs, err := st.ListEvents(ctx, store.EventFilter{
		Kinds: []string{"decision", "assertion", "user_note", "frustration", "session_outcome"},
	})
	if err != nil {
		return nil, err
	}
	for _, e := range evs {
		text := eventText(e.Payload)
		if e.Kind == "session_outcome" {
			text = outcomeText(e.Payload)
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		label := e.Kind
		if !e.TS.IsZero() {
			label += " @ " + e.TS.Local().Format("2006-01-02")
		}
		chunks = append(chunks, Chunk{
			ID: fmt.Sprintf("evt-%d", e.ID), Text: text, Source: label,
			Cat: CatEvent, Kind: e.Kind, When: e.TS,
		})
	}

	// 4. Promoted standing rules — confirmed preferences and distilled lessons.
	// The long-term memory the user actually taught the agent must be
	// recallable ("what are my preferences about X?"), not only inlined.
	for _, sub := range []string{"prefs", "lessons"} {
		dir := filepath.Join(root, ".memcode", sub)
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue // dir absent — normal
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			data, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				continue
			}
			var when time.Time
			if info, err := e.Info(); err == nil {
				when = info.ModTime()
			}
			kind := strings.TrimSuffix(sub, "s") // pref | lesson
			chunks = append(chunks, Chunk{
				ID: sub + "/" + e.Name(), Text: string(data), Source: kind + "/" + e.Name(),
				Cat: CatRule, Kind: kind, When: when,
			})
		}
	}

	return chunks, nil
}

// outcomeText renders a session_outcome payload as recallable prose.
func outcomeText(payload []byte) string {
	var p struct {
		SessionID string `json:"session_id"`
		Outcome   string `json:"outcome"`
		Evidence  string `json:"evidence"`
	}
	if json.Unmarshal(payload, &p) != nil || p.Outcome == "" {
		return ""
	}
	return fmt.Sprintf("agent session %s outcome: %s — %s", p.SessionID, p.Outcome, p.Evidence)
}

// rankBM25 scores every chunk with BM25, multiplies in a query-aware weight, adds
// an exact-phrase bonus, and returns the top k positive-scoring hits.
func rankBM25(chunks []Chunk, query, scopePath string, k int) []Hit {
	n := len(chunks)
	docTokens := make([][]string, n)
	df := map[string]int{}
	totalLen := 0
	for i, c := range chunks {
		toks := tokenize(c.Text)
		docTokens[i] = toks
		totalLen += len(toks)
		for term := range uniq(toks) {
			df[term]++
		}
	}
	avgdl := 1.0
	if totalLen > 0 {
		avgdl = float64(totalLen) / float64(n)
	}

	qTerms := uniq(tokenize(query))
	phrase := strings.ToLower(strings.TrimSpace(query))
	history := asksAboutHistory(phrase)

	hits := make([]Hit, 0, n)
	for i, c := range chunks {
		bm := 0.0
		tf := termFreq(docTokens[i])
		dl := float64(len(docTokens[i]))
		for term := range qTerms {
			f := float64(tf[term])
			if f == 0 {
				continue
			}
			idf := math.Log(1 + (float64(n)-float64(df[term])+0.5)/(float64(df[term])+0.5))
			bm += idf * (f * (bm25K1 + 1)) / (f + bm25K1*(1-bm25B+bm25B*dl/avgdl))
		}
		w := weight(c, history, scopePath)
		score := bm * w
		if phrase != "" && strings.Contains(strings.ToLower(c.Text), phrase) {
			score += phraseBonus * w
		}
		if score > 0 {
			hits = append(hits, Hit{Chunk: c, Score: score})
		}
	}

	sort.Slice(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	if k > 0 && k < len(hits) {
		hits = hits[:k]
	}
	return hits
}

// weight is the static prior for a chunk: current claims and authoritative docs
// rank highest; stale/conflicted material is penalized UNLESS the query asks
// about history/conflicts, in which case it is surfaced.
func weight(c Chunk, history bool, scopePath string) float64 {
	var w float64
	switch c.Cat {
	case CatClaim:
		switch c.Status {
		case "current":
			w = 1.4
		case "candidate":
			w = 1.0
		case "conflicted":
			w = pick(history, 1.3, 0.8)
		case "stale":
			w = pick(history, 1.2, 0.6)
		default:
			w = 1.0
		}
	case CatDoc:
		switch c.Kind {
		case "claude", "codex/agents":
			w = 1.3 // the explicit instruction files outrank generic docs
		case "readme":
			w = 1.1
		case "docs":
			w = 1.0
		default:
			w = 1.05
		}
		if c.Stale {
			w *= pick(history, 1.2, 0.6)
		}
		w *= recencyBoost(c.When)
	case CatEvent:
		if c.Kind == "decision" || c.Kind == "assertion" {
			w = 1.2
		} else {
			w = 1.0
		}
		w *= recencyBoost(c.When)
	case CatRule:
		// A promoted rule crossed the evidence bar (≥3 signals, ≥2 sessions) —
		// the user's own confirmed doctrine outranks a doc paragraph on the
		// same terms.
		w = 1.5
		w *= recencyBoost(c.When)
	default:
		w = 1.0
	}
	if scopePath != "" && scopeMatch(c.Scope, scopePath) {
		w *= 1.3
	}
	return w
}

// asksAboutHistory reports whether the query is explicitly after stale/old/
// conflicting material, so the ranker surfaces it instead of penalizing it.
func asksAboutHistory(q string) bool {
	for _, m := range []string{"stale", "conflict", "old", "older", "previous", "history",
		"historical", "deprecated", "legacy", "used to", "formerly", "outdated", "superseded"} {
		if strings.Contains(q, m) {
			return true
		}
	}
	return false
}

// scopeMatch reports whether a chunk's scope governs (or is governed by) path.
func scopeMatch(scope, path string) bool {
	if scope == "" || scope == "." {
		return false
	}
	scope = strings.TrimSuffix(scope, "/")
	path = strings.TrimSuffix(path, "/")
	return scope == path || strings.HasPrefix(path, scope+"/") || strings.HasPrefix(scope, path+"/")
}

func pick(cond bool, a, b float64) float64 {
	if cond {
		return a
	}
	return b
}

// recencyBoost gives newer material a gentle multiplicative edge (1.0–1.25).
func recencyBoost(t time.Time) float64 {
	if t.IsZero() {
		return 1.0
	}
	days := time.Since(t).Hours() / 24
	switch {
	case days < 30:
		return 1.25
	case days < 90:
		return 1.15
	case days < 365:
		return 1.05
	default:
		return 1.0
	}
}

func parseDay(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// chunkProse splits a document into ~maxChunkChars chunks on blank-line
// (paragraph) boundaries, so each chunk is a coherent passage.
func chunkProse(text string) []string {
	paras := strings.Split(text, "\n\n")
	var chunks []string
	var cur strings.Builder
	flush := func() {
		if s := strings.TrimSpace(cur.String()); s != "" {
			chunks = append(chunks, s)
		}
		cur.Reset()
	}
	for _, p := range paras {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if cur.Len()+len(p) > maxChunkChars && cur.Len() > 0 {
			flush()
		}
		if cur.Len() > 0 {
			cur.WriteString("\n\n")
		}
		cur.WriteString(p)
	}
	flush()
	return chunks
}

// eventText pulls human-readable text out of an event payload.
func eventText(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(payload, &m); err != nil {
		return ""
	}
	var parts []string
	for _, key := range []string{"title", "text", "note", "message", "decision", "content", "reason"} {
		if v, ok := m[key]; ok {
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				parts = append(parts, s)
			}
		}
	}
	return strings.Join(parts, " — ")
}

// --- lexical helpers ---

// tokenize lowercases, splits on non-alphanumeric runs, drops 1-char tokens and
// common stopwords (so "where did we decide X" keys on "decide" and "X").
func tokenize(text string) []string {
	fields := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'))
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if len(f) <= 1 || stopwords[f] {
			continue
		}
		out = append(out, f)
	}
	return out
}

func uniq(toks []string) map[string]struct{} {
	m := make(map[string]struct{}, len(toks))
	for _, t := range toks {
		m[t] = struct{}{}
	}
	return m
}

func termFreq(toks []string) map[string]int {
	m := make(map[string]int, len(toks))
	for _, t := range toks {
		m[t]++
	}
	return m
}

var stopwords = map[string]bool{
	"the": true, "and": true, "for": true, "are": true, "but": true, "not": true,
	"you": true, "all": true, "any": true, "can": true, "did": true, "does": true,
	"how": true, "where": true, "what": true, "why": true, "who": true, "when": true,
	"with": true, "this": true, "that": true, "from": true, "have": true, "has": true,
	"our": true, "do": true, "is": true, "in": true, "of": true, "to": true,
	"on": true, "at": true, "it": true, "be": true, "or": true, "as": true, "an": true,
	"should": true, "would": true, "use": true,
}
