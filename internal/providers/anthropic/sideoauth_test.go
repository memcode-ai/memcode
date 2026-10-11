package anthropic

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Web search/fetch on a Claude subscription token must carry the OAuth shape a
// turn does: system[0] exactly the Claude Code identity, and the OAuth betas
// kept alongside web-fetch's own (setting the header instead of adding it
// dropped them).
func TestSideChannelsCarryTheOAuthShape(t *testing.T) {
	var systems [][]string
	var betas []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			System []struct{ Text string } `json:"system"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		var texts []string
		for _, b := range body.System {
			texts = append(texts, b.Text)
		}
		systems = append(systems, texts)
		betas = append(betas, strings.Join(r.Header.Values("anthropic-beta"), ","))
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"id":"m","type":"message","role":"assistant","model":"x","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer srv.Close()

	c := NewAnthropic("sk-ant-oat-tok")
	c.baseURL = srv.URL
	if _, _, err := c.WebSearch(context.Background(), "q"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.WebFetch(context.Background(), "https://example.com"); err != nil {
		t.Fatal(err)
	}
	for i, sys := range systems {
		if len(sys) == 0 || sys[0] != claudeCodeSystemPrefix {
			t.Errorf("call %d: system[0] must be the identity block, got %q", i, sys)
		}
		for _, b := range oauthOnlyBetas {
			if !strings.Contains(betas[i], b) {
				t.Errorf("call %d: beta %q missing from %q", i, b, betas[i])
			}
		}
	}
	if !strings.Contains(betas[1], "web-fetch-2025-09-10") {
		t.Errorf("web fetch lost its own beta: %q", betas[1])
	}
}
