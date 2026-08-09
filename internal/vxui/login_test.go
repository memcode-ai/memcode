package vxui

// Mandatory-login gate tests: signed out, the TUI opens, local commands work,
// and everything gateway-backed (slash or free text) prompts for /login
// instead of dispatching.

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	vaxis "github.com/memcode-ai/memcode/internal/forks/vaxis"
	"github.com/memcode-ai/memcode/internal/forks/vaxis/ui"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/agent/runtime"
	"github.com/memcode-ai/memcode/internal/llm"
	"github.com/memcode-ai/memcode/internal/provider"
	"github.com/memcode-ai/memcode/internal/store"
	"github.com/memcode-ai/memcode/internal/theme"
)

// newSignedOutRecRunner is newRecRunner with a DISCONNECTED lazy provider —
// the real signed-out boot (no MEMCODE_API_TOKEN in env). The startup
// sign-in card is dismissed (Esc) so tests can drive the signed-out shell;
// card behavior itself is covered by the dedicated tests below.
func newSignedOutRecRunner(t *testing.T) (*runtime.Session, *recBackend, *ui.Runner) {
	sess, be, runner, _ := newSignedOutRecRunnerCard(t)
	runner.HandleEvent(vaxis.Key{Keycode: vaxis.KeyEsc}, time.Now())
	return sess, be, runner
}

// newSignedOutRecRunnerCard boots signed out and leaves the sign-in card up.
func newSignedOutRecRunnerCard(t *testing.T) (*runtime.Session, *recBackend, *ui.Runner, *appState) {
	t.Helper()
	theme.Set("aurora")
	t.Setenv(provider.EnvAPIToken, "")
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	sess := runtime.New(st, llm.NewRunner(provider.NewFromEnvLazy()), t.TempDir(), "fake-model", permissions.ModeAuto, io.Discard)
	var state *appState
	root := &stateCapture{appWidget: appWidget{ctx: context.Background(), sess: sess, themeName: "aurora"}, state: &state}
	app := ui.NewApp(root, ui.WithDynamicPrimaryScreen(), ui.WithTheme(uiTheme(theme.Active().Palette)))
	be := &recBackend{ev: make(chan ui.Event)}
	runner := ui.NewRunner(app, be, nil)
	now := time.Now()
	runner.Start(now)
	_ = runner.HandleFrame(now)
	return sess, be, runner, state
}

// TestConnectedDefaults: providers without the Connector capability (every
// test fake) count as connected — the login gate must never fire for them —
// while a lazy provider without a REAL login (memcode_-prefixed key) is
// disconnected.
func TestConnectedDefaults(t *testing.T) {
	if !newTestSession(t).Connected() {
		t.Fatal("a session over a plain fake provider must count as connected")
	}
	t.Setenv(provider.EnvAPIToken, "")
	lazy := provider.NewFromEnvLazy()
	if lazy.Connected() {
		t.Fatal("lazy provider without a token must be disconnected")
	}
	// A stored value that is NOT a memcode_ org key is not a login — this is a
	// LOCAL decision (no network): the TUI boots onto the sign-in card.
	t.Setenv(provider.EnvAPIToken, "some-random-old-value")
	if provider.NewFromEnvLazy().Connected() {
		t.Fatal("a non-memcode_ token must boot disconnected")
	}
	lazy.SetCredentials("https://api.example.com", "memcode_x")
	if !lazy.Connected() {
		t.Fatal("SetCredentials must connect the lazy provider")
	}
	lazy.ClearCredentials()
	if lazy.Connected() {
		t.Fatal("ClearCredentials must disconnect the lazy provider")
	}
}

