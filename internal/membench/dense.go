package membench

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/memcode-ai/memcode/internal/sessionlog"
)

// Dense / hybrid retrieval. Lexical BM25 hits its ceiling on paraphrase
// (LoCoMo category-1: "What hobby did X pick up?" vs "I started throwing
// pots last month" share zero terms). Embeddings close that gap. The
// embedder is any OpenAI-compatible /embeddings endpoint, configured by env
// (no defaults; the CLI stays provider-agnostic):
//
//	MEMBENCH_EMBED_BASE   endpoint base URL, e.g. https://.../v1
//	MEMBENCH_EMBED_KEY    bearer key for that endpoint
//	MEMBENCH_EMBED_MODEL  embedding model name
//
// Embeddings run through a content-addressed cache under the bench cache
// dir so re-runs and adapter tuning don't re-bill.

type embedConfig struct {
	base, key, model string
}

func embedConfigFromEnv() (embedConfig, error) {
	cfg := embedConfig{
		base:  os.Getenv("MEMBENCH_EMBED_BASE"),
		key:   os.Getenv("MEMBENCH_EMBED_KEY"),
		model: os.Getenv("MEMBENCH_EMBED_MODEL"),
	}
	if cfg.base == "" || cfg.key == "" || cfg.model == "" {
		return cfg, fmt.Errorf("hybrid adapter needs MEMBENCH_EMBED_BASE, MEMBENCH_EMBED_KEY, and MEMBENCH_EMBED_MODEL (any OpenAI-compatible embeddings endpoint)")
	}
	return cfg, nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

const embedBatch = 64

func (c embedConfig) embed(texts []string) ([][]float32, error) {
	out := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += embedBatch {
		end := start + embedBatch
		if end > len(texts) {
			end = len(texts)
		}
		body, _ := json.Marshal(map[string]any{"model": c.model, "input": texts[start:end]})
		req, err := http.NewRequest("POST", strings.TrimRight(c.base, "/")+"/embeddings", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+c.key)
		req.Header.Set("Content-Type", "application/json")
		resp, err := (&http.Client{Timeout: 120 * time.Second}).Do(req)
		if err != nil {
			return nil, err
		}
		var parsed struct {
			Data []struct {
				Index     int       `json:"index"`
				Embedding []float32 `json:"embedding"`
			} `json:"data"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		err = json.NewDecoder(resp.Body).Decode(&parsed)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		if parsed.Error != nil {
			return nil, fmt.Errorf("embeddings API: %s", parsed.Error.Message)
		}
		if len(parsed.Data) != end-start {
			return nil, fmt.Errorf("embeddings API returned %d vectors for %d inputs", len(parsed.Data), end-start)
		}
		sort.Slice(parsed.Data, func(i, j int) bool { return parsed.Data[i].Index < parsed.Data[j].Index })
		for _, d := range parsed.Data {
			out = append(out, d.Embedding)
		}
	}
	return out, nil
}

// embedCached embeds texts through a content-addressed gzip JSON cache:
// the key hashes endpoint, model, and every input, so config or content
// changes can never serve stale vectors.
func (c embedConfig) embedCached(texts []string) ([][]float32, error) {
	h := sha256.New()
	h.Write([]byte(c.base + "\x00" + c.model))
	for _, t := range texts {
		h.Write([]byte{0})
		h.Write([]byte(t))
	}
	key := hex.EncodeToString(h.Sum(nil)[:16])
	path := filepath.Join(CacheDir(), "embeds", key+".json.gz")
	if vecs, err := readVecCache(path, len(texts)); err == nil {
		return vecs, nil
	}
	vecs, err := c.embed(texts)
	if err != nil {
		return nil, err
	}
	_ = writeVecCache(path, vecs)
	return vecs, nil
}

func readVecCache(path string, want int) ([][]float32, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	var vecs [][]float32
	if err := json.NewDecoder(gz).Decode(&vecs); err != nil {
		return nil, err
	}
	if len(vecs) != want {
		return nil, fmt.Errorf("cache size mismatch")
	}
	return vecs, nil
}

func writeVecCache(path string, vecs [][]float32) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	gz := gzip.NewWriter(f)
	if err := json.NewEncoder(gz).Encode(vecs); err != nil {
		f.Close()
		return err
	}
	if err := gz.Close(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func cosine(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// HybridAdapter blends the v2 lexical score with embedding cosine
// similarity: score = bm25/bm25max + HybridWeight*cosine. Turns are read
// back from the ingested logs like every other adapter.
type HybridAdapter struct {
	cfg          embedConfig
	HybridWeight float64
}

// NewHybridAdapter fails fast when no embedding key is configured.
func NewHybridAdapter() (*HybridAdapter, error) {
	cfg, err := embedConfigFromEnv()
	if err != nil {
		return nil, err
	}
	return &HybridAdapter{cfg: cfg, HybridWeight: 1.0}, nil
}

func (h *HybridAdapter) Name() string { return "hybrid" }

func (h *HybridAdapter) Rank(root string, q Question, docs []SessionDoc, k int) ([]string, error) {
	// Lexical half: the v2 adapter's turn scores, recomputed here so both
	// halves see identical turn sets.
	var ids []string
	var texts []string
	var tss []time.Time
	for _, doc := range docs {
		recs, err := sessionlog.Recent(root, doc.ID, 0)
		if err != nil {
			continue
		}
		for _, r := range recs {
			if r.Slug == "" || r.Text == "" {
				continue
			}
			ids = append(ids, r.Slug)
			texts = append(texts, r.Text)
			tss = append(tss, r.TS)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}

	lex := bm25Scores(texts, q.Text, q.Type)
	maxLex := 0.0
	for _, s := range lex {
		if s > maxLex {
			maxLex = s
		}
	}

	qv, err := h.cfg.embedCached([]string{q.Text})
	if err != nil {
		return nil, err
	}
	tv, err := h.cfg.embedCached(texts)
	if err != nil {
		return nil, err
	}

	hits := make([]scoredTurn, 0, len(ids))
	for i := range ids {
		s := cosine(qv[0], tv[i]) * h.HybridWeight
		if maxLex > 0 {
			s += lex[i] / maxLex
		}
		if s <= 0 {
			continue
		}
		hits = append(hits, scoredTurn{ids[i], s, tss[i]})
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].score > hits[j].score })

	if q.Gran == BySession {
		return BM25Adapter{V2: true}.rankSessions(hits, strings.Contains(q.Type, "knowledge-update"), k), nil
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

// bm25Scores is the v2 lexical scorer over raw texts, returned positionally.
func bm25Scores(texts []string, query, qtype string) []float64 {
	docToks := make([][]string, len(texts))
	df := map[string]int{}
	totalLen := 0
	for i, t := range texts {
		toks := tokenizeV2(t)
		docToks[i] = toks
		totalLen += len(toks)
		seen := map[string]bool{}
		for _, tok := range toks {
			if !seen[tok] {
				seen[tok] = true
				df[tok]++
			}
		}
	}
	n := len(texts)
	avgdl := 1.0
	if n > 0 && totalLen > 0 {
		avgdl = float64(totalLen) / float64(n)
	}
	qToks := map[string]bool{}
	for _, t := range append(tokenizeV2(query), expandForType(qtype)...) {
		qToks[t] = true
	}
	qBi := bigrams(tokenizeV2(query))

	out := make([]float64, n)
	for i := range texts {
		tf := map[string]int{}
		for _, tok := range docToks[i] {
			tf[tok]++
		}
		dl := float64(len(docToks[i]))
		bm := 0.0
		for term := range qToks {
			f := float64(tf[term])
			if f == 0 {
				continue
			}
			idf := math.Log(1 + (float64(n)-float64(df[term])+0.5)/(float64(df[term])+0.5))
			bm += idf * (f * (bm25K1 + 1)) / (f + bm25K1*(1-bm25B+bm25B*dl/avgdl))
		}
		if bm > 0 && len(qBi) > 0 {
			for bg := range bigrams(docToks[i]) {
				if qBi[bg] {
					bm += 1.0
				}
			}
		}
		out[i] = bm
	}
	return out
}
