package vxui

import (
	"strings"

	"github.com/memcode-ai/memcode/internal/forks/vaxis/ui"

	"github.com/memcode-ai/memcode/internal/agent/runtime"
)

// askCard renders the agent's clarifying question (the ask_user gate): the question, then its
// 2-4 candidate answers as a numbered selectable list — or the user types a free-form answer.
// Same frame/primitives as the approval card.
func (s *appState) askCard() ui.Widget {
	q := s.askReq
	rows := []ui.Widget{
		ui.RichText{Spans: []ui.TextSpan{{Text: q.Question, Style: s.sty.emph}}, SoftWrap: true, MaxLines: 8},
	}
	if len(q.Options) > 0 {
		opts := make([]choice, 0, len(q.Options))
		for _, o := range q.Options {
			opts = append(opts, choice{label: o.Label, desc: strings.TrimSpace(o.Description)})
		}
		rows = append(rows, ui.SizedBox{Height: 1})
		rows = append(rows, s.optionList(opts, s.askChoice, true)...)
	}
	rows = append(rows,
		ui.SizedBox{Height: 1},
		s.hintRow("↑↓ select · Enter · or type your own answer · Esc to skip"),
	)
	return s.card(rows...)
}

// answerAsk sends the answer back to the blocked engine goroutine and clears the card. An empty
// answer = "skip" — the engine proceeds on best judgment and records it as an open question.
func (s *appState) answerAsk(answer string) {
	if s.askReply != nil {
		s.askReply <- runtime.AskResponse{Answer: answer}
	}
	s.SetState(func() {
		// Restore any ordinary draft that existed before this ask card. The
		// temporary answer buffer is discarded; it must never turn into a
		// separate submitted chat message after the user chooses an option.
		s.finishModalInput()
		s.askReq = nil
		s.askReply = nil
		s.askChoice = 0
		s.advanceHitl() // pop this card; show the next queued prompt (or resume the clock if none)
	})
}
