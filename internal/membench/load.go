package membench

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	longMemEvalURL = "https://huggingface.co/datasets/xiaowu0162/longmemeval-cleaned/resolve/main/longmemeval_s_cleaned.json"
	locomoURL      = "https://raw.githubusercontent.com/snap-research/locomo/main/data/locomo10.json"
)

// CacheDir is where datasets and retrieval logs live; never committed.
func CacheDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "memcode-bench")
	}
	return filepath.Join(home, ".cache", "memcode-bench")
}

// Load fetches (or reads from cache / an explicit path) and parses a dataset.
// Supported names: "longmemeval-s", "locomo".
func Load(name, dataPath string) (*Dataset, error) {
	var url, file string
	switch name {
	case "longmemeval-s":
		url, file = longMemEvalURL, "longmemeval_s_cleaned.json"
	case "locomo":
		url, file = locomoURL, "locomo10.json"
	default:
		return nil, fmt.Errorf("unknown dataset %q (want longmemeval-s or locomo)", name)
	}

	path := dataPath
	if path == "" {
		var err error
		path, err = fetch(url, file)
		if err != nil {
			return nil, fmt.Errorf("download %s: %w (pass --data with a local copy)", name, err)
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	switch name {
	case "longmemeval-s":
		return parseLongMemEval(raw)
	default:
		return parseLoCoMo(raw)
	}
}

func fetch(url, file string) (string, error) {
	dir := CacheDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, file)
	if st, err := os.Stat(path); err == nil && st.Size() > 0 {
		return path, nil
	}
	resp, err := http.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	tmp := path + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	return path, os.Rename(tmp, path)
}

// ─── LongMemEval ────────────────────────────────────────────────────────────

type lmeInstance struct {
	QuestionID        string          `json:"question_id"`
	QuestionType      string          `json:"question_type"`
	Question          string          `json:"question"`
	QuestionDate      string          `json:"question_date"`
	HaystackDates     []string        `json:"haystack_dates"`
	HaystackSessionID []string        `json:"haystack_session_ids"`
	HaystackSessions  [][]lmeTurn     `json:"haystack_sessions"`
	AnswerSessionIDs  []string        `json:"answer_session_ids"`
	Answer            json.RawMessage `json:"answer"`
}

type lmeTurn struct {
	Role      string `json:"role"`
	Content   string `json:"content"`
	HasAnswer bool   `json:"has_answer"`
}

func parseLongMemEval(raw []byte) (*Dataset, error) {
	var instances []lmeInstance
	if err := json.Unmarshal(raw, &instances); err != nil {
		return nil, fmt.Errorf("longmemeval parse: %w", err)
	}
	ds := &Dataset{Name: "longmemeval-s", Gran: BySession}
	for _, in := range instances {
		q := Question{
			ID:     in.QuestionID,
			Type:   in.QuestionType,
			Text:   in.Question,
			Answer: rawToString(in.Answer),
			Date:   parseWhen(in.QuestionDate),
			Gold:   map[string]bool{},
		}
		// Abstention instances (question_id suffixed _abs) keep an empty Gold
		// and are skipped by the runner, matching the official retrieval eval.
		if !strings.HasSuffix(in.QuestionID, "_abs") {
			for _, id := range in.AnswerSessionIDs {
				q.Gold[id] = true
			}
		}
		for si, turns := range in.HaystackSessions {
			id := fmt.Sprintf("s%d", si)
			if si < len(in.HaystackSessionID) {
				id = in.HaystackSessionID[si]
			}
			doc := SessionDoc{ID: id}
			if si < len(in.HaystackDates) {
				doc.TS = parseWhen(in.HaystackDates[si])
			}
			for ti, t := range turns {
				doc.Turns = append(doc.Turns, Turn{
					Role:     t.Role,
					Text:     t.Content,
					ID:       fmt.Sprintf("%s#%d", id, ti),
					Evidence: t.HasAnswer,
				})
			}
			q.Haystack = append(q.Haystack, doc)
		}
		ds.Questions = append(ds.Questions, q)
	}
	return ds, nil
}

// ─── LoCoMo ─────────────────────────────────────────────────────────────────

type locomoSample struct {
	SampleID     string                     `json:"sample_id"`
	Conversation map[string]json.RawMessage `json:"conversation"`
	QA           []locomoQA                 `json:"qa"`
}

type locomoQA struct {
	Question string          `json:"question"`
	Answer   json.RawMessage `json:"answer"`
	Category json.Number     `json:"category"`
	Evidence []string        `json:"evidence"`
}

type locomoTurn struct {
	Speaker string `json:"speaker"`
	DiaID   string `json:"dia_id"`
	Text    string `json:"text"`
	Caption string `json:"blip_caption"`
}

func parseLoCoMo(raw []byte) (*Dataset, error) {
	var samples []locomoSample
	if err := json.Unmarshal(raw, &samples); err != nil {
		return nil, fmt.Errorf("locomo parse: %w", err)
	}
	ds := &Dataset{Name: "locomo", Gran: ByTurn}
	for _, s := range samples {
		var speakerA, speakerB string
		if v, ok := s.Conversation["speaker_a"]; ok {
			json.Unmarshal(v, &speakerA)
		}
		if v, ok := s.Conversation["speaker_b"]; ok {
			json.Unmarshal(v, &speakerB)
		}

		// Collect session_<n> keys in chronological (numeric) order.
		type sessKey struct {
			num int
			key string
		}
		var keys []sessKey
		for k := range s.Conversation {
			var n int
			if _, err := fmt.Sscanf(k, "session_%d", &n); err == nil && !strings.Contains(k, "date_time") &&
				!strings.Contains(k, "observation") && !strings.Contains(k, "summary") {
				keys = append(keys, sessKey{n, k})
			}
		}
		sort.Slice(keys, func(i, j int) bool { return keys[i].num < keys[j].num })

		var haystack []SessionDoc
		for _, sk := range keys {
			var turns []locomoTurn
			if err := json.Unmarshal(s.Conversation[sk.key], &turns); err != nil {
				continue
			}
			doc := SessionDoc{ID: fmt.Sprintf("%s-%s", s.SampleID, sk.key)}
			if v, ok := s.Conversation[sk.key+"_date_time"]; ok {
				var dt string
				json.Unmarshal(v, &dt)
				doc.TS = parseWhen(dt)
			}
			for _, t := range turns {
				text := t.Text
				if t.Caption != "" {
					text += " [shared an image: " + t.Caption + "]"
				}
				// Speaker names ride the text: LoCoMo questions reference
				// speakers by name and the session log has no speaker field.
				role := "user"
				if t.Speaker == speakerB {
					role = "assistant"
				}
				doc.Turns = append(doc.Turns, Turn{
					Role: role,
					Text: t.Speaker + ": " + text,
					ID:   t.DiaID,
				})
			}
			haystack = append(haystack, doc)
		}

		for qi, qa := range s.QA {
			q := Question{
				ID:       fmt.Sprintf("%s-q%d", s.SampleID, qi),
				Type:     "category-" + qa.Category.String(),
				Text:     qa.Question,
				Answer:   rawToString(qa.Answer),
				Gold:     map[string]bool{},
				Haystack: haystack,
			}
			// Category 5 is adversarial/unanswerable; evidence stays empty and
			// the runner skips it, same as LongMemEval abstention.
			if qa.Category.String() != "5" {
				for _, e := range qa.Evidence {
					q.Gold[e] = true
				}
			}
			ds.Questions = append(ds.Questions, q)
		}
	}
	return ds, nil
}

// rawToString renders a JSON answer value (string, number, or list) as
// plain text for the QA judge.
func rawToString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return strings.Trim(string(raw), `"`)
}

// parseWhen tries the timestamp layouts the two datasets use. Zero time on
// failure; adapters treat zero as "no timestamp".
func parseWhen(s string) time.Time {
	s = strings.TrimSpace(s)
	layouts := []string{
		"2006/01/02 (Mon) 15:04",     // LongMemEval haystack/question dates
		"2006/01/02 15:04",           //
		"3:04 pm on 2 January, 2006", // LoCoMo session_date_time
		"3:04pm on 2 January, 2006",
		"2 January, 2006",
		time.RFC3339,
	}
	for _, l := range layouts {
		if t, err := time.Parse(l, s); err == nil {
			return t
		}
	}
	return time.Time{}
}
