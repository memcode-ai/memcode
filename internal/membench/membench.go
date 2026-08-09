// Package membench evaluates memcode's session-memory retrieval against public
// conversational-memory benchmarks (LongMemEval, LoCoMo) with ZERO model calls.
//
// Each benchmark question's chat history is ingested as real .memcode session
// logs (one events.jsonl per benchmark session, timestamps preserved), then
// retrieval adapters rank content for the question and are scored against the
// benchmark's own evidence labels (answer_session_ids / evidence dia_ids).
// Metrics: recall@k and nDCG@k. The point is to measure the retrieval layer
// exactly as the product exercises it, before any answer-mode run.
package membench

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// Turn is one benchmark utterance. ID is the benchmark's identity for the
// turn (LongMemEval: "<session>#<idx>"; LoCoMo: the dia_id) and rides the
// ingested Record so ranked results map straight back to evidence labels.
type Turn struct {
	Role     string // "user" | "assistant"
	Text     string
	ID       string
	Evidence bool
}

// SessionDoc is one benchmark chat session. Facts are attached by
// GenerateFacts and ingested as KindFacts records, mirroring what the
// product's cognition loop appends to real sessions.
type SessionDoc struct {
	ID    string
	TS    time.Time
	Turns []Turn
	Facts []Fact
}

// Question is one scored instance. Gold holds the evidence ids at the
// benchmark's native granularity: session ids for LongMemEval, dia ids for
// LoCoMo. Haystack is the history visible to this question.
type Question struct {
	ID       string
	Type     string
	Text     string
	Answer   string // gold answer text (QA mode)
	Date     time.Time
	Gold     map[string]bool
	Haystack []SessionDoc
	Gran     Granularity // stamped from the dataset by Run; adapters may branch on it
}

// Granularity of the ranked unit a dataset is scored at.
type Granularity int

const (
	BySession Granularity = iota
	ByTurn
)

// Dataset is a fully parsed benchmark.
type Dataset struct {
	Name      string
	Gran      Granularity
	Questions []Question
}

// Adapter ranks turns for a question over an ingested root and returns turn
// IDs, best first. The scorer aggregates turns to sessions when the dataset
// is session-granular.
type Adapter interface {
	Name() string
	Rank(root string, q Question, docs []SessionDoc, k int) ([]string, error)
}

// QuestionResult is the scored outcome for one question under one adapter.
type QuestionResult struct {
	QuestionID string
	Type       string
	Ranked     []string
	RecallAtK  map[int]float64
	NDCGAtK    map[int]float64
}

// RunResult aggregates one adapter's scores over a dataset.
type RunResult struct {
	Dataset   string
	Adapter   string
	Questions int
	Skipped   int // no-evidence questions (abstention / adversarial)
	Recall    map[int]float64
	NDCG      map[int]float64
	ByType    map[string]typeAgg
	PerQ      []QuestionResult
}

type typeAgg struct {
	N      int
	Recall map[int]float64
}

var kCuts = []int{1, 3, 5, 10}

// Run ingests each question's haystack into a throwaway .memcode root, ranks
// with the adapter, and scores against the gold labels. Workers bound the
// ingest cost; every root is deleted unless keep is set.
func Run(ds *Dataset, ad Adapter, workDir string, limit int, keep bool) (*RunResult, error) {
	qs := ds.Questions
	if limit > 0 && limit < len(qs) {
		qs = qs[:limit]
	}

	res := &RunResult{
		Dataset: ds.Name,
		Adapter: ad.Name(),
		Recall:  map[int]float64{},
		NDCG:    map[int]float64{},
		ByType:  map[string]typeAgg{},
	}
	prepareAdapter(ad)

	type outcome struct {
		qr   QuestionResult
		skip bool
		err  error
	}
	outs := make([]outcome, len(qs))

	var wg sync.WaitGroup
	sem := make(chan struct{}, max(1, runtime.NumCPU()-1))
	for i := range qs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			q := qs[i]
			q.Gran = ds.Gran
			if len(q.Gold) == 0 { // abstention / adversarial: no retrieval target
				outs[i] = outcome{skip: true}
				return
			}
			root, err := os.MkdirTemp(workDir, "membench-*")
			if err != nil {
				outs[i] = outcome{err: err}
				return
			}
			if !keep {
				defer os.RemoveAll(root)
			}
			if err := Ingest(root, q.Haystack); err != nil {
				outs[i] = outcome{err: fmt.Errorf("ingest %s: %w", q.ID, err)}
				return
			}
			ranked, err := ad.Rank(root, q, q.Haystack, maxK()*4)
			if err != nil {
				outs[i] = outcome{err: fmt.Errorf("rank %s: %w", q.ID, err)}
				return
			}
			// Facts records reuse their source turn's id, so a ranked list can
			// repeat an id; first appearance wins or gold hits double-count.
			ranked = dedupeIDs(ranked)
			if ds.Gran == BySession {
				ranked = turnsToSessions(ranked)
			}
			outs[i] = outcome{qr: score(q, ranked)}
		}(i)
	}
	wg.Wait()

	for _, o := range outs {
		if o.err != nil {
			return nil, o.err
		}
		if o.skip {
			res.Skipped++
			continue
		}
		res.PerQ = append(res.PerQ, o.qr)
	}
	aggregate(res)
	return res, nil
}

