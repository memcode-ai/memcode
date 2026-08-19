package membench

import (
	"math"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/memcode-ai/memcode/internal/sessionlog"
)

// ─── product: the shipping sessionlog.Search ────────────────────────────────

// ProductAdapter runs the production session-search code path exactly as the
// memcode{session,search} tool runs it. Since the Phase B promotion this is
// the ranked search; the pre-promotion substring behavior lives on as
// LegacyAdapter for the before/after table. Opts installs a ranking-feature
// variant (via Prepare, before workers spawn) so the ablation table isolates
// each layer's lift; nil keeps whatever is installed.
type ProductAdapter struct {
	Variant string
	Opts    *sessionlog.RankingOptions
}

func (a ProductAdapter) Name() string {
	if a.Variant == "" {
		return "product"
	}
	return "product-" + a.Variant
}

func (a ProductAdapter) Prepare() {
	if a.Opts != nil {
		sessionlog.Ranking = *a.Opts
	}
}

func (ProductAdapter) Rank(root string, q Question, _ []SessionDoc, k int) ([]string, error) {
	recs, err := sessionlog.Search(root, q.Text, k)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, r := range recs {
		if r.Slug != "" {
			out = append(out, r.Slug)
		}
	}
	return out, nil
}

// LegacyAdapter (the pre-2026-07-25 substring scan) lives in legacy.go behind
// the `membench` build tag — bench-only history that must not compile into a
// release binary; legacy_stub.go keeps the type present without the tag.

// ─── bm25: lexical ranking over the ingested turns ──────────────────────────

// BM25 parameters mirror internal/recall (k1=1.5, b=0.75) so a future
// promotion into sessionlog.Search inherits tuned constants, not new ones.
const (
	bm25K1 = 1.5
	bm25B  = 0.75
)

// BM25Adapter tokenizes the question and every ingested turn and ranks turns
// by BM25. Reads the turns back from the ingested session logs (not the
// in-memory dataset) so the whole file pipeline is exercised.
type BM25Adapter struct {
	// Time-aware scoring (the bm25+time adapter is this struct with TimeAware
	// set): explicit date cues in the question define a window that boosts
	// in-window turns and dampens out-of-window ones, and knowledge-update
	// questions get a recency tiebreak so the latest version of a changed
	// fact outranks the original.
	TimeAware bool
	// V2 layers the accuracy work on top: light stemming, bigram matching,
	// category-aware query expansion, and relative-date windows. Kept as a
	// flag so v1 numbers stay reproducible for the before/after table.
	V2 bool
}

func (a BM25Adapter) Name() string {
	switch {
	case a.V2 && a.TimeAware:
		return "bm25v2+time"
	case a.V2:
		return "bm25v2"
	case a.TimeAware:
		return "bm25+time"
	}
	return "bm25"
}

type turnDoc struct {
	id   string
	toks []string
	ts   time.Time
}

type scoredTurn struct {
	id    string
	score float64
	ts    time.Time
}

// rankSessions aggregates turn scores per session (sum of the top two turns:
// one great line should not lose to a session with two good ones) and emits
// turn ids session-major so the runner's first-appearance collapse yields the
// session ranking. Turns arrive score-sorted.
func (a BM25Adapter) rankSessions(hits []scoredTurn, knowledgeUpdate bool, k int) []string {
	type sess struct {
		id    string
		score float64
		ts    time.Time
		turns []scoredTurn
	}
	byID := map[string]*sess{}
	var order []*sess
	for _, h := range hits {
		sid := h.id
		if i := strings.LastIndex(h.id, "#"); i > 0 {
			sid = h.id[:i]
		}
		s := byID[sid]
		if s == nil {
			s = &sess{id: sid}
			byID[sid] = s
			order = append(order, s)
		}
		s.turns = append(s.turns, h)
		if h.ts.After(s.ts) {
			s.ts = h.ts
		}
	}
	for _, s := range order {
		s.score = s.turns[0].score // score-sorted input: top-2 is a prefix
		if len(s.turns) > 1 {
			s.score += s.turns[1].score
		}
	}
	sort.SliceStable(order, func(i, j int) bool { return order[i].score > order[j].score })

	if knowledgeUpdate && len(order) > 1 {
		// Tighter band than the v1 turn-level rerank (which regressed R@1):
		// only near-ties defer to recency.
		floor := order[0].score * 0.8
		cut := 0
		for cut < len(order) && order[cut].score >= floor {
			cut++
		}
		head := order[:cut]
		sort.SliceStable(head, func(i, j int) bool { return head[i].ts.After(head[j].ts) })
	}

	// At most two turns per session in the output so a deep session cannot
	// crowd later sessions out of the k-capped id list before the runner's
	// session collapse.
	var out []string
	for _, s := range order {
		for i, t := range s.turns {
			if i >= 2 {
				break
			}
			out = append(out, t.id)
			if k > 0 && len(out) >= k {
				return out
			}
		}
	}
	return out
}

