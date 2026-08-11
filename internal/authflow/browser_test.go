package authflow

import (
	"strings"
	"testing"
)

// The Windows-only login failure was `rundll32 url.dll,FileProtocolHandler`
// mangling the auth URL's ? into %3F. windowsBrowserCmd replaced it with
// `cmd /c start`, which reintroduces a different hazard: cmd treats & as a
// command separator, so the &state= half of the query can be lost. This test
// verifies the argv is built so BOTH query params survive and the ? is never
// re-encoded. (OS-level `start` behavior still needs a real Windows box; this
// pins the quoting logic that we can reason about deterministically.)
func TestWindowsBrowserCmdPreservesFullQuery(t *testing.T) {
	const url = "https://www.memcode.ai/api/cli/auth?port=54321&state=abc123"

	prog, args := windowsBrowserCmd(url)

	if prog != "cmd" {
		t.Fatalf("prog = %q, want cmd", prog)
	}
	if len(args) != 4 || args[0] != "/c" || args[1] != "start" || args[2] != "" {
		t.Fatalf("args = %v, want [/c start \"\" <url>]", args)
	}

	got := args[3] // the URL argument as cmd will receive it on the command line

	if strings.Contains(got, "%3F") {
		t.Fatalf("the ? was percent-encoded to %%3F (the original bug): %q", got)
	}
	if !strings.Contains(got, "/api/cli/auth?port=") {
		t.Fatalf("the ? was not preserved literally: %q", got)
	}

	// Every & must be caret-escaped, or cmd reads it as a command separator and
	// drops everything after it (losing &state=).
	if strings.Contains(strings.ReplaceAll(got, "^&", ""), "&") {
		t.Fatalf("found an unescaped & — cmd would truncate the URL there: %q", got)
	}

	// cmd collapses ^& back to & before launching the browser. Simulate that and
	// assert we recover the EXACT original URL — including &state=abc123.
	if recovered := strings.ReplaceAll(got, "^&", "&"); recovered != url {
		t.Fatalf("browser would receive %q, want %q", recovered, url)
	}
}
