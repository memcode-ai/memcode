package grok

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/memcode-ai/memcode/internal/subscription/oauthtest"
)

// TestHelperResolveProcess is the child of TestResolveAcrossProcesses.
func TestHelperResolveProcess(t *testing.T) {
	u := os.Getenv("GROK_HELPER_ISSUER")
	if u == "" {
		t.Skip("helper process only")
	}
	issuer = u
	tok, err := Resolve()
	if err != nil {
		os.Stdout.WriteString("ERR " + err.Error() + "\n")
		os.Exit(1)
	}
	os.Stdout.WriteString("TOKEN " + tok + "\n")
	os.Exit(0)
}

// Concurrent memcode processes with an expired Grok login: exactly one spends
// the single-use refresh token; the rest re-read the rotated login.
func TestResolveAcrossProcesses(t *testing.T) {
	srv := oauthtest.New("r0", 300*time.Millisecond, nil)
	defer srv.Close()
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	if err := write(creds{AccessToken: "acc-old", RefreshToken: "r0", TokenEndpoint: srv.URL, ExpiresAt: time.Now().Unix() - 10}); err != nil {
		t.Fatal(err)
	}
	const procs = 3
	outs := make([]string, procs)
	var wg sync.WaitGroup
	for i := range procs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := exec.Command(os.Args[0], "-test.run=^TestHelperResolveProcess$", "-test.count=1")
			cmd.Env = append(os.Environ(), "XDG_CONFIG_HOME="+cfg, "MEMCODE_HOME="+filepath.Join(cfg, "memcode-home"),
				"GROK_HELPER_ISSUER="+srv.URL)
			var b bytes.Buffer
			cmd.Stdout = &b
			_ = cmd.Run()
			outs[i] = b.String()
		}()
	}
	wg.Wait()
	for i, o := range outs {
		if !strings.Contains(o, "TOKEN access-1\n") {
			t.Errorf("process %d: %q, want the one refreshed token", i, o)
		}
	}
	if n, r := srv.Refreshes(), srv.Rejected(); n != 1 || r != 0 {
		t.Fatalf("refreshes=%d rejected=%d, want exactly 1 refresh and no replayed token", n, r)
	}
}