func (a BM25Adapter) Rank(root string, q Question, docs []SessionDoc, k int) ([]string, error) {
	lex := tokenize
	if a.V2 {
		lex = tokenizeV2
	}
	// Read back what Ingest wrote: file truth, matching production reads.
	var turns []turnDoc
	for _, doc := range docs {
		recs, err := sessionlog.Recent(root, doc.ID, 0)
		if err != nil {
			continue
		}
		for _, r := range recs {
			if r.Slug == "" || r.Text == "" {
				continue
			}
			turns = append(turns, turnDoc{id: r.Slug, toks: lex(r.Text), ts: r.TS})
		}
	}

	n := len(turns)
	if n == 0 {
		return nil, nil
	}
	df := map[string]int{}
	totalLen := 0
	for _, t := range turns {
		totalLen += len(t.toks)
		seen := map[string]bool{}
		for _, tok := range t.toks {
			if !seen[tok] {
				seen[tok] = true
				df[tok]++
			}
		}
	}
	avgdl := float64(totalLen) / float64(n)
	if avgdl == 0 {
		avgdl = 1
	}

	qTokList := lex(q.Text)
	if a.V2 {
		qTokList = append(qTokList, expandForType(q.Type)...)
	}
	qToks := map[string]bool{}
	for _, t := range qTokList {
		qToks[t] = true
	}
	var qBigrams map[string]bool
	if a.V2 {
		qBigrams = bigrams(lex(q.Text))
	}

	var window *timeWindow
	if a.TimeAware {
		window = extractWindow(q.Text, q.Date)
		if window == nil && a.V2 {
			window = relativeWindow(q.Text, q.Date)
		}
	}
	knowledgeUpdate := a.TimeAware && strings.Contains(q.Type, "knowledge-update")

	hits := make([]scoredTurn, 0, n)
	for _, t := range turns {
		tf := map[string]int{}
		for _, tok := range t.toks {
			tf[tok]++
		}
		dl := float64(len(t.toks))
		bm := 0.0
		for term := range qToks {
			f := float64(tf[term])
			if f == 0 {
				continue
			}
			idf := math.Log(1 + (float64(n)-float64(df[term])+0.5)/(float64(df[term])+0.5))
			bm += idf * (f * (bm25K1 + 1)) / (f + bm25K1*(1-bm25B+bm25B*dl/avgdl))
		}
		if bm == 0 {
			continue
		}
		if a.V2 && len(qBigrams) > 0 {
			// Adjacent-pair matches reward phrase overlap that unigram BM25
			// can't see ("golden retriever" vs the words apart).
			for bg := range bigrams(t.toks) {
				if qBigrams[bg] {
					bm += 1.0
				}
			}
		}
		if window != nil && !t.ts.IsZero() {
			if window.contains(t.ts) {
				bm *= 1.5
			} else {
				bm *= 0.6
			}
		}
		hits = append(hits, scoredTurn{t.id, bm, t.ts})
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].score > hits[j].score })

	if a.V2 && q.Gran == BySession {
		return a.rankSessions(hits, knowledgeUpdate, k), nil
	}

	// Knowledge updates: among plausibly-relevant hits (within half of the
	// top score) the LATEST mention wins — supersession as reranking, since a
	// changed fact's update usually shares fewer terms with the question than
	// the original statement did.
	if knowledgeUpdate && len(hits) > 1 {
		floor := hits[0].score * 0.5
		cut := 0
		for cut < len(hits) && hits[cut].score >= floor {
			cut++
		}
		head := hits[:cut]
		sort.SliceStable(head, func(i, j int) bool { return head[i].ts.After(head[j].ts) })
	}
	if k > 0 && k < len(hits) {
		hits = hits[:k]
	}
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.id
	}
	return out, nil
}

