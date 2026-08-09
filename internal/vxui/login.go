package vxui

// /login and /logout — the TUI halves of mandatory login. The browser flow
// itself lives in internal/authflow (shared with `memcode login`); this file
// owns the in-session experience: async run with Esc/Ctrl+C cancel, progress
// lines, credential swap without a restart, and the post-login models refresh.

import (
	"github.com/memcode-ai/memcode/internal/forks/vaxis/ui"

	"context"
	"fmt"
	"os"

	"github.com/memcode-ai/memcode/internal/authflow"
	"github.com/memcode-ai/memcode/internal/provider"
)

// loginSlash runs the browser login flow without leaving the TUI. Endpoint
// mode counts as connected but NOT signed in — /login still works there and
// switches the session onto the hosted gateway.
func (s *appState) loginSlash() {
	if _, onEndpoint := s.w.sess.Endpoint(); s.w.sess.Connected() && !onEndpoint {
		s.sysln("already signed in — gateway " + provider.APIURL())
		return
	}
	s.runAsync(func(ctx context.Context) string {
		res, err := authflow.Run(ctx, func(msg string) {
			s.rt.Dispatch(func() { s.sysln("  " + msg) })
		})
		if err != nil {
			return "✗ " + err.Error()
		}
		if werr := authflow.WriteGlobalEnvToken(res.Token, res.GatewayURL); werr != nil {
			return fmt.Sprintf("✗ signed in, but saving credentials failed: %v", werr)
		}
		// Export in-process too: dotenv no-override precedence would otherwise
		// hide the fresh token from FetchModels and spawned children.
		os.Setenv(provider.EnvAPIToken, res.Token)
		os.Setenv(provider.EnvAPIURL, res.GatewayURL)
		// The token must never surface in transcripts or tool output.
		s.w.sess.AddRedactSecrets(res.Token)
		// Swap credentials into the live session — turns work immediately.
		s.w.sess.SetCredentials(res.GatewayURL, res.Token)
		s.resolveServingDefault()
		return "✓ signed in — you're ready to go."
	})
}

// logoutSlash strips the saved token and disconnects the live session. With a
// custom endpoint configured the session falls back onto it (the provider's
// backend selection) — signing out of memcode is not dead air there.
func (s *appState) logoutSlash() {
	removed, err := authflow.StripGlobalEnvToken()
	if err != nil {
		s.sysln("✗ " + err.Error())
		return
	}
	os.Unsetenv(provider.EnvAPIToken)
	os.Unsetenv(provider.EnvAPIURL)
	s.w.sess.ClearCredentials()
	if ep, onEndpoint := s.w.sess.Endpoint(); onEndpoint {
		if removed {
			s.sysln("✓ signed out of memcode.ai — serving from endpoint " + ep.Name)
		} else {
			s.sysln("○ not signed in to memcode.ai — serving from endpoint " + ep.Name)
		}
		return
	}
	if removed {
		s.sysln("✓ signed out — run /login to reconnect")
	} else {
		s.sysln("○ already signed out")
	}
}

// signedOutNotice is the one-line gate message for anything needing the gateway.
func (s *appState) signedOutNotice() {
	s.sysln("○ signed out — run /login to connect to memcode.ai (this needs your account)")
}

// handleLoginPromptKey drives the startup sign-in card: Enter opens the
// browser login, Esc dismisses to the signed-out shell.
func (s *appState) handleLoginPromptKey(key string) ui.EventResult {
	switch key {
	case "Enter":
		s.SetState(func() { s.loginPrompting = false })
		s.loginSlash()
		return ui.EventHandled
	case "Escape":
		s.SetState(func() { s.loginPrompting = false })
		s.sysln("○ browsing signed out — run /login whenever you're ready")
		return ui.EventHandled
	}
	return ui.EventHandled // modal: swallow everything else
}

// loginPromptView renders the startup sign-in card — the first thing a
// not-logged-in user sees, before they type anything. The second path: no
// account needed when you bring your own OpenAI-compatible endpoint (the card
// only shows when NO backend is configured, so this is the pointer, not a
// picker).
func (s *appState) loginPromptView() ui.Widget {
	var rows []ui.Widget
	rows = append(rows, ui.RichText{Spans: []ui.TextSpan{
		{Text: "Welcome to memcode", Style: s.sty.brand},
	}})
	rows = append(rows, ui.RichText{Spans: []ui.TextSpan{
		{Text: "  Sign in to connect this machine to your memcode.ai account —", Style: s.sty.muted},
	}})
	rows = append(rows, ui.RichText{Spans: []ui.TextSpan{
		{Text: "  it takes one browser click and you're ready to build.", Style: s.sty.muted},
	}})
	rows = append(rows, ui.SizedBox{Height: 1})
	rows = append(rows, s.optionList([]choice{{label: "Sign in with your browser"}}, 0, false)...)
	rows = append(rows, ui.SizedBox{Height: 1})
	rows = append(rows, ui.RichText{Spans: []ui.TextSpan{
		{Text: "  or point memcode at a local endpoint: set " + provider.EnvEndpointURL, Style: s.sty.muted},
	}})
	rows = append(rows, ui.RichText{Spans: []ui.TextSpan{
		{Text: "  (e.g. an Ollama http://localhost:11434/v1) and relaunch — no account needed.", Style: s.sty.muted},
	}})
	rows = append(rows, s.hintRow("Enter sign in · Esc later (you can run /login anytime)"))
	return s.card(rows...)
}
