package runtime

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// run_tests finds a runner for the common project shapes, including a plain
// Python project with test files and no config (the case that sent a session
// around in circles).
func TestDetectTestRunner(t *testing.T) {
	pyRunner := "unittest"
	if _, err := exec.LookPath("pytest"); err == nil {
		pyRunner = "pytest"
	}
	for _, tc := range []struct {
		name  string
		files map[string]string
		want  string
	}{
		{"go module", map[string]string{"go.mod": "module x"}, "go"},
		{"python tests, no config", map[string]string{"tests/test_engine.py": ""}, pyRunner},
		{"python package tests", map[string]string{"moonlight/tests/test_level1.py": ""}, pyRunner},
		{"jest", map[string]string{"package.json": `{"devDependencies":{"jest":"1"}}`}, "jest"},
		{"npm test script", map[string]string{"package.json": `{"scripts":{"test":"node test.js"}}`}, "npm"},
		{"npm placeholder test", map[string]string{"package.json": `{"scripts":{"test":"echo \"Error: no test specified\" && exit 1"}}`}, ""},
		{"cargo", map[string]string{"Cargo.toml": "[package]"}, "cargo"},
		{"make test", map[string]string{"Makefile": "build:\n\tgo build\ntest:\n\t./run\n"}, "make"},
		{"nothing", map[string]string{"README.md": "hi"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			for rel, body := range tc.files {
				writeFile(t, root, rel, body)
			}
			if got := detectTestRunner(root); got != tc.want {
				t.Fatalf("detectTestRunner = %q, want %q", got, tc.want)
			}
		})
	}
}

// Docs-only edits don't invalidate a passing test run for the completion gate;
// code and data edits do.
func TestDocsOnlyEditsKeepVerification(t *testing.T) {
	for path, want := range map[string]bool{
		"README.md": true, ".gitignore": true, "LICENSE": true, "docs/guide.mdx": true, "CHANGELOG.md": true,
		"engine.py": false, "lexicon/core.txt": false, "config.json": false,
	} {
		if got := docsOnlyPath(path); got != want {
			t.Errorf("docsOnlyPath(%q) = %v, want %v", path, got, want)
		}
	}
	s := &Session{}
	s.markEdited("engine.py")
	s.metrics.toolCalls = 5
	s.markVerified(true)
	s.metrics.toolCalls = 6
	s.markEdited("README.md")
	s.markEdited(".gitignore")
	if s.metrics.lastVerifyOKSeq <= s.metrics.lastEditSeq {
		t.Fatal("a README/.gitignore edit after passing tests must not require rerunning them")
	}
	s.metrics.toolCalls = 7
	s.markEdited("engine.py")
	if s.metrics.lastVerifyOKSeq > s.metrics.lastEditSeq {
		t.Fatal("a code edit after the tests must require rerunning them")
	}
}
