package membench

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// Phase C: answer-mode QA. Retrieval-trimmed context goes to an answering
// model, a judge model grades the answer against the benchmark's gold
// answer (the LongMemEval methodology; use the paper's judge model when the
// number must be comparable to published results). Config by env:
//
//	MEMBENCH_QA_PROVIDER  anthropic | openai        (default anthropic)
//	MEMBENCH_QA_BASE      API base URL              (provider default)
//	MEMBENCH_QA_KEY       falls back to ANTHROPIC_API_KEY / OPENAI_API_KEY
//	MEMBENCH_QA_MODEL     answering model           (default claude-haiku-4-5)
//	MEMBENCH_JUDGE_*      same shape; defaults to the QA config
type chatConfig struct {
	provider, base, key, model string
	maxTok                     int // overrides MEMBENCH_QA_MAXTOK / the 512 default when set
}

func chatConfigFromEnv(prefix string) (chatConfig, error) {
	cfg := chatConfig{
		provider: envOr(prefix+"_PROVIDER", "anthropic"),
		base:     os.Getenv(prefix + "_BASE"),
		key:      os.Getenv(prefix + "_KEY"),
		model:    envOr(prefix+"_MODEL", "claude-haiku-4-5"),
	}
	if cfg.base == "" {
		if cfg.provider == "anthropic" {
			cfg.base = "https://api.anthropic.com"
		} else {
			cfg.base = "https://api.openai.com/v1"
		}
	}
	if cfg.key == "" {
		if cfg.provider == "anthropic" {
			cfg.key = os.Getenv("ANTHROPIC_API_KEY")
		} else {
			cfg.key = os.Getenv("OPENAI_API_KEY")
		}
	}
	if cfg.key == "" {
		return cfg, fmt.Errorf("no %s key: set %s_KEY (or the provider's standard env var)", prefix, prefix)
	}
	return cfg, nil
}

