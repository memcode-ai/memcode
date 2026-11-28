package codex

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/memcode-ai/memcode/internal/subscription/oauthtest"
)

func jwtFor(account string, exp int64) string {
	b, _ := json.Marshal(map[string]any{
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": account},
		"exp":                         exp,
	})
	seg := func(v []byte) string { return base64.RawURLEncoding.EncodeToString(v) }
	return seg([]byte("{}")) + "." + seg(b) + "." + seg([]byte("sig"))
}

// TestHelperResolveProcess is the child of TestResolveAcrossProcesses.
func TestHelperResolveProcess(t *testing.T) {
	u := os.Getenv("CODEX_HELPER_TOKEN_URL")
	if u == "" {
		t.Skip("helper process only")
	}
	codexTokenURL = u
	b, err := Resolve()
	if err != nil {
		os.Stdout.WriteString("ERR " + err.Error() + "\n")
		os.Exit(1)
	}
	os.Stdout.WriteString("TOKEN " + b.Token + "\n")
	os.Exit(0)
}

// Concurrent memcode processes with an expiring Codex login: exactly one spends
// the single-use refresh token; the rest re-read the rotated login.
func TestResolveAcrossProcesses(t *testing.T) {
	dir := t.TempDir()
	body, _ := json.Marshal(map[string]any{"tokens": map[string]string{
		"access_token": jwtFor("acct", time.Now().Add(-time.Hour).Unix()), "refresh_token": "r0",
	}})
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	exp := time.Now().Add(time.Hour).Unix()
	srv := oauthtest.New("r0", 300*time.Millisecond, func(n int) string { return jwtFor(fmt.Sprintf("acct-%d", n), exp) })
	defer srv.Close()
	want := jwtFor("acct-1", exp)

	const procs = 3
	outs := make([]string, procs)
	var wg sync.WaitGroup
	for i := range procs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := exec.Command(os.Args[0], "-test.run=^TestHelperResolveProcess$", "-test.count=1")
			cmd.Env = append(os.Environ(), "CODEX_HOME="+dir, "MEMCODE_HOME="+filepath.Join(dir, "memcode-home"),
				"CODEX_HELPER_TOKEN_URL="+srv.URL)
			var b bytes.Buffer
			cmd.Stdout = &b
			_ = cmd.Run()
			outs[i] = b.String()
		}()
	}
	wg.Wait()
	for i, o := range outs {
		if !strings.Contains(o, "TOKEN "+want+"\n") {
			t.Errorf("process %d: %q, want the one refreshed token", i, o)
		}
	}
	if n, r := srv.Refreshes(), srv.Rejected(); n != 1 || r != 0 {
		t.Fatalf("refreshes=%d rejected=%d, want exactly 1 refresh and no replayed token", n, r)
	}
}
