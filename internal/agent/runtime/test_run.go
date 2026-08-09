package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/memcode-ai/memcode/internal/agent/tools"
)

// runTestsTool runs the repo's tests and returns a STRUCTURED pass/fail summary — the
// counts plus the failing test names and their output — instead of a raw log the model
// has to scan. It auto-detects the runner from the repo, running Go's structured -json
// output when it's a Go module (per-test results, precise), and a generic run for
// pytest/jest (summary line + tail). Marks the verify metric like a `go test` bash run.
func (s *Session) runTestsTool(ctx context.Context, input json.RawMessage) toolResult {
	var in tools.RunTestsInput
	if err := json.Unmarshal(input, &in); err != nil {
		return errResult(err.Error())
	}
	cctx, cancel := context.WithTimeout(ctx, bashTimeout)
	defer cancel()

	switch runner := detectTestRunner(s.root); runner {
	case "go":
		return s.runGoTests(cctx, in)
	case "pytest":
		return s.runGenericTests(cctx, "pytest", pytestArgs(in), in)
	case "jest":
		return s.runGenericTests(cctx, "npx", jestArgs(in), in)
	default:
		return errResult("run_tests couldn't detect a test runner (no go.mod / pyproject.toml / package.json with jest|vitest). Use bash to run the project's test command directly.")
	}
}

// detectTestRunner picks a runner from repo markers. Go wins when go.mod is present
// (this is a Go-first tool); otherwise pytest, then jest/vitest.
func detectTestRunner(root string) string {
	if exists(filepath.Join(root, "go.mod")) {
		return "go"
	}
	if exists(filepath.Join(root, "pyproject.toml")) || exists(filepath.Join(root, "pytest.ini")) || exists(filepath.Join(root, "setup.cfg")) {
		return "pytest"
	}
	if b, err := os.ReadFile(filepath.Join(root, "package.json")); err == nil {
		if s := string(b); strings.Contains(s, "\"jest\"") || strings.Contains(s, "\"vitest\"") {
			return "jest"
		}
	}
	return ""
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// goTestEvent is one line of `go test -json` output.
type goTestEvent struct {
	Action  string `json:"Action"` // run | pass | fail | skip | output
	Package string `json:"Package"`
	Test    string `json:"Test"`
	Output  string `json:"Output"`
}

// runGoTests runs `go test -json` and reduces the event stream to a structured summary:
// pass/fail/skip counts and, for each failing test, its captured output.
func (s *Session) runGoTests(ctx context.Context, in tools.RunTestsInput) toolResult {
	pkg := "./..."
	if in.Path != "" {
		pkg = in.Path
	}
	args := []string{"test", "-json"}
	if in.Run != "" {
		args = append(args, "-run", in.Run)
	}
	args = append(args, pkg)
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = s.root
	out, _ := cmd.CombinedOutput() // non-zero exit on failure is expected; we parse the stream

	var pass, fail, skip int
	failOutput := map[string]*strings.Builder{} // "pkg.Test" → captured output
	order := []string{}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue // a non-JSON line (e.g. a build error) — surfaced via the raw tail below
		}
		var e goTestEvent
		if json.Unmarshal([]byte(line), &e) != nil {
			continue
		}
		key := e.Package + "." + e.Test
		switch e.Action {
		case "pass":
			if e.Test != "" {
				pass++
			}
		case "skip":
			if e.Test != "" {
				skip++
			}
		case "fail":
			if e.Test != "" {
				fail++
			}
		case "output":
			if e.Test != "" {
				b := failOutput[key]
				if b == nil {
					b = &strings.Builder{}
					failOutput[key] = b
					order = append(order, key)
				}
				b.WriteString(e.Output)
			}
		}
	}

	// A build failure (no JSON test events) — surface the raw compiler error.
	if pass+fail+skip == 0 {
		raw := strings.TrimSpace(string(out))
		s.toolLine(true, "RunTests", "go", "build failed", true)
		return errResult("go test produced no results (likely a build error):\n" + truncate(raw, 4000))
	}

	s.markVerified(fail == 0)
	var b strings.Builder
	fmt.Fprintf(&b, "go test: %d passed, %d failed, %d skipped\n", pass, fail, skip)
	if fail > 0 {
		b.WriteString("\nFAILURES:\n")
		sort.Strings(order)
		shown := 0
		for _, key := range order {
			// Only the tests that actually FAILED have their output worth showing; a passed
			// test's "output" (if captured) is noise. We can't cheaply know which key failed
			// without tracking, so show outputs for keys that contain a FAIL marker.
			body := failOutput[key].String()
			if !strings.Contains(body, "--- FAIL") && !strings.Contains(body, "FAIL") {
				continue
			}
			if shown >= 15 {
				fmt.Fprintf(&b, "\n…and more failures (showing first 15)\n")
				break
			}
			shown++
			fmt.Fprintf(&b, "\n%s\n%s", strings.TrimPrefix(key, "."), truncate(strings.TrimSpace(body), 1500))
		}
	}
	status := fmt.Sprintf("%d/%d pass", pass, pass+fail)
	s.toolLine(true, "RunTests", "go", status, fail > 0)
	return textResult(strings.TrimRight(b.String(), "\n"))
}

// runGenericTests runs pytest/jest and returns the summary line plus the failing tail —
// less structured than Go, but still the signal (counts + what failed), not the full log.
func (s *Session) runGenericTests(ctx context.Context, bin string, args []string, in tools.RunTestsInput) toolResult {
	if _, err := exec.LookPath(bin); err != nil {
		return errResult(bin + " isn't on PATH — run the project's test command via bash instead.")
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = s.root
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	s.markVerified(err == nil)
	// Keep the tail (summary lines live at the end for both pytest and jest) plus any lines
	// naming a failure, so the model sees the counts and what broke without the whole log.
	summary := lastLines(text, 40)
	s.toolLine(true, "RunTests", bin, map[bool]string{true: "pass", false: "fail"}[err == nil], err != nil)
	return textResult(s.redactor.Redact(truncate(summary, maxToolOutput)))
}

func pytestArgs(in tools.RunTestsInput) []string {
	args := []string{"-q"}
	if in.Run != "" {
		args = append(args, "-k", in.Run)
	}
	if in.Path != "" {
		args = append(args, in.Path)
	}
	return args
}

func jestArgs(in tools.RunTestsInput) []string {
	args := []string{"jest", "--silent"}
	if in.Run != "" {
		args = append(args, "-t", in.Run)
	}
	if in.Path != "" {
		args = append(args, in.Path)
	}
	return args
}

// lastLines returns the last n lines of s.
func lastLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
