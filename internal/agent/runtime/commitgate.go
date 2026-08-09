package runtime

import (
	"context"
	"os/exec"
	"strings"

	"github.com/memcode-ai/memcode/internal/config"
	"github.com/memcode-ai/memcode/internal/events"
)

// commitGateOK is the "commit before a large work block?" check, run at the
// plan→apply boundary (the reliable signal that a big multi-file change is
// about to start). When the tree is dirty it offers to checkpoint first, so
// the plan's edits land on a clean base — a diff you can review or revert on
// its own, not tangled with in-flight work.
//
// EVERY option proceeds with the apply — this gate never stops the work:
//   - "Commit first, then continue": the agent commits the CURRENT tree as a
//     checkpoint commit (the user just sanctioned staging everything — this is
//     their explicit instruction, not agent freelancing), then the apply runs
//     on the clean base.
//   - "Continue without committing": run now, edits mix with the dirty tree.
//   - "Continue, and don't ask again": proceed + persist skip.
//
// A dismissed/auto-resolved answer (yolo auto-execute resolves HITL questions)
// proceeds WITHOUT committing — blocking an unattended flow on a question
// nobody will answer is worse than a mixed diff. Skipped entirely when:
// headless (no ask channel), yolo planning, not a git repo, clean tree, or a
// remembered skip.
func (s *Session) commitGateOK(ctx context.Context) bool {
	if s.planCtl.ConsumeCommitGate() { // the plan selector already resolved this (commit or not) — one-shot
		return true
	}
	if s.ask == nil || s.planCtl.Yolo() { // headless / yolo: never interpose a question
		return true
	}
	if !s.rootIsGit() {
		return true
	}
	cfg, err := config.Load(s.root)
	if s.GitStat(ctx).Clean() {
		return true
	}
	if err == nil {
		switch cfg.CommitBeforeWork {
		case "skip": // remembered: never ask, just proceed on the dirty tree
			return true
		case "commit": // remembered: always checkpoint-commit, silently, then proceed
			s.checkpointCommit(ctx)
			return true
		}
	}

	const (
		optCommit = "Commit first, then continue"
		optAlways = "Always commit first (don't ask again)"
		optGo     = "Continue without committing"
		optNever  = "Never ask — just continue"
	)
	resp := s.ask(ctx, AskRequest{
		Question: "You have uncommitted changes. Commit them as a checkpoint before I start this plan? " +
			"(keeps the plan's edits as a separate, reviewable diff)",
		Options: []AskOption{
			{Label: optCommit, Description: "I commit everything currently in the tree as a checkpoint, then execute the plan."},
			{Label: optAlways, Description: "Same as above, every time — checkpoint silently before large work (saved to .memcode/config.json)."},
			{Label: optGo, Description: "Start now; the plan's edits mix with your current changes."},
			{Label: optNever, Description: "Proceed now and never ask this again (saved to .memcode/config.json)."},
		},
	})

	switch resp.Answer {
	case optCommit:
		s.checkpointCommit(ctx)
	case optAlways:
		if cfg != nil {
			cfg.CommitBeforeWork = "commit"
			_ = cfg.Save()
		}
		s.checkpointCommit(ctx)
	case optNever:
		if cfg != nil {
			cfg.CommitBeforeWork = "skip"
			_ = cfg.Save()
		}
	}
	return true // the gate informs; it never halts the work
}

// CommitGateChoice is the TUI's entry point when the PLAN SELECTOR carries the
// commit decision ("Commit first, then execute" / "Execute without committing")
// instead of a second card: it performs the commit when asked and marks the
// gate resolved so the apply turn doesn't re-ask.
func (s *Session) CommitGateChoice(ctx context.Context, commitFirst bool) {
	if commitFirst {
		s.checkpointCommit(ctx)
	}
	s.planCtl.ResolveCommitGate()
}

// CommitGateNeeded reports whether the plan selector should offer the commit
// choice: interactive, a git repo, a dirty tree, and no remembered preference
// (a remembered "commit"/"skip" is honored silently by commitGateOK).
func (s *Session) CommitGateNeeded(ctx context.Context) bool {
	if s.planCtl.Yolo() || !s.rootIsGit() {
		return false
	}
	if cfg, err := config.Load(s.root); err == nil &&
		(cfg.CommitBeforeWork == "skip" || cfg.CommitBeforeWork == "commit") {
		return false
	}
	return !s.GitStat(ctx).Clean()
}

// checkpointCommit commits the ENTIRE dirty tree as a user-sanctioned
// checkpoint before plan execution. This is the one sanctioned exception to
// surgical staging: the user explicitly chose "commit first" on the card, so
// staging everything IS their instruction. Failure warns and proceeds — a
// mixed diff beats a bricked apply.
func (s *Session) checkpointCommit(ctx context.Context) {
	if out, err := exec.CommandContext(ctx, "git", "-C", s.root, "add", "-A").CombinedOutput(); err != nil {
		s.printf("%s\n", metaStyle.Render("  ⎿ checkpoint commit failed (git add): "+clip(strings.TrimSpace(string(out)), 120)+" — continuing on the dirty tree"))
		return
	}
	msg := "checkpoint: uncommitted work before plan execution"
	out, err := exec.CommandContext(ctx, "git", "-C", s.root, "commit", "-m", msg).CombinedOutput()
	if err != nil {
		s.printf("%s\n", metaStyle.Render("  ⎿ checkpoint commit failed: "+clip(strings.TrimSpace(string(out)), 120)+" — continuing on the dirty tree"))
		return
	}
	sha, _ := exec.CommandContext(ctx, "git", "-C", s.root, "rev-parse", "--short", "HEAD").Output()
	s.printf("%s\n", metaStyle.Render("  ⎿ checkpointed your changes as "+strings.TrimSpace(string(sha))+" — executing the plan on a clean tree"))
	s.emit(ctx, events.KindCommandExecuted, map[string]any{"command": "git commit (pre-plan checkpoint)", "via": "commit-gate"})
}