func maxK() int { return kCuts[len(kCuts)-1] }

// prepareAdapter runs an adapter's one-time setup (e.g. installing its
// ranking-option variant) BEFORE workers spawn, so per-question Rank calls
// never touch shared state concurrently.
func prepareAdapter(ad Adapter) {
	if p, ok := ad.(interface{ Prepare() }); ok {
		p.Prepare()
	}
}

// SplitQuestions carves a tune/holdout split by question index parity so knob
// tuning ("even") never reads the reporting set ("odd"). "all"/"" is identity.
func SplitQuestions(ds *Dataset, split string) (*Dataset, error) {
	switch split {
	case "", "all":
		return ds, nil
	case "even", "odd":
		want := 0
		if split == "odd" {
			want = 1
		}
		out := &Dataset{Name: ds.Name + "/" + split, Gran: ds.Gran}
		for i, q := range ds.Questions {
			if i%2 == want {
				out.Questions = append(out.Questions, q)
			}
		}
		return out, nil
	default:
		return nil, fmt.Errorf("unknown split %q (even | odd | all)", split)
	}
}

func dedupeIDs(ids []string) []string {
	seen := map[string]bool{}
	out := ids[:0]
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// turnsToSessions collapses a ranked turn list to a ranked session list,
// first appearance wins. Turn IDs are "<session>#<idx>".
func turnsToSessions(ranked []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, id := range ranked {
		s := id
		if i := strings.LastIndex(id, "#"); i > 0 {
			s = id[:i]
		}
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func score(q Question, ranked []string) QuestionResult {
	qr := QuestionResult{
		QuestionID: q.ID,
		Type:       q.Type,
		Ranked:     ranked,
		RecallAtK:  map[int]float64{},
		NDCGAtK:    map[int]float64{},
	}
	for _, k := range kCuts {
		top := ranked
		if len(top) > k {
			top = top[:k]
		}
		hit := 0
		dcg := 0.0
		for i, id := range top {
			if q.Gold[id] {
				hit++
				dcg += 1.0 / log2(float64(i+2))
			}
		}
		ideal := 0.0
		n := min(k, len(q.Gold))
		for i := 0; i < n; i++ {
			ideal += 1.0 / log2(float64(i+2))
		}
		qr.RecallAtK[k] = float64(hit) / float64(len(q.Gold))
		if ideal > 0 {
			qr.NDCGAtK[k] = dcg / ideal
		}
	}
	return qr
}

func aggregate(res *RunResult) {
	res.Questions = len(res.PerQ)
	if res.Questions == 0 {
		return
	}
	byType := map[string]*typeAgg{}
	for _, qr := range res.PerQ {
		ta := byType[qr.Type]
		if ta == nil {
			ta = &typeAgg{Recall: map[int]float64{}}
			byType[qr.Type] = ta
		}
		ta.N++
		for _, k := range kCuts {
			res.Recall[k] += qr.RecallAtK[k]
			res.NDCG[k] += qr.NDCGAtK[k]
			ta.Recall[k] += qr.RecallAtK[k]
		}
	}
	for _, k := range kCuts {
		res.Recall[k] /= float64(res.Questions)
		res.NDCG[k] /= float64(res.Questions)
	}
	for name, ta := range byType {
		for _, k := range kCuts {
			ta.Recall[k] /= float64(ta.N)
		}
		res.ByType[name] = *ta
	}
}

// Render prints the result table.
func (r *RunResult) Render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s · adapter=%s · questions=%d (skipped %d without evidence)\n",
		r.Dataset, r.Adapter, r.Questions, r.Skipped)
	fmt.Fprintf(&b, "  %-10s", "")
	for _, k := range kCuts {
		fmt.Fprintf(&b, "R@%-6d", k)
	}
	fmt.Fprintf(&b, "NDCG@%d\n", maxK())
	fmt.Fprintf(&b, "  %-10s", "overall")
	for _, k := range kCuts {
		fmt.Fprintf(&b, "%-8.3f", r.Recall[k])
	}
	fmt.Fprintf(&b, "%.3f\n", r.NDCG[maxK()])

	types := make([]string, 0, len(r.ByType))
	for t := range r.ByType {
		types = append(types, t)
	}
	sort.Strings(types)
	for _, t := range types {
		ta := r.ByType[t]
		fmt.Fprintf(&b, "  %-28s n=%-4d", t, ta.N)
		for _, k := range kCuts {
			fmt.Fprintf(&b, "R@%d=%.3f  ", k, ta.Recall[k])
		}
		b.WriteString("\n")
	}
	return b.String()
}

// WriteLog persists per-question ranked ids for later parity runs against the
// benchmarks' own scoring scripts.
func (r *RunResult) WriteLog(dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, fmt.Sprintf("%s_%s.jsonl", r.Dataset, r.Adapter))
	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	for _, qr := range r.PerQ {
		fmt.Fprintf(f, `{"question_id":%q,"ranked":[`, qr.QuestionID)
		for i, id := range qr.Ranked {
			if i > 0 {
				f.WriteString(",")
			}
			fmt.Fprintf(f, "%q", id)
		}
		f.WriteString("]}\n")
	}
	return path, nil
}

func log2(x float64) float64 { return math.Log2(x) }
