package personal

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

type RunSpec struct {
	Executable             string
	Args                   []string
	Inputs                 map[string][]byte
	AllowedExecutables     []string
	Timeout                time.Duration
	MaxOutputBytes         int
	Environment            map[string]string
	RequireHardenedSandbox bool
}
type RunResult struct {
	Stdout, Stderr string
	ExitCode       int
	ChangedFiles   []string
}

func RunGenerated(ctx context.Context, s RunSpec) (RunResult, error) {
	if s.RequireHardenedSandbox && !SandboxAvailable() {
		return RunResult{}, fmt.Errorf("enforceable generated-code sandbox is unavailable")
	}
	if !subset([]string{s.Executable}, s.AllowedExecutables) {
		return RunResult{}, fmt.Errorf("executable %q is not allowed", s.Executable)
	}
	dir, err := os.MkdirTemp("", "memcode-personal-run-")
	if err != nil {
		return RunResult{}, err
	}
	defer os.RemoveAll(dir)
	for p, b := range s.Inputs {
		clean := filepath.Clean(p)
		if filepath.IsAbs(clean) || clean == ".." {
			return RunResult{}, fmt.Errorf("invalid staged input %q", p)
		}
		full := filepath.Join(dir, clean)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			return RunResult{}, err
		}
		if err := os.WriteFile(full, b, 0o600); err != nil {
			return RunResult{}, err
		}
	}
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, s.Executable, s.Args...)
	cmd.Dir = dir
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + dir, "TMPDIR=" + dir}
	for k, v := range s.Environment {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &limitedWriter{w: &stdout, n: limit(s.MaxOutputBytes)}
	cmd.Stderr = &limitedWriter{w: &stderr, n: limit(s.MaxOutputBytes)}
	err = cmd.Run()
	result := RunResult{Stdout: stdout.String(), Stderr: stderr.String()}
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			result.ExitCode = ee.ExitCode()
		} else {
			return result, err
		}
	}
	return result, nil
}
func SandboxAvailable() bool         { return runtime.GOOS == "linux" && commandExists("bwrap") }
func commandExists(name string) bool { _, err := exec.LookPath(name); return err == nil }
func limit(n int) int {
	if n <= 0 {
		return 1 << 20
	}
	return n
}

type limitedWriter struct {
	w *bytes.Buffer
	n int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	orig := len(p)
	if l.n <= 0 {
		return orig, nil
	}
	if len(p) > l.n {
		p = p[:l.n]
	}
	_, err := l.w.Write(p)
	l.n -= len(p)
	return orig, err
}
