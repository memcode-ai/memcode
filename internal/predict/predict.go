// Package predict infers where a developer was working and what they were about
// to do next. The evidence is deterministic (recent commits, the current dirty
// working tree, the subsystems those touch, active objectives); the synthesis
// is a single model call. This is the seed of the expectations → deviations
// layer: a prediction is an expectation about the next step.
package predict

import (
	"context"
	"os/exec"
	"strings"

	"github.com/memcode-ai/memcode/internal/agent/focus"
	"github.com/memcode-ai/memcode/internal/agent/secrets"
	"github.com/memcode-ai/memcode/internal/gitlog"
	"github.com/memcode-ai/memcode/internal/objectives"
	"github.com/memcode-ai/memcode/internal/producer"
	"github.com/memcode-ai/memcode/internal/sessionlog"
	"github.com/memcode-ai/memcode/internal/store"
	"github.com/memcode-ai/memcode/internal/structure"
)

// Evidence is the deterministic signal the prediction is built from.
type Evidence struct {
	Commits    []gitlog.Commit
	DirtyFiles []string
	Diff       string // current working-tree diff (truncated, redacted)
	Subsystems []string
	Objectives []string
	Focus      focus.State // where you left off — current/open/paused/done, from the episodic log + objectives
}

// Gather collects the evidence for a prediction (no model call). excludeSessionID
// is the current live session, if any — its episodic signal is skipped so a fresh
// session doesn't shadow the prior meaningful one ("where you left off" must point
// at the LAST distinct session). Pass "" from standalone CLI where there is none.
func Gather(ctx context.Context, st store.Store, root, excludeSessionID string) (Evidence, error) {
	topo, err := structure.Load(ctx, st)
	if err != nil {
		return Evidence{}, err
	}

	ev := Evidence{
		Commits:    gitlog.Recent(ctx, root, "", 12),
		DirtyFiles: dirtyFiles(ctx, root),
	}

	// Secrets must never reach the model, even in a diff.
	red := secrets.NewFromEnv()
	ev.Diff = red.Redact(truncate(workingDiff(ctx, root), 4000))

	seen := map[string]bool{}
	for _, f := range ev.DirtyFiles {
		if sub, ok := structure.ContainingSubsystem(topo.Subsystems, f); ok && !seen[sub.Key] {
			seen[sub.Key] = true
			ev.Subsystems = append(ev.Subsystems, sub.Key)
		}
	}
	// If nothing is dirty, fall back to the subsystem of the most recent change.
	if len(ev.Subsystems) == 0 {
		for _, f := range recentlyChanged(ctx, root, 1) {
			if sub, ok := structure.ContainingSubsystem(topo.Subsystems, f); ok {
				ev.Subsystems = append(ev.Subsystems, sub.Key)
				break
			}
		}
	}

	cur, _ := objectives.New(st).Current(ctx)
	for _, o := range cur {
		ev.Objectives = append(ev.Objectives, o.Title)
	}

	// Cognitive signal: the latest DISTINCT session reduced to a FocusState (current/
	// open/paused/done) plus durable objectives. Excluding the current session keeps
	// "where you left off" sharp instead of echoing the fresh session back.
	if recs, err := sessionlog.LatestRecentExcluding(root, excludeSessionID, 120); err == nil && len(recs) > 0 {
		ev.Focus = focus.Reduce(recs, cur)
	}
	return ev, nil
}

// Prompt builds the (system, user) messages for the synthesis call.
// UserPrompt serializes the evidence into the user message. The system prompt
// is the gateway's (mode "predict") — the CLI ships facts, not doctrine.
func UserPrompt(ev Evidence) (user string) {
	var b strings.Builder
	// Ordered by what the next move should come FROM: open intent and in-flight work
	// first; completed commits LAST as context. (Leading with commits made the model
	// anchor on done work and recommend redoing it.)
	if len(ev.Objectives) > 0 {
		b.WriteString("OPEN objectives (durable intent — the next move usually serves one of these):\n")
		for _, o := range ev.Objectives {
			b.WriteString("  - " + o + "\n")
		}
		b.WriteString("\n")
	}
	if lines := ev.Focus.Lines(); len(lines) > 0 {
		b.WriteString("Where you left off (unresolved/paused threads from the episodic log):\n")
		for _, l := range lines {
			b.WriteString("  - " + l + "\n")
		}
		b.WriteString("\n")
	}
	if len(ev.DirtyFiles) > 0 {
		b.WriteString("Uncommitted, in-flight changes (work NOT yet finished — often the next move is to finish/verify this):\n")
		for _, f := range ev.DirtyFiles {
			b.WriteString("  " + f + "\n")
		}
		b.WriteString("\n")
	}
	if strings.TrimSpace(ev.Diff) != "" {
		b.WriteString("Current diff (truncated):\n" + ev.Diff + "\n\n")
	}
	if len(ev.Subsystems) > 0 {
		b.WriteString("Active subsystem(s): " + strings.Join(ev.Subsystems, ", ") + "\n\n")
	}
	b.WriteString("Recently COMPLETED commits (already shipped — CONTEXT only; never recommend finishing/redoing these; newest first; [tool] = who produced it):\n")
	for _, c := range ev.Commits {
		tag := ""
		if r := producer.Classify(c.Author, c.AuthorEmail, c.Subject, c.Body); r.Producer != producer.Human {
			tag = " [" + string(r.Producer) + "]"
		}
		b.WriteString("  " + c.Date + "  " + c.Subject + tag + "\n")
	}
	return b.String()
}

// --- deterministic git helpers ---

func dirtyFiles(ctx context.Context, root string) []string {
	out, err := exec.CommandContext(ctx, "git", "-C", root, "status", "--porcelain").Output()
	if err != nil {
		return nil
	}
	var files []string
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if len(line) < 4 {
			continue
		}
		p := strings.TrimSpace(line[3:])
		if i := strings.Index(p, " -> "); i >= 0 {
			p = p[i+4:]
		}
		if p == ".memcode" || strings.HasPrefix(p, ".memcode/") {
			continue // memcode's own state isn't the developer's work
		}
		files = append(files, p)
	}
	return files
}

func workingDiff(ctx context.Context, root string) string {
	out, err := exec.CommandContext(ctx, "git", "-C", root, "diff").Output()
	if err != nil {
		return ""
	}
	return string(out)
}

func recentlyChanged(ctx context.Context, root string, nCommits int) []string {
	out, err := exec.CommandContext(ctx, "git", "-C", root, "show", "--name-only", "--format=").Output()
	if err != nil {
		return nil
	}
	var files []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			files = append(files, line)
		}
	}
	return files
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n…(truncated)"
}