// ─── shared lexing / temporal cues ──────────────────────────────────────────

var tokenRe = regexp.MustCompile(`[a-z0-9]+`)

var stopwords = map[string]bool{
	"the": true, "a": true, "an": true, "and": true, "or": true, "of": true,
	"to": true, "in": true, "on": true, "at": true, "is": true, "was": true,
	"are": true, "were": true, "do": true, "did": true, "does": true,
	"what": true, "when": true, "which": true, "who": true, "how": true,
	"i": true, "my": true, "me": true, "you": true, "your": true,
	"that": true, "this": true, "it": true, "for": true, "with": true,
	"have": true, "has": true, "had": true, "be": true, "about": true,
}

func tokenize(text string) []string {
	raw := tokenRe.FindAllString(strings.ToLower(text), -1)
	out := raw[:0]
	for _, t := range raw {
		if !stopwords[t] {
			out = append(out, t)
		}
	}
	return out
}

// tokenizeV2 adds light suffix stripping so surface variants meet
// ("adopted"/"adopt", "puppies"/"puppy"-ish). Deliberately not a full
// stemmer; short tokens are left alone to avoid collisions.
func tokenizeV2(text string) []string {
	toks := tokenize(text)
	for i, t := range toks {
		toks[i] = stemLite(t)
	}
	return toks
}

