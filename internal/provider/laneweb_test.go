package provider

import (
	"context"
	"testing"
)

// Web search/fetch follow the session's pin onto its vendor's lane, so a Claude
// subscription serves them instead of the gateway side channel. No pin, a pin
// with no lane, or a lane that can't serve web calls (Codex) keeps the gateway.
func TestLaneWebFollowsThePin(t *testing.T) {
	claude := laneFor(t, "anthropic", "claude-sub", "claude-opus-5-5", "https://api.anthropic.com", nil)
	codex := laneFor(t, "openai", "codex", "terra", "https://chatgpt.com/backend-api/codex", nil)
	l := lazyWith(nil, []lane{claude, codex})

	if l.laneWeb(WithPin(context.Background(), "opus")) == nil {
		t.Error("an opus pin on a Claude subscription must use the lane's web channel")
	}
	if l.laneWeb(context.Background()) != nil {
		t.Error("no pin on ctx must keep the gateway side channel")
	}
	if l.laneWeb(WithPin(context.Background(), "gemini-pro")) != nil {
		t.Error("a pin with no lane of its vendor must keep the gateway side channel")
	}
	if l.laneWeb(WithPin(context.Background(), "terra")) != nil {
		t.Error("the Codex lane can't serve web calls; it must keep the gateway side channel")
	}
}
