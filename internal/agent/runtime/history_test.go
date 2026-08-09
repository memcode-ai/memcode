package runtime

import (
	"context"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/store"
	"github.com/memcode-ai/memcode/internal/wire"
)

// capturingProvider records the messages it's handed each call and always replies
// with a tool-less text answer ("answer-N").
type capturingProvider struct{ calls [][]wire.Message }

func (p *capturingProvider) Complete(_ context.Context, r wire.Request) (wire.Response, error) {
	if r.Mode == "turn_intent" { // the routing judge is a side call — not part of the scripted turn
		return wire.Response{StopReason: "end_turn", Blocks: []wire.Block{{Type: "text", Text: "n/a"}}}, nil
	}
	p.calls = append(p.calls, r.Messages)
	return wire.Response{StopReason: "end_turn", Blocks: []wire.Block{
		{Type: "text", Text: "answer-" + strconv.Itoa(len(p.calls))}}}, nil
}

// A tool-less assistant answer must stay in the conversation history, or the next
// turn sees the prior question as unanswered and re-answers it (the bug: it
// re-described a pasted screenshot when later asked to "kill the shell").
func TestTextAnswerStaysInHistory(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	prov := &capturingProvider{}
	s := newSess(st, prov, root, "sonnet", permissions.ModeAsk, io.Discard)
	chat := s.StartChat(ctx)
	defer s.EndChat(ctx)

	s.Submit(ctx, chat, "what does this say?")
	s.Submit(ctx, chat, "now kill the shell")

	// Some Complete call must have received turn 1's assistant answer in its history.
	for _, msgs := range prov.calls {
		for _, m := range msgs {
			if m.Role != "assistant" {
				continue
			}
			for _, b := range m.Blocks {
				if strings.Contains(b.Text, "answer-1") {
					return // assistant answer carried forward — fixed
				}
			}
		}
	}
	t.Fatalf("turn 1's assistant answer never reached history (%d calls) — tool-less response dropped", len(prov.calls))
}