func stemLite(t string) string {
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

func bigrams(toks []string) map[string]bool {
	if len(toks) < 2 {
		return nil
	}
	out := make(map[string]bool, len(toks)-1)
	for i := 0; i+1 < len(toks); i++ {
		out[toks[i]+" "+toks[i+1]] = true
	}
	return out
}

// expandForType adds retrieval cues the question implies but rarely states.
// Preference questions ask "what does the user prefer" while the evidence
// says "I love/always/usually…"; assistant-fact questions point at assistant
// phrasing. Tokens are pre-stemmed to match tokenizeV2 output.
func expandForType(qtype string) []string {
	switch {
	case strings.Contains(qtype, "preference"):
		return []string{"prefer", "favorite", "usually", "alway"}
	default:
		return nil
	}
}

var relRe = regexp.MustCompile(`(?i)\b(last|this|past)\s+(year|month|week)\b|\byesterday\b`)

// relativeWindow resolves "last year/month/week" and "yesterday" against the
// question's own date. Only used when no explicit date cue matched.
func relativeWindow(text string, qdate time.Time) *timeWindow {
	if qdate.IsZero() {
		return nil
	}
	m := relRe.FindStringSubmatch(text)
	if m == nil {
		return nil
	}
	switch {
	case strings.EqualFold(m[0], "yesterday"):
		from := qdate.AddDate(0, 0, -1).Truncate(24 * time.Hour)
		return &timeWindow{from, from.AddDate(0, 0, 1)}
	case strings.EqualFold(m[2], "year"):
		y := qdate.Year()
		if strings.EqualFold(m[1], "last") || strings.EqualFold(m[1], "past") {
			y--
		}
		from := time.Date(y, 1, 1, 0, 0, 0, 0, time.UTC)
		return &timeWindow{from, from.AddDate(1, 0, 0)}
	case strings.EqualFold(m[2], "month"):
		from := time.Date(qdate.Year(), qdate.Month(), 1, 0, 0, 0, 0, time.UTC)
		if strings.EqualFold(m[1], "last") || strings.EqualFold(m[1], "past") {
			from = from.AddDate(0, -1, 0)
		}
		return &timeWindow{from, from.AddDate(0, 1, 0)}
	case strings.EqualFold(m[2], "week"):
		from := qdate.Truncate(24*time.Hour).AddDate(0, 0, -7)
		if strings.EqualFold(m[1], "this") {
			from = from.AddDate(0, 0, 7)
		}
		return &timeWindow{from, from.AddDate(0, 0, 7)}
	}
	return nil
}

type timeWindow struct{ from, to time.Time }

func (w timeWindow) contains(t time.Time) bool {
	return !t.Before(w.from) && t.Before(w.to)
}

var (
	yearRe  = regexp.MustCompile(`\b(20\d{2})\b`)
	monthRe = regexp.MustCompile(`(?i)\b(january|february|march|april|may|june|july|august|september|october|november|december)\b`)
)

var monthNum = map[string]time.Month{
	"january": 1, "february": 2, "march": 3, "april": 4, "may": 5, "june": 6,
	"july": 7, "august": 8, "september": 9, "october": 10, "november": 11, "december": 12,
}

// extractWindow turns explicit date cues in a question into a coarse window:
// month+year → that month; bare year → that year; bare month → that month in
// the question's own year when known. Nil when the question names no dates.
func extractWindow(text string, qdate time.Time) *timeWindow {
	yearM := yearRe.FindStringSubmatch(text)
	monM := monthRe.FindStringSubmatch(text)
	switch {
	case yearM != nil && monM != nil:
		y := atoiSafe(yearM[1])
		m := monthNum[strings.ToLower(monM[1])]
		from := time.Date(y, m, 1, 0, 0, 0, 0, time.UTC)
		return &timeWindow{from, from.AddDate(0, 1, 0)}
	case yearM != nil:
		y := atoiSafe(yearM[1])
		from := time.Date(y, 1, 1, 0, 0, 0, 0, time.UTC)
		return &timeWindow{from, from.AddDate(1, 0, 0)}
	case monM != nil && !qdate.IsZero():
		m := monthNum[strings.ToLower(monM[1])]
		y := qdate.Year()
		// A month later in the calendar than the question date means last year.
		if m > qdate.Month() {
			y--
		}
		from := time.Date(y, m, 1, 0, 0, 0, 0, time.UTC)
		return &timeWindow{from, from.AddDate(0, 1, 0)}
	}
	return nil
}

func atoiSafe(s string) int {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}

// shippedRanking captures the product defaults at init, before any ablation
// variant mutates the global, so "product" always means what ships.
var shippedRanking = sessionlog.Ranking

// productVariants is the ablation ladder: each step turns on one more layer,
// so adjacent rows isolate that layer's lift. "all" equals shipped defaults.
func productVariants() []Adapter {
	mk := func(v string, o sessionlog.RankingOptions) Adapter {
		opts := o
		return ProductAdapter{Variant: v, Opts: &opts}
	}
	return []Adapter{
		mk("base", sessionlog.RankingOptions{}),
		mk("fw", sessionlog.RankingOptions{FieldWeights: true}),
		mk("fw+rm3", sessionlog.RankingOptions{FieldWeights: true, RM3: true}),
		mk("fw+rm3+adj", sessionlog.RankingOptions{FieldWeights: true, RM3: true, Adjacency: true}),
		mk("all", sessionlog.RankingOptions{FieldWeights: true, RM3: true, Adjacency: true, EntityPPR: true}),
	}
}

// Adapters returns the named adapter set ("all" = every adapter).
func Adapters(name string) []Adapter {
	switch name {
	case "legacy":
		return []Adapter{LegacyAdapter{}}
	case "product", "naive":
		return []Adapter{ProductAdapter{Opts: &shippedRanking}}
	case "ablation":
		return productVariants()
	case "bm25":
		return []Adapter{BM25Adapter{}}
	case "bm25+time", "bm25time":
		return []Adapter{BM25Adapter{TimeAware: true}}
	case "bm25v2":
		return []Adapter{BM25Adapter{V2: true}}
	case "bm25v2+time":
		return []Adapter{BM25Adapter{V2: true, TimeAware: true}}
	default:
		return []Adapter{
			LegacyAdapter{}, ProductAdapter{}, BM25Adapter{V2: true, TimeAware: true},
		}
	}
}
