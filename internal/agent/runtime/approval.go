package runtime

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
)

// ApprovalRequest is a structured description of an action awaiting the user's
// OK. The metadata (Title/Label/Detail) lets a front-end render a rich prompt;
// Command + Editable enable "allow, but run this instead". This replaces the old
// bare prompt string — a boolean approver is too crude for a serious agent.
type ApprovalRequest struct {
	Title    string // the thing being approved — the command, or "edit <path>"
	Label    string // category header, e.g. "Bash command" / "Edit file"
	Detail   string // optional one-line description under the title
	Command  string // the command, when this is a command (so it can be edited/remembered)
	Cwd      string // working directory for a command (for the "don't ask again … in <dir>" scope)
	Editable bool   // whether "allow with edited command" / "don't ask again" apply (commands)
	Risk     string // risk class label
	// RememberScopes replaces the single default don't-ask-again option with one card
	// option per scope (MCP: remember this tool / remember the whole server). When set,
	// the card reads Execute / <scopes…> / Cancel and Cancel is a plain deny.
	RememberScopes []ApprovalScope
}

// ApprovalScope is one scoped "and don't ask again" option a request offers.
type ApprovalScope struct {
	Key   string // returned in ApprovalDecision.RememberScope when chosen
	Label string // full option text on the card
}

// ApprovalDecision is the user's structured answer. It generalizes yes/no into
// the four outcomes a real agent needs: allow · allow-with-edited-input ·
// deny-with-reason · interrupt (stop going down this path entirely).
type ApprovalDecision struct {
	Allow         bool   // run it
	Remember      bool   // and don't ask again for like commands (persist an approval rule)
	RememberScope string // when Allow: the chosen ApprovalScope.Key ("" = none / plain yes)
	Command       string // when Allow and non-empty: run THIS instead of the original
	Reason        string // when !Allow: why — fed back to the model so it can adjust
	Interrupt     bool   // STOP the whole turn (Esc / "No, stop") — the model does not get another call
	Redirect      bool   // when !Allow with a typed Reason: deny this action and skip its siblings, but let the turn CONTINUE so the model reads the feedback and responds (does NOT terminate)
}

// Allowed is a plain yes.
func Allowed() ApprovalDecision { return ApprovalDecision{Allow: true} }

// Denied is a no, optionally with a reason the model will see.
func Denied(reason string) ApprovalDecision { return ApprovalDecision{Reason: reason} }

// stdinApprover is the default line-REPL approver. It understands:
//
//	y / yes / <enter>   → allow
//	n / no              → deny
//	s / stop            → interrupt (stop this turn)
//	! <command>         → allow, but run <command> instead (editable actions)
//	<anything else>     → deny, using the text as the reason fed back to the model
func stdinApprover(out io.Writer) func(context.Context, ApprovalRequest) ApprovalDecision {
	reader := bufio.NewReader(os.Stdin)
	return func(_ context.Context, req ApprovalRequest) ApprovalDecision {
		hint := "[y=yes"
		if req.Editable {
			hint += ", a=always, !<cmd>=run instead"
		}
		for i, sc := range req.RememberScopes {
			hint += fmt.Sprintf(", %d=%s", i+1, sc.Label)
		}
		hint += ", s=stop, or type what to do differently]"
		fmt.Fprintf(out, "%s (%s) %s: ", req.Title, req.Risk, hint)
		line, err := reader.ReadString('\n')
		if err != nil {
			return ApprovalDecision{} // non-interactive / EOF → deny
		}
		text := strings.TrimSpace(line)
		switch strings.ToLower(text) {
		case "y", "yes":
			return Allowed()
		case "a", "always":
			if req.Editable {
				return ApprovalDecision{Allow: true, Remember: true}
			}
			return Allowed()
		case "", "n", "no":
			return Denied("")
		case "s", "stop":
			return ApprovalDecision{Interrupt: true, Reason: "user stopped this turn"}
		}
		if n := len(req.RememberScopes); n > 0 && len(text) == 1 && text[0] >= '1' && int(text[0]-'0') <= n {
			return ApprovalDecision{Allow: true, RememberScope: req.RememberScopes[text[0]-'1'].Key}
		}
		if req.Editable && strings.HasPrefix(text, "!") {
			return ApprovalDecision{Allow: true, Command: strings.TrimSpace(text[1:])}
		}
		return Denied(text) // free text = deny + tell the agent what to do differently
	}
}
