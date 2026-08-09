package runtime

import (
	"testing"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
)

// bashStat is the three-state tool marker: a clean exit is success, a command that couldn't run
// is a failure, and a chained SEQUENCE that produced output but ended non-zero (a trailing grep
// that matched nothing) is a partial — shown yellow, not red. Lock the boundaries.
func TestBashStat(t *testing.T) {
	cases := []struct {
		name     string
		stdout   string
		exitCode int
		command  string
		want     toolStat
	}{
		{"clean exit", "stuff", 0, "go vet ./...", statOK},
		{"clean exit, no output", "", 0, "true", statOK},
		{"single command failed", "", 1, "go test ./...", statFail},
		{"single command failed with output", "FAIL\n", 1, "go test ./...", statFail},
		{"pipeline failed (one stmt)", "", 1, "go build ./... | tail", statFail},
		{"command not found", "", 127, "staticcheck ./...", statFail},
		{"not executable", "", 126, "./script.sh", statFail},
		{"could not run / signal", "", -1, "sleep 1", statFail},
		{"chained, last grep no-match, with output", "a\nb\n", 1, "go list ./...; echo ---; grep zzz f", statWarn},
		{"chained non-zero but NO output → failure", "", 1, "false; grep zzz f", statFail},
	}
	for _, c := range cases {
		if got := bashStat(c.stdout, c.exitCode, c.command); got != c.want {
			t.Errorf("%s: bashStat(%q, %d, %q) = %d, want %d", c.name, c.stdout, c.exitCode, c.command, got, c.want)
		}
	}
}

// IsSequence must use the shell AST: a|b and a&&b are ONE statement; a;b and newline-separated
// are sequences. (A trailing grep in a sequence is the partial-success case bashStat keys on.)
func TestIsSequence(t *testing.T) {
	seq := []string{"a; b", "a; b; c", "cd api && grep x f; echo ---; grep y f", "a\nb"}
	for _, c := range seq {
		if !permissions.IsSequence(c) {
			t.Errorf("%q should be a sequence", c)
		}
	}
	single := []string{"grep x f", "a | b | c", "a && b", "go test ./...", "a || b"}
	for _, c := range single {
		if permissions.IsSequence(c) {
			t.Errorf("%q is a single command/pipeline, not a sequence", c)
		}
	}
}
