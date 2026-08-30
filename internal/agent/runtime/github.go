package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

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
		if msg := s.prCreatePreflight(ctx, in.Base); msg != "" {
			s.toolLine(true, "GitHub", "create PR", "blocked: "+clip(msg, 160), true)
			return errResult(msg)
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

// prCreatePreflight keeps PR creation at the end of the work: a PR without
// committed, pushed branch work is at best a noisy gh prompt and at worst a
// false "done" marker. Return "" when ready, else a user/model-facing reason.
func (s *Session) prCreatePreflight(ctx context.Context, base string) string {
	run := func(args ...string) (string, error) {
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(cctx, "git", args...)
		cmd.Dir = s.root
		out, err := cmd.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	if out, err := run("status", "--porcelain"); err != nil {
		return "pr_create preflight failed: git status failed: " + firstLine(out)
	} else if strings.TrimSpace(out) != "" {
		return "pr_create is a final step: the worktree still has uncommitted changes. Commit the branch work and push it before creating the PR."
	}
	branch, err := run("branch", "--show-current")
	if err != nil || branch == "" {
		return "pr_create needs a named feature branch; commit on a branch before creating the PR."
	}
	if branch == "main" || branch == "master" {
		return "pr_create needs a feature branch, not " + branch + ". Create and push a branch before opening the PR."
	}
	if strings.TrimSpace(base) == "" {
		base = "origin/main"
	} else if !strings.Contains(base, "/") {
		base = "origin/" + strings.TrimSpace(base)
	}
	ahead, err := run("rev-list", "--count", base+"..HEAD")
	if err != nil {
		return "pr_create preflight failed: cannot compare against " + base + " — fetch the base branch or pass a valid base."
	}
	if ahead == "0" {
		return "pr_create has nothing to open: this branch has no commits ahead of " + base + ". Commit the completed work first."
	}
	upstream, err := run("rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}")
	if err != nil || upstream == "" {
		return "pr_create needs the current branch pushed upstream. Run git push -u origin " + branch + " after committing, then create the PR."
	}
	unpushed, err := run("rev-list", "--count", upstream+"..HEAD")
	if err != nil {
		return "pr_create preflight failed: cannot compare against upstream " + upstream + ". Push the branch again, then create the PR."
	}
	if unpushed != "0" {
		return "pr_create needs the latest commits pushed first (" + unpushed + " local commit(s) are not on " + upstream + ")."
	}
	return ""
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
		reason := strings.TrimSpace(firstLine(text))
		if reason == "" {
			reason = err.Error()
		}
		s.toolLine(true, "GitHub", label, "failed: "+clip(s.redactor.Redact(reason), 160), true)
		return errResult(fmt.Sprintf("gh %s failed: %v\n%s", args[0], err, truncate(text, 2000)))
	}
	s.toolLine(true, "GitHub", label, "", false)
	return textResult(s.redactor.Redact(truncate(text, maxToolOutput)))
}
