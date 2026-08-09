package vxui

import (
	"testing"

	"github.com/memcode-ai/memcode/internal/agent/runtime"
	"github.com/memcode-ai/memcode/internal/theme"
)

// TestAskCardBuilds covers the clarifying-question card and the shared optionList primitive:
// a label row per option plus a muted description row when one is set.
func TestAskCardBuilds(t *testing.T) {
	theme.Set("aurora")
	s := &appState{sty: makeStyles(theme.Active().Palette)}
	s.askReq = &runtime.AskRequest{
		Question: "Which auth provider?",
		Options:  []runtime.AskOption{{Label: "Auth0", Description: "hosted, quick to wire"}, {Label: "Clerk"}},
	}
	if w := s.askCard(); w == nil {
		t.Fatal("askCard returned nil")
	}
	// 2 options, one with a description → 3 rows (2 labels + 1 desc).
	if rows := s.optionList([]choice{{label: "Auth0", desc: "hosted"}, {label: "Clerk"}}, 0, true); len(rows) != 3 {
		t.Fatalf("optionList rows = %d, want 3 (2 labels + 1 desc line)", len(rows))
	}
}
