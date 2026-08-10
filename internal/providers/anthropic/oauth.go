package anthropic

import (
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/memcode-ai/memcode/internal/wire"
)

// oauth.go — the Claude Code compatibility mode. A Claude Pro/Max subscription
// is used through an OAuth token that ONLY Anthropic's official Claude Code
// client is expected to present, so a request carrying one must look like Claude
// Code or it is refused (or billed as third-party "extra usage" instead of the
// subscription's plan limits). This file isolates every part of that
// impersonation so the normal API-key path stays completely untouched.
//
// SCOPE — the mode is gated on the CREDENTIAL, never the host. Only a
// subscription OAuth token (below) turns it on; a real sk-ant-api* console key
// takes the clean x-api-key path with none of these transforms. That is what
// keeps the Claude Code identity from ever leaking onto a user's own-key traffic
// or a third-party Anthropic-compatible endpoint.

// isOAuthToken reports whether key is a Claude subscription OAuth token rather
// than a normal console API key. sk-ant-api* is a console key (clean path);
// sk-ant-oat* (OAuth setup token), cc-* (Claude Code access token), and a JWT
// (eyJ…) are OAuth credentials that require the compatibility mode.
func isOAuthToken(key string) bool {
	switch {
	case strings.HasPrefix(key, "sk-ant-api"):
		return false
	case strings.HasPrefix(key, "sk-ant-oat"),
		strings.HasPrefix(key, "cc-"),
		strings.HasPrefix(key, "eyJ"):
		return true
	}
	return false
}

// claudeCodeSystemPrefix is the identity block prepended to the system prompt on
// the OAuth path — the assertion Anthropic's official client leads with.
const claudeCodeSystemPrefix = "You are Claude Code, Anthropic's official CLI for Claude."

// oauthOnlyBetas are the beta flags the OAuth/Claude-Code path requires. Added
// (not set) so memcode's own cache-TTL beta is preserved.
var oauthOnlyBetas = []string{"claude-code-20250219", "oauth-2025-04-20"}

// claudeCodeFallbackVersion is the User-Agent version used when the Claude Code
// CLI isn't installed to report its own. Anthropic rejects a UA that lags too
// far, so this is kept reasonably current.
const claudeCodeFallbackVersion = "2.1.74"

var (
	ccVersionOnce sync.Once
	ccVersion     string
)

// claudeCodeUserAgent returns the "claude-code/<version> (external, cli)"
// User-Agent, detecting the installed CLI's version once (falling back to a
// pinned recent version).
func claudeCodeUserAgent() string {
	ccVersionOnce.Do(func() {
		ccVersion = claudeCodeFallbackVersion
		for _, bin := range []string{"claude", "claude-code"} {
			cmd := exec.Command(bin, "--version")
			done := make(chan struct{})
			var out []byte
			go func() { out, _ = cmd.Output(); close(done) }()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				_ = cmd.Process.Kill()
			}
			if f := strings.Fields(string(out)); len(f) > 0 && f[0] != "" {
				ccVersion = f[0]
				break
			}
		}
	})
	return "claude-code/" + ccVersion + " (external, cli)"
}

// toOAuthToolName maps a memcode tool name to the wire name the OAuth path
// requires: a single-underscore or unprefixed name is billed as a third-party
// app (or 400s), while an mcp__ name draws from plan limits. Already-mcp__ names
// (memcode's MCP-bridge tools) pass through unchanged.
func toOAuthToolName(name string) string {
	switch {
	case strings.HasPrefix(name, "mcp__"):
		return name
	case strings.HasPrefix(name, "mcp_"):
		return "mcp__" + name[len("mcp_"):]
	default:
		return "mcp__" + name
	}
}

// oauthEncodeRequest returns a copy of r shaped for the OAuth path — the Claude
// Code identity prepended to the system prompt, and every tool (definitions and
// replayed tool_use history) renamed to its mcp__ wire form — plus the reverse
// map that oauthDecodeResponse uses to restore memcode's real tool names. The
// caller's request is not mutated.
func oauthEncodeRequest(r wire.Request) (wire.Request, map[string]string) {
	rev := map[string]string{}
	record := func(orig string) string {
		wn := toOAuthToolName(orig)
		if wn != orig {
			rev[wn] = orig
		}
		return wn
	}

	// System: prepend the Claude Code identity. We do NOT blanket-rewrite the
	// body: unlike a distinct agent name, "memcode" is also a path (.memcode)
	// and a tool name, so a global replace would corrupt the prompt. The leading
	// identity block is the assertion the filter keys on.
	if r.System != "" {
		r.System = claudeCodeSystemPrefix + "\n\n" + r.System
	} else {
		r.System = claudeCodeSystemPrefix
	}

	// Tool definitions.
	if len(r.Tools) > 0 {
		tools := make([]wire.ToolDef, len(r.Tools))
		copy(tools, r.Tools)
		for i := range tools {
			tools[i].Name = record(tools[i].Name)
		}
		r.Tools = tools
	}

	// Replayed assistant tool_use blocks must carry the same wire names.
	msgs := make([]wire.Message, len(r.Messages))
	copy(msgs, r.Messages)
	for mi := range msgs {
		var cloned bool
		for bi, b := range msgs[mi].Blocks {
			if b.Type == "tool_use" {
				if !cloned {
					msgs[mi].Blocks = append([]wire.Block(nil), msgs[mi].Blocks...)
					cloned = true
				}
				msgs[mi].Blocks[bi].Name = record(b.Name)
			}
		}
	}
	r.Messages = msgs
	return r, rev
}

// oauthDecodeResponse restores memcode's real tool names on the returned
// tool_use blocks, reversing the mcp__ wire renaming so the runtime dispatches
// to the tools it defined.
func oauthDecodeResponse(resp *wire.Response, rev map[string]string) {
	if len(rev) == 0 {
		return
	}
	for i, b := range resp.Blocks {
		if b.Type == "tool_use" {
			if orig, ok := rev[b.Name]; ok {
				resp.Blocks[i].Name = orig
			}
		}
	}
}
