package sessionlog

import (
	"math"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Ranked session search. Replaces the old contiguous-substring scan, which
// scored recall 0.000 on LoCoMo and 0.001 on LongMemEval-S in the membench
// audit (2026-07-25): a paraphrased question simply never substring-matches
// the stored sentence. This ranker scored R@5 0.92 / R@10 0.955 on
// LongMemEval-S over the same corpora.
//
// Scoring: BM25 over a record's searchable text (constants mirror
// internal/recall), plus an adjacent-word-pair bonus, plus a large exact
// bonus when the raw query appears verbatim (the old substring behavior
// survives as the top tier), times a date-window factor when the query names
// a time ("in May 2024", "last week"). Results are ordered session-major:
// sessions ranked by the sum of their two best records, so one strong
// session surfaces as a group instead of interleaving with noise.

const (
	rankK1          = 1.5
	rankB           = 0.75
	rankExactBonus  = 6.0
	rankBigramBonus = 1.0
	perSessionLead  = 2 // records per session in the first pass of results

	rankRM3Docs   = 10  // feedback docs for pseudo-relevance expansion
	rankRM3Terms  = 8   // expansion terms added to the query
	rankRM3Weight = 0.4 // weight of an expansion term vs 1.0 for an original
	rankAdjacency = 0.3 // credit a record inherits from its strongest neighbor
	rankRRFK      = 60.0
)

// RankingOptions toggles the retrieval features layered on the BM25 core so
// membench can isolate each one's lift. Defaults are the measured winners
// (membench ablation, 2026-07-25): ALL OFF. Facts indexing — the data layer,
// not a knob here — delivered the lift on both benchmarks (LoCoMo R@5
// 0.502→0.612, LME R@5 held 0.93+); every global reranking layer measured
// neutral-to-harmful on at least one dataset (field weights flip sign across
// granularities, RM3 and adjacency trade R@1 for tail recall, session-level
// PPR+RRF scrambles good lexical orderings). They remain as options for
// ablation and for callers with a matching corpus shape.
type RankingOptions struct {
	FieldWeights bool // per-kind multipliers: facts > compaction > user > assistant > tool
	RM3          bool // pseudo-relevance feedback query expansion (skipped on exact-tier hits)
	Adjacency    bool // a strong turn credits its immediate neighbors — evidence sits next door
	EntityPPR    bool // personalized PageRank over the facts entity graph, RRF-fused at session level
}

// Ranking is the live configuration. The bench mutates it between adapter
// runs; the product path only ever reads it.
var Ranking = RankingOptions{}

// rankKindWeight is BM25F-lite: a fact states the answer in one sentence, a
// compaction summarizes it, dialogue mentions it, tool output buries it.
var rankKindWeight = map[string]float64{
	KindFacts:            1.2,
	KindCompaction:       1.1,
	KindUserMessage:      1.05,
	KindAssistantMessage: 1.0,
	KindToolCall:         0.9,
}

func kindWeight(kind string) float64 {
	if w, ok := rankKindWeight[kind]; ok {
		return w
	}
	return 1.0
}

var rankTokenRe = regexp.MustCompile(`[a-z0-9]+`)

var rankStopwords = map[string]bool{
	"the": true, "a": true, "an": true, "and": true, "or": true, "of": true,
	"to": true, "in": true, "on": true, "at": true, "is": true, "was": true,
	"are": true, "were": true, "do": true, "did": true, "does": true,
	"what": true, "when": true, "which": true, "who": true, "how": true,
	"i": true, "my": true, "me": true, "you": true, "your": true,
	"that": true, "this": true, "it": true, "for": true, "with": true,
	"have": true, "has": true, "had": true, "be": true, "about": true,
}

func rankTokenize(text string) []string {
	raw := rankTokenRe.FindAllString(strings.ToLower(text), -1)
	out := raw[:0]
	for _, t := range raw {
		if rankStopwords[t] {
			continue
		}
		out = append(out, rankStem(t))
	}
	return out
}

// rankStem is deliberately light: enough that surface variants meet, short
// tokens untouched to avoid collisions.
func rankStem(t string) string {
	switch {
	case len(t) > 5 && strings.HasSuffix(t, "ing"):
		return t[:len(t)-3]
	case len(t) > 4 && strings.HasSuffix(t, "ed"):
		return t[:len(t)-2]
	case len(t) > 4 && strings.HasSuffix(t, "ies"):
		return t[:len(t)-3] + "y"
	case len(t) > 3 && strings.HasSuffix(t, "s") && !strings.HasSuffix(t, "ss"):
		return t[:len(t)-1]
	}
	return t
}

func rankBigrams(toks []string) map[string]bool {
	if len(toks) < 2 {
		return nil
	}
	out := make(map[string]bool, len(toks)-1)
	for i := 0; i+1 < len(toks); i++ {
		out[toks[i]+" "+toks[i+1]] = true
	}
	return out
}

func recordHaystack(r Record) string {
	hay := r.Text + "\x00" + r.Content + "\x00" + r.Input + "\x00" + r.Tool
	// Lesson signals carry their content in Trigger/Strategy, and facts carry
	// entity keys — all of it is exactly the "what happened here" material
	// search exists to find.
	if r.Trigger != "" || r.Strategy != "" {
		hay += "\x00" + r.Trigger + "\x00" + r.Strategy
	}
	if len(r.Entities) > 0 {
		hay += "\x00" + strings.Join(r.Entities, " ")
	}
	return hay
}

type rankedRecord struct {
	rec   Record
	score float64
}

type rankDoc struct {
	sid  string
	rec  Record
	toks []string
	tf   map[string]int
}

// rankRecords scores every record against the query and returns them ordered
// session-major. now anchors relative date cues ("last week"). The optional
// layers (Ranking) run in order: field-weighted BM25 first pass → RM3 query
// expansion + rescore → neighbor adjacency → session aggregation → RRF fusion
// with the facts entity graph.
func rankRecords(bySession map[string][]Record, query string, now time.Time) []Record {
	qRaw := strings.ToLower(strings.TrimSpace(query))
	qToks := rankTokenize(query)
	qWeights := map[string]float64{}
	for _, t := range qToks {
		qWeights[t] = 1.0
	}
	qBi := rankBigrams(qToks)
	window := queryWindow(query, now)

	// Corpus stats. Docs of one session stay contiguous and in log order —
	// the adjacency pass depends on that.
	var docs []rankDoc
	df := map[string]int{}
	totalLen := 0
	for sid, recs := range bySession {
		for _, r := range recs {
			toks := rankTokenize(recordHaystack(r))
			tf := map[string]int{}
			for _, t := range toks {
				tf[t]++
			}
			docs = append(docs, rankDoc{sid, r, toks, tf})
			totalLen += len(toks)
			for t := range tf {
				df[t]++
			}
		}
	}
	n := len(docs)
	if n == 0 {
		return nil
	}
	avgdl := float64(totalLen) / float64(n)
	if avgdl == 0 {
		avgdl = 1
	}
	idf := func(term string) float64 {
		return math.Log(1 + (float64(n)-float64(df[term])+0.5)/(float64(df[term])+0.5))
	}

	score := func(d *rankDoc, weights map[string]float64) (float64, bool) {
		dl := float64(len(d.toks))
		s := 0.0
		for term, wq := range weights {
			f := float64(d.tf[term])
			if f == 0 {
				continue
			}
			s += wq * idf(term) * (f * (rankK1 + 1)) / (f + rankK1*(1-rankB+rankB*dl/avgdl))
		}
		if s > 0 && len(qBi) > 0 {
			for bg := range rankBigrams(d.toks) {
				if qBi[bg] {
					s += rankBigramBonus
				}
			}
		}
		// Exact tier: a verbatim substring hit always outranks fuzzy matches,
		// preserving the old search's guarantee for literal lookups.
		exact := qRaw != "" && strings.Contains(strings.ToLower(recordHaystack(d.rec)), qRaw)
		if exact {
			s += rankExactBonus
		}
		if Ranking.FieldWeights {
			s *= kindWeight(d.rec.Kind)
		}
		if window != nil && !d.rec.TS.IsZero() {
			if window.contains(d.rec.TS) {
				s *= 1.5
			} else {
				s *= 0.6
			}
		}
		return s, exact
	}

	scores := make([]float64, n)
	anyExact := false
	for i := range docs {
		s, exact := score(&docs[i], qWeights)
		scores[i] = s
		anyExact = anyExact || exact
	}

	// RM3: when nothing matched verbatim, let the best fuzzy matches vote on
	// what the query is "about" and rescore with those terms added. An exact
	// hit means the vocabulary already aligns — expansion could only dilute.
	if Ranking.RM3 && !anyExact {
		if expanded := rm3Expand(docs, scores, qWeights, idf); expanded != nil {
			for i := range docs {
				scores[i], _ = score(&docs[i], expanded)
			}
		}
	}

	// Adjacency: the evidence for a matching turn often sits in the turn next
	// to it (the answer to a matched question, the question behind a matched
	// answer). Credit flows one step, from the stronger neighbor.
	if Ranking.Adjacency {
		adj := make([]float64, n)
		copy(adj, scores)
		for i := range docs {
			best := 0.0
			if i > 0 && docs[i-1].sid == docs[i].sid && scores[i-1] > best {
				best = scores[i-1]
			}
			if i+1 < n && docs[i+1].sid == docs[i].sid && scores[i+1] > best {
				best = scores[i+1]
			}
			adj[i] += rankAdjacency * best
		}
		scores = adj
	}

	type sessAgg struct {
		id    string
		score float64
		hits  []rankedRecord
	}
	sessions := map[string]*sessAgg{}
	for i, d := range docs {
		if scores[i] <= 0 {
			continue
		}
		s := sessions[d.sid]
		if s == nil {
			s = &sessAgg{id: d.sid}
			sessions[d.sid] = s
		}
		s.hits = append(s.hits, rankedRecord{d.rec, scores[i]})
	}
	if len(sessions) == 0 {
		return nil
	}

	order := make([]*sessAgg, 0, len(sessions))
	for _, s := range sessions {
		sort.SliceStable(s.hits, func(i, j int) bool { return s.hits[i].score > s.hits[j].score })
		s.score = s.hits[0].score
		if len(s.hits) > 1 {
			s.score += s.hits[1].score
		}
		order = append(order, s)
	}
	sort.SliceStable(order, func(i, j int) bool { return order[i].score > order[j].score })

	// Entity-graph fusion: PPR ranks sessions by what they were ABOUT (facts
	// entities), lexical rank by what they literally said. Reciprocal-rank
	// fusion blends the two orderings; no facts or no seed match = no-op.
	// RRF is rank-only and would let a thematic match demote a verbatim one,
	// so — like RM3 — it stands down when the exact tier fired.
	if Ranking.EntityPPR && !anyExact {
		if ppr := entityPPRSessions(bySession, qToks); len(ppr) > 0 {
			pprRank := map[string]int{}
			for i, id := range ppr {
				pprRank[id] = i
			}
			fused := map[string]float64{}
			for i, s := range order {
				f := 1.0 / (rankRRFK + float64(i+1))
				if pr, ok := pprRank[s.id]; ok {
					f += 1.0 / (rankRRFK + float64(pr+1))
				}
				fused[s.id] = f
			}
			sort.SliceStable(order, func(i, j int) bool { return fused[order[i].id] > fused[order[j].id] })
		}
	}

	// First pass: the lead records of every relevant session. Second pass:
	// the remainder, so deep sessions still yield all their hits.
	var out []Record
	for _, s := range order {
		lead := perSessionLead
		if lead > len(s.hits) {
			lead = len(s.hits)
		}
		for i := 0; i < lead; i++ {
			out = append(out, s.hits[i].rec)
		}
	}
	for _, s := range order {
		for i := perSessionLead; i < len(s.hits); i++ {
			out = append(out, s.hits[i].rec)
		}
	}
	return out
}

// rm3Expand implements RM3-style pseudo-relevance feedback with zero extra
// I/O: the top feedback docs from the first pass vote terms by tf-idf mass,
// and the best non-query terms join the query at rankRM3Weight. Deterministic
// (ties break on the term string). Nil when there is nothing to learn from.
func rm3Expand(docs []rankDoc, scores []float64, base map[string]float64, idf func(string) float64) map[string]float64 {
	type ds struct {
		i int
		s float64
	}
	var top []ds
	for i, s := range scores {
		if s > 0 {
			top = append(top, ds{i, s})
		}
	}
	if len(top) == 0 {
		return nil
	}
	sort.Slice(top, func(a, b int) bool { return top[a].s > top[b].s })
	if len(top) > rankRM3Docs {
		top = top[:rankRM3Docs]
	}
	mass := map[string]float64{}
	for _, t := range top {
		for term, f := range docs[t.i].tf {
			if base[term] > 0 {
				continue
			}
			mass[term] += float64(f) * idf(term)
		}
	}
	if len(mass) == 0 {
		return nil
	}
	type tw struct {
		term string
		w    float64
	}
	terms := make([]tw, 0, len(mass))
	for t, w := range mass {
		terms = append(terms, tw{t, w})
	}
	sort.Slice(terms, func(a, b int) bool {
		if terms[a].w != terms[b].w {
			return terms[a].w > terms[b].w
		}
		return terms[a].term < terms[b].term
	})
	if len(terms) > rankRM3Terms {
		terms = terms[:rankRM3Terms]
	}
	out := make(map[string]float64, len(base)+len(terms))
	for t, w := range base {
		out[t] = w
	}
	for _, t := range terms {
		out[t.term] = rankRM3Weight
	}
	return out
}

// ─── query date cues ────────────────────────────────────────────────────────

type queryTimeWindow struct{ from, to time.Time }

func (w queryTimeWindow) contains(t time.Time) bool {
	return !t.Before(w.from) && t.Before(w.to)
}

var (
	rankYearRe  = regexp.MustCompile(`\b(20\d{2})\b`)
	rankMonthRe = regexp.MustCompile(`(?i)\b(january|february|march|april|may|june|july|august|september|october|november|december)\b`)
	rankRelRe   = regexp.MustCompile(`(?i)\b(last|this|past)\s+(year|month|week)\b|\byesterday\b`)
)

var rankMonthNum = map[string]time.Month{
	"january": 1, "february": 2, "march": 3, "april": 4, "may": 5, "june": 6,
	"july": 7, "august": 8, "september": 9, "october": 10, "november": 11, "december": 12,
}

func queryWindow(text string, now time.Time) *queryTimeWindow {
	yearM := rankYearRe.FindStringSubmatch(text)
	monM := rankMonthRe.FindStringSubmatch(text)
	switch {
	case yearM != nil && monM != nil:
		y := atoi4(yearM[1])
		from := time.Date(y, rankMonthNum[strings.ToLower(monM[1])], 1, 0, 0, 0, 0, time.UTC)
		w := queryTimeWindow{from, from.AddDate(0, 1, 0)}
		return &w
	case yearM != nil:
		from := time.Date(atoi4(yearM[1]), 1, 1, 0, 0, 0, 0, time.UTC)
		w := queryTimeWindow{from, from.AddDate(1, 0, 0)}
		return &w
	case monM != nil && !now.IsZero():
		m := rankMonthNum[strings.ToLower(monM[1])]
		y := now.Year()
		if m > now.Month() {
			y--
		}
		from := time.Date(y, m, 1, 0, 0, 0, 0, time.UTC)
		w := queryTimeWindow{from, from.AddDate(0, 1, 0)}
		return &w
	}
	if m := rankRelRe.FindStringSubmatch(text); m != nil && !now.IsZero() {
		switch {
		case strings.EqualFold(m[0], "yesterday"):
			from := now.AddDate(0, 0, -1).Truncate(24 * time.Hour)
			w := queryTimeWindow{from, from.AddDate(0, 0, 1)}
			return &w
		case strings.EqualFold(m[2], "year"):
			y := now.Year()
			if !strings.EqualFold(m[1], "this") {
				y--
			}
			from := time.Date(y, 1, 1, 0, 0, 0, 0, time.UTC)
			w := queryTimeWindow{from, from.AddDate(1, 0, 0)}
			return &w
		case strings.EqualFold(m[2], "month"):
			from := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
			if !strings.EqualFold(m[1], "this") {
				from = from.AddDate(0, -1, 0)
			}
			w := queryTimeWindow{from, from.AddDate(0, 1, 0)}
			return &w
		case strings.EqualFold(m[2], "week"):
			from := now.Truncate(24*time.Hour).AddDate(0, 0, -7)
			if strings.EqualFold(m[1], "this") {
				from = from.AddDate(0, 0, 7)
			}
			w := queryTimeWindow{from, from.AddDate(0, 0, 7)}
			return &w
		}
	}
	return nil
}

func atoi4(s string) int {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}
