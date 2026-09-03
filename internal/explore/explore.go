// Package explore runs a fan-out of read-only "reader" sub-agents over a
// repository and synthesizes their findings into one answer. It is the parallel
// half of memcode's "many readers, one serialized writer" model: every explorer
// is read-only (read_file/ripgrep/git_diff, no edits or commands), so any number
// can run concurrently with no write contention. A final synthesis pass merges
// the per-scope findings.
package explore

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/agent/runtime"
	"github.com/memcode-ai/memcode/internal/llm"
	"github.com/memcode-ai/memcode/internal/store"
	"github.com/memcode-ai/memcode/internal/structure"
	"github.com/memcode-ai/memcode/internal/wire"
)

// defaultReaders caps concurrency so a big monorepo doesn't spawn a hundred
// simultaneous model calls. It is the DEFAULT: the agent.explore policy's
// `concurrency` field overrides it when the user has set one.
const defaultReaders = 6

// readers resolves the concurrency cap. 0 (nothing configured) means the
// default, so a caller that has no policy layer stays on today's behavior.
func readers(n int) int {
	if n <= 0 {
		return defaultReaders
	}
	return n
}

// Finding is one explorer's answer for a scope.
type Finding struct {
	Scope  string
	Answer string
	Err    error
}

// Run fans out read-only explorers across the repository's subsystems (each
// scoped to one), collects their findings, and prints a synthesized answer to
// out. The store and provider are shared; the SQLite WAL serializes the event
// writes the explorers emit.
func Run(ctx context.Context, st store.Store, runner *llm.Runner, root, model, question string, concurrency int, out io.Writer) error {
	scopes := pickScopes(ctx, st)
	fmt.Fprintf(out, "exploring %q across %d scope(s) with read-only agents…\n\n", question, len(scopes))

	findings := fanOut(ctx, st, runner, root, model, question, scopes, concurrency)

	for _, f := range findings {
		label := f.Scope
		if label == "" {
			label = "repo"
		}
		if f.Err != nil {
			fmt.Fprintf(out, "● %s — error: %v\n", label, f.Err)
			continue
		}
		fmt.Fprintf(out, "● %s\n%s\n\n", label, indent(strings.TrimSpace(f.Answer)))
	}

	fmt.Fprintf(out, "── synthesis ──\n")
	answer, err := synthesize(ctx, runner, question, findings)
	if err != nil {
		return err
	}
	fmt.Fprintln(out, strings.TrimSpace(answer))
	return nil
}

// fanOut runs one read-only explorer per scope, capped at concurrency.
func fanOut(ctx context.Context, st store.Store, runner *llm.Runner, root, model, question string, scopes []string, concurrency int) []Finding {
	return FanOut(ctx, st, runner, root, model, question, scopes, concurrency, nil)
}

// Progress reports an explorer's lifecycle to a front-end (started/finished per
// scope), so the UI can show compact per-agent progress instead of every call.
type Progress func(scope string, done bool, err error)

// FanOut runs one read-only explorer per scope concurrently (capped by
// concurrency, 0 = the default), reporting lifecycle via progress (may be nil), and returns the
// findings. Each explorer is its own read-only session writing to io.Discard —
// its tool calls and narration never reach the user; only the orchestrator's
// progress + the final synthesis do. This is what keeps deep research QUIET.
func FanOut(ctx context.Context, st store.Store, runner *llm.Runner, root, model, question string, scopes []string, concurrency int, progress Progress) []Finding {
	findings := make([]Finding, len(scopes))
	sem := make(chan struct{}, readers(concurrency))
	var wg sync.WaitGroup
	for i, scope := range scopes {
		wg.Add(1)
		go func(i int, scope string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if progress != nil {
				progress(scope, false, nil)
			}
			sess := runtime.New(st, runner.Fork(), root, model, permissions.ModeAsk, io.Discard)
			ans, err := sess.Answer(ctx, scope, question)
			findings[i] = Finding{Scope: scope, Answer: ans, Err: err}
			if progress != nil {
				progress(scope, true, err)
			}
		}(i, scope)
	}
	wg.Wait()
	return findings
}

// pickScopes returns the subsystem keys to fan out over, or a single whole-repo
// scope when the topology hasn't been modeled yet.
func pickScopes(ctx context.Context, st store.Store) []string {
	topo, err := structure.Load(ctx, st)
	if err != nil || len(topo.Subsystems) == 0 {
		return []string{""}
	}
	scopes := make([]string, 0, len(topo.Subsystems))
	for _, sub := range topo.Subsystems {
		scopes = append(scopes, sub.Key)
	}
	return scopes
}

// synthesize makes one model call that merges the per-scope findings into a
// single grounded answer.
func synthesize(ctx context.Context, runner *llm.Runner, question string, findings []Finding) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "Question: %s\n\nFindings from read-only explorers (one per subsystem):\n", question)
	any := false
	for _, f := range findings {
		if f.Err != nil || strings.TrimSpace(f.Answer) == "" {
			continue
		}
		any = true
		label := f.Scope
		if label == "" {
			label = "repo"
		}
		fmt.Fprintf(&b, "\n[%s]\n%s\n", label, strings.TrimSpace(f.Answer))
	}
	if !any {
		return "No explorer found anything relevant to the question.", nil
	}

	resp, err := runner.Complete(ctx, llm.Explore, wire.Request{
		Mode:      "synthesize",
		MaxTokens: 1500, // a merged answer, not an essay (the ladder routes purpose=explore to the cheap lane)
		Messages:  []wire.Message{{Role: "user", Blocks: []wire.Block{{Type: "text", Text: b.String()}}}},
	})
	if err != nil {
		return "", err
	}
	return resp.Text(), nil
}

func indent(s string) string {
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = "    " + l
	}
	return strings.Join(lines, "\n")
}
