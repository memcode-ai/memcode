package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/agent/tools"
)

// githubTool is a TYPED surface over the `gh` CLI: PRs, issues, and CI status in
// structured actions, so the model doesn't hand-assemble shell strings. Read actions
// run directly; write actions (pr_create, comment) go through the permission gate — the
// same floor a `gh pr create` bash command would hit. Requires `gh` on PATH and auth'd.
func (s *Session) githubTool(ctx context.Context, input json.RawMessage) toolResult {
	var in tools.GitHubInput
	if err := json.Unmarshal(input, &in); err != nil {
		return errResult(err.Error())
	}
	switch in.Action {
	case "pr_view":
		if in.Number <= 0 {
			return errResult("pr_view needs a PR `number`.")
		}
		return s.ghRun(ctx, "PR #"+strconv.Itoa(in.Number),
			"pr", "view", strconv.Itoa(in.Number), "--json",
			"number,title,state,author,body,url,isDraft,reviewDecision,labels,comments")
	case "pr_list":
		return s.ghRun(ctx, "open PRs", "pr", "list", "--json", "number,title,state,author,isDraft,url", "--limit", "30")
	case "issue_view":
		if in.Number <= 0 {
			return errResult("issue_view needs an issue `number`.")
		}
		return s.ghRun(ctx, "issue #"+strconv.Itoa(in.Number),
			"issue", "view", strconv.Itoa(in.Number), "--json", "number,title,state,author,body,url,labels,comments")
	case "issue_list":
		return s.ghRun(ctx, "open issues", "issue", "list", "--json", "number,title,state,author,url,labels", "--limit", "30")
	case "checks":
		args := []string{"pr", "checks"}
		if in.Number > 0 {
			args = append(args, strconv.Itoa(in.Number))
		}
		args = append(args, "--json", "name,state,bucket,link")
		return s.ghRun(ctx, "CI checks", args...)

	case "pr_create":
		if strings.TrimSpace(in.Title) == "" || strings.TrimSpace(in.Body) == "" {
			return errResult("pr_create needs a `title` and `body`.")
		}
		if ok, reason := s.gate(ctx, permissions.Medium, false, ApprovalRequest{
			Title: in.Title, Label: "Open a GitHub PR", Detail: "gh pr create", Risk: permissions.Medium.String(),
		}); !ok {
			return errResult("pr_create denied: " + reason)
		}
		args := []string{"pr", "create", "--title", in.Title, "--body", in.Body}
		if in.Base != "" {
			args = append(args, "--base", in.Base)
		}
		return s.ghRun(ctx, "create PR", args...)
	case "comment":
		if in.Number <= 0 || strings.TrimSpace(in.Body) == "" {
			return errResult("comment needs a `number` and `body`.")
		}
		if ok, reason := s.gate(ctx, permissions.Medium, false, ApprovalRequest{
			Title: "#" + strconv.Itoa(in.Number), Label: "Comment on GitHub", Detail: "gh pr/issue comment", Risk: permissions.Medium.String(),
		}); !ok {
			return errResult("comment denied: " + reason)
		}
		// `gh pr comment` works for issues too (issues and PRs share the numbering).
		return s.ghRun(ctx, "comment on #"+strconv.Itoa(in.Number),
			"pr", "comment", strconv.Itoa(in.Number), "--body", in.Body)
	default:
		return errResult("github action must be one of: pr_view, pr_list, issue_view, issue_list, checks, pr_create, comment.")
	}
}

// ghRun invokes gh with the given args in the repo root, returns its output (redacted,
// truncated), and surfaces a tool line. A non-zero exit returns the error text.
func (s *Session) ghRun(ctx context.Context, label string, args ...string) toolResult {
	if _, err := exec.LookPath("gh"); err != nil {
		return errResult("the GitHub CLI `gh` isn't installed or on PATH — install it and run `gh auth login`.")
	}
	cctx, cancel := context.WithTimeout(ctx, bashTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "gh", args...)
	cmd.Dir = s.root
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		s.toolLine(true, "GitHub", label, "failed", true)
		return errResult(fmt.Sprintf("gh %s failed: %v\n%s", args[0], err, truncate(text, 2000)))
	}
	s.toolLine(true, "GitHub", label, "", false)
	return textResult(s.redactor.Redact(truncate(text, maxToolOutput)))
}
