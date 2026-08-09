package runtime

import (
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/memcode-ai/memcode/internal/agent/plan"
)

// The knowledge tool is offered to the executive (chat + plan) but NEVER to read-only explorers,
// and — unlike skill — it needs no discovery condition (the catalog is embedded, always present).
func TestKnowledgeToolOffered(t *testing.T) {
	s := &Session{planCtl: &plan.Controller{}}
	if !hasTool(s.toolDefs(), "knowledge") {
		t.Error("chat must offer the knowledge tool (embedded catalog, no discovery needed)")
	}
	s.readOnly = true
	if hasTool(s.toolDefs(), "knowledge") {
		t.Error("read-only explorers must NOT be offered the knowledge tool")
	}
}

// Consulting a pack is UNGATED reference — no approval function is consulted (this Session has
// none) and the full pack comes back, Facts and Idioms.
func TestKnowledgeConsultUngated(t *testing.T) {
	s := &Session{out: io.Discard} // no approve func — proves the path never gates
	tr := s.useKnowledge(json.RawMessage(`{"topic":"vercel"}`))
	if tr.isError {
		t.Fatalf("consult should succeed, got error: %s", resultText(tr))
	}
	if txt := resultText(tr); !strings.Contains(txt, "VERCEL_ENV") || !strings.Contains(txt, "Idioms") {
		t.Errorf("consult should return the full pack (Facts + Idioms), got:\n%s", txt)
	}
	// An unknown topic is a clean error listing what's available, not a crash.
	if e := s.useKnowledge(json.RawMessage(`{"topic":"cobol"}`)); !e.isError {
		t.Error("unknown pack should return an error result")
	}
}

func resultText(tr toolResult) string {
	var b strings.Builder
	for _, blk := range tr.blocks {
		b.WriteString(blk.Text)
	}
	return b.String()
}