// TestSignedOutGatesGatewayCommands: every non-local catalog command must show
// the login notice signed out — and must NOT print "unknown command" (proving
// the gate fired before dispatch, not instead of it).
func TestSignedOutGatesGatewayCommands(t *testing.T) {
	for _, c := range slashCommands {
		if c.local {
			continue
		}
		t.Run(c.name, func(t *testing.T) {
			_, be, runner := newSignedOutRecRunner(t)
			now := time.Now()
			typeSlash(runner, c.name, now)
			for i := 0; i < 10; i++ {
				be.drain()
				_ = runner.HandleFrame(now)
			}
			rec := be.recorded()
			if !strings.Contains(rec, "signed out — run /login") {
				t.Fatalf("%s signed out must prompt for /login.\nrecorded=%q", c.name, rec)
			}
			if strings.Contains(rec, "unknown command") {
				t.Fatalf("%s fell through to unknown-command.\nrecorded=%q", c.name, rec)
			}
		})
	}
}

// TestSignedOutLocalCommandsWork: whitelisted commands run normally signed out.
func TestSignedOutLocalCommandsWork(t *testing.T) {
	_, be, runner := newSignedOutRecRunner(t)
	now := time.Now()
	typeSlash(runner, "/todos", now)
	_ = runner.HandleFrame(now)
	if !strings.Contains(be.recorded(), "no active tasks") {
		t.Fatalf("/todos must work signed out.\nrecorded=%q", be.recorded())
	}
	typeSlash(runner, "/status", now)
	_ = runner.HandleFrame(now)
	if !strings.Contains(be.recorded(), "mode") {
		t.Fatalf("/status must work signed out.\nrecorded=%q", be.recorded())
	}
}

// TestSignedOutDoctorLocalReport: /doctor is whitelisted but model-backed —
// signed out it reports local checks (including the sign-in finding) instead
// of dispatching a doomed model call.
func TestSignedOutDoctorLocalReport(t *testing.T) {
	_, be, runner := newSignedOutRecRunner(t)
	now := time.Now()
	typeSlash(runner, "/doctor", now)
	_ = runner.HandleFrame(now)
	rec := be.recorded()
	if !strings.Contains(rec, "not signed in — run /login") {
		t.Fatalf("/doctor signed out must report the sign-in finding.\nrecorded=%q", rec)
	}
}

// TestSignedOutFreeTextGated: a free-text turn is a model call — signed out it
// must show the login notice and never reach the scheduler (no echo prompt).
func TestSignedOutFreeTextGated(t *testing.T) {
	_, be, runner := newSignedOutRecRunner(t)
	now := time.Now()
	typeSlash(runner, "fix the flaky test", now)
	for i := 0; i < 10; i++ {
		be.drain()
		_ = runner.HandleFrame(now)
	}
	rec := be.recorded()
	if !strings.Contains(rec, "signed out — run /login") {
		t.Fatalf("free text signed out must prompt for /login.\nrecorded=%q", rec)
	}
}

// TestLogoutDisconnectsSession: /logout on a connected session flips it to
// signed out, and the very next gateway command hits the gate.
func TestLogoutDisconnectsSession(t *testing.T) {
	theme.Set("aurora")
	t.Setenv(provider.EnvAPIToken, "memcode_live")
	// Fresh XDG home so StripGlobalEnvToken touches a sandbox, not ~/.config.
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	sess := runtime.New(st, llm.NewRunner(provider.NewFromEnvLazy()), t.TempDir(), "fake-model", permissions.ModeAuto, io.Discard)
	if !sess.Connected() {
		t.Fatal("session with env token must boot connected")
	}
	root := &appWidget{ctx: context.Background(), sess: sess, themeName: "aurora"}
	app := ui.NewApp(root, ui.WithDynamicPrimaryScreen(), ui.WithTheme(uiTheme(theme.Active().Palette)))
	be := &recBackend{ev: make(chan ui.Event)}
	runner := ui.NewRunner(app, be, nil)
	now := time.Now()
	runner.Start(now)
	_ = runner.HandleFrame(now)

	typeSlash(runner, "/logout", now)
	_ = runner.HandleFrame(now)
	if sess.Connected() {
		t.Fatal("/logout must disconnect the live session")
	}
	typeSlash(runner, "/model", now)
	_ = runner.HandleFrame(now)
	if !strings.Contains(be.recorded(), "signed out — run /login") {
		t.Fatalf("post-logout gateway command must hit the login gate.\nrecorded=%q", be.recorded())
	}
}

