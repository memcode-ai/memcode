package runtime

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/memcode-ai/memcode/internal/agent/tools"
	"github.com/memcode-ai/memcode/internal/events"
)

// AskOption is one candidate answer — a concise Label plus an optional muted
// Description line. Aliased to the tool-input type so there's ONE shape from the
// model's JSON through to the rendered card.
type AskOption = tools.AskOption

// AskRequest is a clarifying question the agent poses to the user (HITL).
type AskRequest struct {
	Question string
	Options  []AskOption // 2-4 choices (label + optional description); the user may also type their own
}

// AskResponse is the user's answer (a chosen option or free text); empty means
// the user dismissed it (the agent should then proceed on its best judgment).
type AskResponse struct{ Answer string }

// askUserTool poses a question to the user and returns their answer as the tool
// result, so the agent can continue with the decision resolved instead of
// guessing or deferring it to an open question. It blocks the turn until the
// user answers (like the approval gate). Not available to read-only explorers.
func (s *Session) askUserTool(ctx context.Context, input json.RawMessage) toolResult {
	var in tools.AskUserInput
	if err := json.Unmarshal(input, &in); err != nil {
		return errResult(err.Error())
	}
	q := strings.TrimSpace(in.Question)
	if q == "" {
		return errResult("ask_user needs a `question`.")
	}
	// The card shows the question live; record the resolved Q→A in scrollback.
	resp := s.ask(ctx, AskRequest{Question: q, Options: pruneEscapeOptions(in.Options)})
	ans := strings.TrimSpace(resp.Answer)
	// Flush any unterminated streamed prose (the model often narrates the question right
	// before calling ask_user) so the Q→A record below BEGINS on its own line and the
	// question can't glue onto the "⏺ AskUser(…)" marker (the question-bleed). A bare
	// newline is a no-op when scrollback is already at a line boundary.
	s.printf("\n")
	if ans == "" {
		s.toolLine(true, "AskUser", q, "skipped", false)
		return textResult("(the user didn't answer — proceed with your best judgment, and note this as an open question in the plan)")
	}
	s.emit(ctx, events.KindUserNote, map[string]any{"question": q, "answer": ans})
	s.toolLine(true, "AskUser", q, "", false)
	s.printf("%s\n", metaStyle.Render("  ⎿ "+clip(ans, 2000))) // the user's own answer — echo it generously, not a 1-line teaser (the model gets it in full regardless)
	return textResult("The user answered: " + ans)
}

// pruneEscapeOptions removes a generic "Something else / Other / None of these"
// freeform-escape option the model sometimes appends — it's redundant with the
// always-present "type your own answer" input, and shows as a dead menu item.
func pruneEscapeOptions(opts []AskOption) []AskOption {
	kept := make([]AskOption, 0, len(opts))
	for _, o := range opts {
		l := strings.ToLower(strings.TrimSpace(o.Label))
		if l == "other" ||
			strings.HasPrefix(l, "something else") ||
			strings.HasPrefix(l, "none of the") ||
			strings.HasPrefix(l, "other (") ||
			strings.HasPrefix(l, "let me ") ||
			strings.Contains(l, "(describe)") ||
			strings.Contains(l, "(specify)") ||
			strings.Contains(l, "type your own") ||
			strings.Contains(l, "type my own") ||
			strings.Contains(l, "write my own") {
			continue
		}
		kept = append(kept, o)
	}
	return kept
}

// stdinAsker is the default line-REPL question handler: prints the question and
// numbered options, accepts a number (pick an option) or free text.
func stdinAsker(out io.Writer) func(context.Context, AskRequest) AskResponse {
	reader := bufio.NewReader(os.Stdin)
	return func(_ context.Context, req AskRequest) AskResponse {
		fmt.Fprintf(out, "\n%s\n", req.Question)
		for i, o := range req.Options {
			fmt.Fprintf(out, "  %d. %s\n", i+1, o.Label)
			if d := strings.TrimSpace(o.Description); d != "" {
				fmt.Fprintf(out, "     %s\n", d)
			}
		}
		fmt.Fprintf(out, "your answer (number or text): ")
		line, err := reader.ReadString('\n')
		if err != nil {
			return AskResponse{}
		}
		t := strings.TrimSpace(line)
		if n, e := strconv.Atoi(t); e == nil && n >= 1 && n <= len(req.Options) {
			return AskResponse{Answer: req.Options[n-1].Label}
		}
		return AskResponse{Answer: t}
	}
}
