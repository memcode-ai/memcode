package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/store"
	"github.com/memcode-ai/memcode/internal/wire"
)

// TestOffloadResearchForSynthesis: before plan synthesis, stale research tool-output is offloaded to
// re-fetchable pointers (keeping the most recent budget-scaled keep-count raw), while the assistant PROSE
// findings — what the plan is actually built from — are left untouched.
func TestOffloadResearchForSynthesis(t *testing.T) {
	t.Setenv("MEMCODE_COMPACT_BUDGET", "45000") // pin the budget so the keep-count (floor 8) matches the fixture
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := newSess(st, captureProviderNil{}, t.TempDir(), "sonnet", permissions.ModeAsk, io.Discard)

	keep := keepToolResults(45_000)
	const rounds = 12 // > keep
	var msgs []wire.Message
	msgs = append(msgs, wire.Message{Role: "user", Blocks: []wire.Block{{Type: "text", Text: "plan it"}}})
	for i := 0; i < rounds; i++ {
		id := fmt.Sprintf("r%d", i)
		msgs = append(msgs,
			wire.Message{Role: "assistant", Blocks: []wire.Block{{Type: "tool_use", ID: id, Name: "read_file", Input: json.RawMessage(fmt.Sprintf(`{"path":"f%d.go"}`, i))}}},
			wire.Message{Role: "user", Blocks: []wire.Block{{Type: "tool_result", ToolUseID: id, Content: "BIG DUMP " + strings.Repeat("x", 1000)}}},
			wire.Message{Role: "assistant", Blocks: []wire.Block{{Type: "text", Text: "finding " + id}}},
		)
	}

	s.offloadResearchForSynthesis(&msgs)

	raw, offloaded := 0, 0
	for _, m := range msgs {
		for _, b := range m.Blocks {
			if b.Type == "tool_result" {
				if strings.Contains(b.Content, "offloaded") {
					offloaded++
				} else {
					raw++
				}
			}
		}
	}
	if raw != keep {
		t.Fatalf("should keep the most recent %d research outputs raw, got %d", keep, raw)
	}
	if offloaded != rounds-keep {
		t.Fatalf("should offload the older %d, got %d", rounds-keep, offloaded)
	}
	// The prose findings (what synthesis reasons over) must survive verbatim.
	var prose int
	for _, m := range msgs {
		for _, b := range m.Blocks {
			if b.Type == "text" && strings.HasPrefix(b.Text, "finding ") {
				prose++
			}
		}
	}
	if prose != rounds {
		t.Fatalf("all %d prose findings must survive untouched, got %d", rounds, prose)
	}
	// An offloaded read points back to its file (lossless — re-readable).
	for _, m := range msgs {
		for _, b := range m.Blocks {
			if b.Type == "tool_result" && strings.Contains(b.Content, "offloaded") && !strings.Contains(b.Content, "re-read") {
				t.Fatalf("offloaded read should say how to restore it: %q", b.Content)
			}
		}
	}
}