// TestSignInCardShowsAtBoot: a not-logged-in boot opens ON the sign-in card
// (before the user types anything), Esc dismisses to the signed-out shell,
// and the card never appears on a logged-in boot.
func TestSignInCardShowsAtBoot(t *testing.T) {
	_, be, runner, st := newSignedOutRecRunnerCard(t)
	if !st.loginPrompting {
		t.Fatal("signed-out boot must open on the sign-in card")
	}
	// Modal: ordinary keys are swallowed, nothing reaches the composer.
	runner.HandleEvent(vaxis.Key{Text: "h", Keycode: 'h'}, time.Now())
	if st.composer != "" {
		t.Fatalf("card must swallow keys, composer got %q", st.composer)
	}
	runner.HandleEvent(vaxis.Key{Keycode: vaxis.KeyEsc}, time.Now())
	if st.loginPrompting {
		t.Fatal("Esc must dismiss the card")
	}
	if !strings.Contains(be.recorded(), "run /login whenever") {
		t.Fatalf("dismissal should say how to sign in later.\nrecorded=%q", be.recorded())
	}
}

// TestSignInCardEnterStartsLogin: Enter on the card kicks off the login flow
// (the async browser flow raises the busy spinner).
func TestSignInCardEnterStartsLogin(t *testing.T) {
	_, be, runner, st := newSignedOutRecRunnerCard(t)
	now := time.Now()
	runner.HandleEvent(vaxis.Key{Keycode: vaxis.KeyEnter}, now)
	if st.loginPrompting {
		t.Fatal("Enter must close the card")
	}
	// loginSlash ran: the async login owns the busy surface (state lands via
	// the dispatch queue — drain it).
	busy := false
	for i := 0; i < 100 && !busy; i++ {
		be.drain()
		_ = runner.HandleFrame(now)
		busy = st.busy()
		time.Sleep(5 * time.Millisecond)
	}
	if !busy {
		t.Fatal("Enter must start the login flow (busy)")
	}
	// Cancel the in-flight browser flow so the test tears down cleanly.
	if st.asyncCancel != nil {
		st.asyncCancel()
	}
	for i := 0; i < 100 && st.busy(); i++ {
		be.drain()
		_ = runner.HandleFrame(now)
		time.Sleep(5 * time.Millisecond)
	}
}

// TestLoggedInBootSkipsCard: with a real login stored, no card.
func TestLoggedInBootSkipsCard(t *testing.T) {
	theme.Set("aurora")
	t.Setenv(provider.EnvAPIToken, "memcode_live")
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	sess := runtime.New(st, llm.NewRunner(provider.NewFromEnvLazy()), t.TempDir(), "fake-model", permissions.ModeAuto, io.Discard)
	var state *appState
	root := &stateCapture{appWidget: appWidget{ctx: context.Background(), sess: sess, themeName: "aurora"}, state: &state}
	app := ui.NewApp(root, ui.WithDynamicPrimaryScreen(), ui.WithTheme(uiTheme(theme.Active().Palette)))
	be := &recBackend{ev: make(chan ui.Event)}
	runner := ui.NewRunner(app, be, nil)
	now := time.Now()
	runner.Start(now)
	_ = runner.HandleFrame(now)
	if state.loginPrompting {
		t.Fatal("logged-in boot must not show the sign-in card")
	}
}

// TestIsLocalSlashUnknownCounts: unknown commands are "local" so the user gets
// the unknown-command message, not a misleading login nudge.
func TestIsLocalSlashUnknownCounts(t *testing.T) {
	if !isLocalSlash("/definitely-not-a-command") {
		t.Fatal("unknown commands must count as local")
	}
	if isLocalSlash("/advisor") || isLocalSlash("/model") || isLocalSlash("/plan") {
		t.Fatal("gateway-backed commands must not be local")
	}
	for _, name := range []string{"/help", "/login", "/logout", "/theme", "/quit", "/status"} {
		if !isLocalSlash(name) {
			t.Fatalf("%s must be local", name)
		}
	}
}
