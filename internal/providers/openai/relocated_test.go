package openai

// Tests relocated with the adapter extraction: the OpenAI/Grok halves of the
// native web-search suite and the Responses document-input mapping.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/memcode-ai/memcode/catalog"
	"github.com/memcode-ai/memcode/internal/wire"
)

func searchToolReq() wire.Request {
	return wire.Request{
		Model: "m",
		Tools: []wire.ToolDef{
			{Name: "read_file", InputSchema: map[string]any{"type": "object"}},
			{Name: "web_search", InputSchema: map[string]any{"type": "object"}},
			{Name: "bash", InputSchema: map[string]any{"type": "object"}},
		},
		Messages: []wire.Message{{Role: "user", Blocks: []wire.Block{wire.TextBlock("hi")}}},
	}
}

func TestOpenAINativeWebSearch(t *testing.T) {
	p := NewOpenAI("k").buildParams(searchToolReq(), 4096)
	var native, fnWebSearch bool
	fnCount := 0
	for _, tu := range p.Tools {
		if tu.OfWebSearch != nil {
			native = true
		}
		if tu.OfFunction != nil {
			fnCount++
			if tu.OfFunction.Name == "web_search" {
				fnWebSearch = true
			}
		}
	}
	if !native || fnWebSearch || fnCount != 2 {
		t.Fatalf("openai must swap the web_search def for the Responses built-in: native=%v fnDef=%v fns=%d", native, fnWebSearch, fnCount)
	}
}

func TestGrokNativeSearchInServingTurns(t *testing.T) {
	p := NewGrok("k").buildParams(searchToolReq(), 4096)
	var native, fnWebSearch bool
	for _, tu := range p.Tools {
		if tu.OfWebSearch != nil {
			native = true
		}
		if tu.OfFunction != nil && tu.OfFunction.Name == "web_search" {
			fnWebSearch = true
		}
	}
	if !native || fnWebSearch {
		t.Fatalf("grok must swap the web_search def for the Agent Tools built-in: native=%v fnDef=%v", native, fnWebSearch)
	}
}

func TestGrokAgentToolsSearch(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"resp_1","object":"response","status":"completed","model":"grok-4.6",
			"output":[{"type":"message","id":"m1","role":"assistant","status":"completed",
			"content":[{"type":"output_text","text":"answer with sources","annotations":[]}]}],
			"usage":{"input_tokens":10,"output_tokens":5}}`)
	}))
	defer srv.Close()

	g := NewGrok("k")
	g.baseURL = srv.URL
	text, usage, err := g.WebSearch(context.Background(), "what is memcode")
	if err != nil {
		t.Fatalf("WebSearch: %v", err)
	}
	if text != "answer with sources" {
		t.Fatalf("text = %q", text)
	}
	if usage.Backend != "grok" || usage.Model != catalog.ModelGrok46 || usage.InputTokens != 10 {
		t.Fatalf("usage = %+v", usage)
	}
	if gotPath != "/responses" {
		t.Fatalf("must call the responses endpoint, got %q", gotPath)
	}
	tools, _ := gotBody["tools"].([]any)
	var hasWebSearch bool
	for _, tl := range tools {
		if m, ok := tl.(map[string]any); ok && m["type"] == "web_search" {
			hasWebSearch = true
		}
	}
	if !hasWebSearch {
		t.Fatalf("request must declare the Agent Tools web_search built-in: %v", gotBody["tools"])
	}
}

func TestOpenAIDocumentBecomesInputFile(t *testing.T) {
	pdfBytes := []byte("%PDF-1.4 fake")
	docBlock := wire.DocumentBlock("application/pdf", pdfBytes)
	o := &OpenAI{}
	// Top-level document block → an input_file part in the user input_message.
	items := o.userItems([]wire.Block{wire.TextBlock("read this"), docBlock})
	var found bool
	for _, it := range items {
		if it.OfInputMessage == nil {
			continue
		}
		for _, c := range it.OfInputMessage.Content {
			if c.OfInputFile != nil && strings.HasPrefix(c.OfInputFile.FileData.Value, "data:application/pdf;base64,") {
				found = true
				if !strings.HasSuffix(c.OfInputFile.FileData.Value, base64.StdEncoding.EncodeToString(pdfBytes)) {
					t.Fatal("file data must carry the document bytes")
				}
			}
		}
	}
	if !found {
		t.Fatal("document block must become an input_file part")
	}

	// Inside a tool_result's ContentBlocks → hoisted as an input_file too.
	tr := wire.ToolResultBlocks("tu_1", []wire.Block{wire.TextBlock("read 3 pages"), docBlock}, false)
	items = o.userItems([]wire.Block{tr})
	found = false
	for _, it := range items {
		if it.OfInputMessage == nil {
			continue
		}
		for _, c := range it.OfInputMessage.Content {
			if c.OfInputFile != nil {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("a document inside a tool_result must ride as an input_file")
	}
}