func (c chatConfig) complete(system, user string) (string, error) {
	client := &http.Client{Timeout: 180 * time.Second}
	if c.provider == "anthropic" {
		maxTok := 512
		if v := os.Getenv("MEMBENCH_QA_MAXTOK"); v != "" {
			fmt.Sscanf(v, "%d", &maxTok)
		}
		if c.maxTok > 0 {
			maxTok = c.maxTok
		}
		body, _ := json.Marshal(map[string]any{
			"model":      c.model,
			"max_tokens": maxTok,
			"system":     system,
			"messages":   []map[string]string{{"role": "user", "content": user}},
		})
		req, err := http.NewRequest("POST", strings.TrimRight(c.base, "/")+"/v1/messages", bytes.NewReader(body))
		if err != nil {
			return "", err
		}
		req.Header.Set("x-api-key", c.key)
		req.Header.Set("anthropic-version", "2023-06-01")
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		var parsed struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
			return "", err
		}
		if parsed.Error != nil {
			return "", fmt.Errorf("anthropic API: %s", parsed.Error.Message)
		}
		var out strings.Builder
		for _, b := range parsed.Content {
			if b.Type == "text" {
				out.WriteString(b.Text)
			}
		}
		return out.String(), nil
	}

	body, _ := json.Marshal(map[string]any{
		"model": c.model,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
	})
	req, err := http.NewRequest("POST", strings.TrimRight(c.base, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return "", err
	}
	if parsed.Error != nil {
		return "", fmt.Errorf("chat API: %s", parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 {
		return "", fmt.Errorf("chat API returned no choices")
	}
	return parsed.Choices[0].Message.Content, nil
}

// QAResult aggregates answer-mode accuracy.
type QAResult struct {
	Dataset  string
	Adapter  string
	Model    string
	Total    int
	Correct  int
	ByType   map[string][2]int // type -> {correct, total}
	Failures []string          // question ids judged incorrect (capped)
}

func (r *QAResult) Accuracy() float64 {
	if r.Total == 0 {
		return 0
	}
	return float64(r.Correct) / float64(r.Total)
}

func (r *QAResult) Render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s · QA · retriever=%s · model=%s\n", r.Dataset, r.Adapter, r.Model)
	fmt.Fprintf(&b, "  accuracy %.3f (%d/%d)\n", r.Accuracy(), r.Correct, r.Total)
	types := make([]string, 0, len(r.ByType))
	for t := range r.ByType {
		types = append(types, t)
	}
	sort.Strings(types)
	for _, t := range types {
		ct := r.ByType[t]
		fmt.Fprintf(&b, "  %-28s %.3f (%d/%d)\n", t, float64(ct[0])/float64(max(1, ct[1])), ct[0], ct[1])
	}
	return b.String()
}

const answerSystem = `You answer questions about a user's past chat sessions. You are given excerpts from those sessions, each with its date.

Work in two steps. First, under "Evidence:", list each relevant fact you find with its session date, one per line. Second, under "Answer:", give the final answer in one short sentence.

Rules: use ONLY the excerpts. When the question involves time or counting, reason from the session dates explicitly (compute day/week/month differences rather than guessing). When information changed across sessions, the most recent statement wins. If the excerpts do not contain the answer, write under "Answer:" exactly: The information is not available.`

const judgeSystem = `You are grading answers against a gold reference. Reply with exactly one word: yes if the proposed answer is semantically correct given the reference (paraphrase and formatting differences are fine), no otherwise. For reference answers meaning the information is unavailable, an abstaining response counts as correct.`

// RunQA answers questions with retrieval-trimmed context and judges them.
// topK is how many retrieved sessions ride the prompt.
func RunQA(ds *Dataset, ad Adapter, workDir string, limit, topK int) (*QAResult, error) {
	answer, err := chatConfigFromEnv("MEMBENCH_QA")
	if err != nil {
		return nil, err
	}
	judge, jerr := chatConfigFromEnv("MEMBENCH_JUDGE")
	if jerr != nil {
		judge = answer
	}
	if topK <= 0 {
		topK = 6
	}

	qs := ds.Questions
	if limit > 0 && limit < len(qs) {
		qs = qs[:limit]
	}
	res := &QAResult{Dataset: ds.Name, Adapter: ad.Name(), Model: answer.model, ByType: map[string][2]int{}}
	prepareAdapter(ad)

	type verdict struct {
		qid, qtype string
		ok, ran    bool
		err        error
	}
	outs := make([]verdict, len(qs))
	var wg sync.WaitGroup
	sem := make(chan struct{}, qaWorkers)
	for i := range qs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			q := qs[i]
			if q.Answer == "" {
				return
			}
			q.Gran = ds.Gran
			root, err := os.MkdirTemp(workDir, "membench-qa-*")
			if err != nil {
				outs[i] = verdict{err: err}
				return
			}
			ingestErr := Ingest(root, q.Haystack)
			var ranked []string
			if ingestErr == nil {
				ranked, ingestErr = ad.Rank(root, q, q.Haystack, topK*8)
			}
			os.RemoveAll(root)
			if ingestErr != nil {
				outs[i] = verdict{err: fmt.Errorf("%s: %w", q.ID, ingestErr)}
				return
			}
			ctx := buildContext(q, ranked, topK)

			user := fmt.Sprintf("Session excerpts:\n\n%s\n\nQuestion (asked %s): %s",
				ctx, q.Date.Format("2006-01-02"), q.Text)
			hyp, err := answer.complete(answerSystem, user)
			if err != nil {
				outs[i] = verdict{err: fmt.Errorf("answer %s: %w", q.ID, err)}
				return
			}
			// Judge only the final answer, not the evidence scratchpad.
			final := strings.TrimSpace(hyp)
			if i := strings.LastIndex(final, "Answer:"); i >= 0 {
				final = strings.TrimSpace(final[i+len("Answer:"):])
			}
			verdictPrompt := fmt.Sprintf("Question: %s\nReference answer: %s\nProposed answer: %s\nCorrect?",
				q.Text, q.Answer, final)
			v, err := judge.complete(judgeSystem, verdictPrompt)
			if err != nil {
				outs[i] = verdict{err: fmt.Errorf("judge %s: %w", q.ID, err)}
				return
			}
			outs[i] = verdict{
				qid: q.ID, qtype: q.Type, ran: true,
				ok: strings.HasPrefix(strings.ToLower(strings.TrimSpace(v)), "yes"),
			}
		}(i)
	}
	wg.Wait()

	for _, o := range outs {
		if o.err != nil {
			return nil, o.err
		}
		if !o.ran {
			continue
		}
		res.Total++
		ct := res.ByType[o.qtype]
		ct[1]++
		if o.ok {
			res.Correct++
			ct[0]++
		} else if len(res.Failures) < 50 {
			res.Failures = append(res.Failures, o.qid)
		}
		res.ByType[o.qtype] = ct
	}
	return res, nil
}

// qaWorkers bounds concurrent answer+judge calls; modest so provider rate
// limits stay comfortable.
const qaWorkers = 6

// buildContext renders the top retrieved sessions (or the sessions owning
// the top retrieved turns) with dates, oldest first so the narrative reads
// forward and "latest wins" is visible to the model.
func buildContext(q Question, ranked []string, topK int) string {
	docByID := map[string]*SessionDoc{}
	for i := range q.Haystack {
		docByID[q.Haystack[i].ID] = &q.Haystack[i]
	}
	sessOf := func(unit string) string {
		if i := strings.LastIndex(unit, "#"); i > 0 {
			return unit[:i]
		}
		// LoCoMo dia ids ("D1:3") do not embed the session id: find it.
		for _, d := range q.Haystack {
			for _, t := range d.Turns {
				if t.ID == unit {
					return d.ID
				}
			}
		}
		return ""
	}

	var picked []*SessionDoc
	seen := map[string]bool{}
	for _, unit := range ranked {
		sid := sessOf(unit)
		if sid == "" || seen[sid] {
			continue
		}
		seen[sid] = true
		if d := docByID[sid]; d != nil {
			picked = append(picked, d)
		}
		if len(picked) >= topK {
			break
		}
	}
	sort.SliceStable(picked, func(i, j int) bool { return picked[i].TS.Before(picked[j].TS) })

	var b strings.Builder
	for _, d := range picked {
		date := "unknown date"
		if !d.TS.IsZero() {
			date = d.TS.Format("2006-01-02 15:04")
		}
		fmt.Fprintf(&b, "--- Session on %s ---\n", date)
		for _, t := range d.Turns {
			fmt.Fprintf(&b, "%s: %s\n", strings.ToUpper(t.Role), t.Text)
		}
		b.WriteString("\n")
	}
	return b.String()
}
