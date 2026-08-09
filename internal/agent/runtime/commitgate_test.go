package runtime

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/config"
	"github.com/memcode-ai/memcode/internal/store"
)

func gitRepo(t *testing.T, dirty bool) string {
	t.Helper()
	root := t.TempDir()
	for _, args := range [][]string{{"init"}, {"config", "user.email", "t@t"}, {"config", "user.name", "t"}} {
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		if err := cmd.Run(); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	if _, err := config.Init(root, false); err != nil { // a real session always has an initialized .memcode
		t.Fatalf("config.Init: %v", err)
	}
	if dirty {
		if err := os.WriteFile(filepath.Join(root, "wip.txt"), []byte("in progress\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func gitClean(t *testing.T, root string) bool {
	t.Helper()
	out, err := exec.Command("git", "-C", root, "status", "--porcelain").Output()
	if err != nil {
		t.Fatalf("git status: %v", err)
	}
	return len(strings.TrimSpace(string(out))) == 0
}

func commitGateSess(t *testing.T, root string, answer string) *Session {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	s := newSess(st, &captureProvider{}, root, "m", permissions.ModeAuto, io.Discard)
	s.ask = func(context.Context, AskRequest) AskResponse { return AskResponse{Answer: answer} }
	return s
}

// The commit gate ALWAYS proceeds (it never halts the work). "Commit first"
// actually commits the tree and continues; every other answer proceeds too; a
// clean tree / remembered skip / headless never even asks.
func TestCommitGate(t *testing.T) {
	ctx := context.Background()

	// Dirty + "Commit first, then continue" → commits AND proceeds; tree ends clean.
	rc := gitRepo(t, true)
	if !commitGateSess(t, rc, "Commit first, then continue").commitGateOK(ctx) {
		t.Fatal("commit-first must PROCEED (never halt)")
	}
	if !gitClean(t, rc) {
		t.Fatal("commit-first must actually commit — tree should be clean after")
	}
	// Dirty + dismissed (empty, e.g. auto-resolved) → proceed WITHOUT committing.
	rd := gitRepo(t, true)
	if !commitGateSess(t, rd, "").commitGateOK(ctx) {
		t.Fatal("dismissed must proceed, not brick the apply")
	}
	if gitClean(t, rd) {
		t.Fatal("dismissed must NOT commit — tree should still be dirty")
	}
	// Dirty + "Continue without committing" → proceed, no commit.
	if !commitGateSess(t, gitRepo(t, true), "Continue without committing").commitGateOK(ctx) {
		t.Fatal("continue must proceed")
	}
	// Clean tree → proceed without asking (ask would fail the test if called).
	clean := gitRepo(t, false)
	sClean := commitGateSess(t, clean, "SHOULD-NOT-BE-ASKED")
	asked := false
	sClean.ask = func(context.Context, AskRequest) AskResponse { asked = true; return AskResponse{} }
	if !sClean.commitGateOK(ctx) || asked {
		t.Fatalf("clean tree must proceed without asking (asked=%v)", asked)
	}

	// "Don't ask again" proceeds AND persists skip; a second call never asks.
	root := gitRepo(t, true)
	s := commitGateSess(t, root, "Never ask — just continue")
	if !s.commitGateOK(ctx) {
		t.Fatal("don't-ask-again must proceed")
	}
	cfg, err := config.Load(root)
	if err != nil || cfg.CommitBeforeWork != "skip" {
		t.Fatalf("choice not persisted: %+v err=%v", cfg, err)
	}
	s2 := commitGateSess(t, root, "SHOULD-NOT-BE-ASKED")
	asked2 := false
	s2.ask = func(context.Context, AskRequest) AskResponse { asked2 = true; return AskResponse{} }
	if !s2.commitGateOK(ctx) || asked2 {
		t.Fatalf("remembered skip must proceed silently (asked=%v)", asked2)
	}

	// "Always commit first" persists "commit" AND commits now; the NEXT dirty
	// apply checkpoints silently without asking.
	ra := gitRepo(t, true)
	sa := commitGateSess(t, ra, "Always commit first (don't ask again)")
	if !sa.commitGateOK(ctx) {
		t.Fatal("always-commit must proceed")
	}
	if !gitClean(t, ra) {
		t.Fatal("always-commit must commit the tree now")
	}
	cfgA, err := config.Load(ra)
	if err != nil || cfgA.CommitBeforeWork != "commit" {
		t.Fatalf("always-commit not persisted: %+v err=%v", cfgA, err)
	}
	if err := os.WriteFile(filepath.Join(ra, "wip2.txt"), []byte("more\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sa2 := commitGateSess(t, ra, "SHOULD-NOT-BE-ASKED")
	askedA := false
	sa2.ask = func(context.Context, AskRequest) AskResponse { askedA = true; return AskResponse{} }
	if !sa2.commitGateOK(ctx) || askedA {
		t.Fatalf("remembered always-commit must not ask (asked=%v)", askedA)
	}
	if !gitClean(t, ra) {
		t.Fatal("remembered always-commit must checkpoint silently")
	}

	// A remembered preference means the SELECTOR doesn't offer the choice either.
	if commitGateSess(t, ra, "").CommitGateNeeded(ctx) {
		t.Fatal("remembered pref must suppress the selector's commit choice")
	}

	// No ask channel (headless) → never blocks.
	sh := commitGateSess(t, gitRepo(t, true), "")
	sh.ask = nil
	if !sh.commitGateOK(ctx) {
		t.Fatal("headless (no ask) must proceed")
	}

	// SELECTOR path: the plan selector carries the commit decision. Commit-first
	// commits at choice time; the gate then consumes the one-shot without asking.
	// The latch lives in the plan machine now and only arms during a live plan
	// session (the selector always fires over a Presented plan in production).
	rs := gitRepo(t, true)
	ss := commitGateSess(t, rs, "SHOULD-NOT-BE-ASKED")
	if !ss.CommitGateNeeded(ctx) {
		t.Fatal("dirty interactive repo must offer the selector commit choice")
	}
	enterPlanForTest(ss, "")
	ss.planCtl.Present("1. do the work\n2. verify")
	ss.CommitGateChoice(ctx, true)
	ss.planCtl.Approve("") // /execute: the arm must survive into the apply phase
	if !gitClean(t, rs) {
		t.Fatal("selector commit-first must commit at choice time")
	}
	askedS := false
	ss.ask = func(context.Context, AskRequest) AskResponse { askedS = true; return AskResponse{} }
	if !ss.commitGateOK(ctx) || askedS {
		t.Fatalf("resolved gate must proceed without asking (asked=%v)", askedS)
	}
	if ss.CommitGateNeeded(ctx) {
		t.Fatal("clean tree must not offer the commit choice")
	}

	// "Execute without committing": resolved silently, no commit — and the
	// one-shot is CONSUMED, so a later apply on a still-dirty tree asks again.
	rw := gitRepo(t, true)
	sw := commitGateSess(t, rw, "SHOULD-NOT-BE-ASKED")
	enterPlanForTest(sw, "")
	sw.planCtl.Present("1. do the work\n2. verify")
	sw.CommitGateChoice(ctx, false)
	sw.planCtl.Approve("") // /execute: the arm survives into the apply phase
	askedW := false
	sw.ask = func(context.Context, AskRequest) AskResponse { askedW = true; return AskResponse{} }
	if !sw.commitGateOK(ctx) || askedW {
		t.Fatalf("resolved gate must proceed silently (asked=%v)", askedW)
	}
	if gitClean(t, rw) {
		t.Fatal("execute-without-committing must NOT commit")
	}
	askedW2 := false
	sw.ask = func(context.Context, AskRequest) AskResponse { askedW2 = true; return AskResponse{} }
	if !sw.commitGateOK(ctx) || !askedW2 {
		t.Fatalf("one-shot must be consumed — the next apply should ask (asked=%v)", askedW2)
	}
}
