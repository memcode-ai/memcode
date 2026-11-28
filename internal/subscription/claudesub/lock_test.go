package claudesub

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

// TestHelperResolveProcess is the child process of TestResolveAcrossProcesses:
// one memcode-like process resolving the shared login.
func TestHelperResolveProcess(t *testing.T) {
	u := os.Getenv("CLAUDESUB_HELPER_REFRESH_URL")
	if u == "" {
		t.Skip("helper process only")
	}
	refreshURL = u
	tok, err := Resolve()
	if err != nil {
		os.Stdout.WriteString("ERR " + err.Error() + "\n")
		os.Exit(1)
	}
	os.Stdout.WriteString("TOKEN " + tok + "\n")
	os.Exit(0)
}

// Several processes find the same expired login at once. The file lock plus
// the post-lock re-read mean exactly one of them spends the single-use refresh
// token; the others pick up the rotated login it wrote back.
func TestResolveAcrossProcesses(t *testing.T) {
	home := t.TempDir()
	writeCredFile(t, home, map[string]any{
		"accessToken": "cc-old", "refreshToken": "r0",
		"expiresAt": time.Now().Add(-time.Hour).UnixMilli(),
	})
	srv := oauthtest.New("r0", 300*time.Millisecond, nil)
	defer srv.Close()

	const procs = 3
	outs := make([]string, procs)
	var wg sync.WaitGroup
	for i := range procs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := exec.Command(os.Args[0], "-test.run=^TestHelperResolveProcess$", "-test.count=1")
			cmd.Env = append(os.Environ(), "HOME="+home, "MEMCODE_HOME="+filepath.Join(home, ".memcode"),
				"CLAUDESUB_HELPER_REFRESH_URL="+srv.URL)
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

// Refresh succeeded but the write-back failed (crash / unwritable store). The
// same process keeps working from the in-memory rotated login without
// replaying the spent token; a fresh process that only sees the spent token
// gets one clear "sign in again" error, not a retry loop.
func TestRefreshWriteBackFailureRecovery(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("MEMCODE_HOME", filepath.Join(home, ".memcode"))
	writeCredFile(t, home, map[string]any{
		"accessToken": "cc-old", "refreshToken": "r0",
		"expiresAt": time.Now().Add(-time.Hour).UnixMilli(),
	})
	dir := filepath.Join(home, ".claude")
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)
	defer func() { unsaved.spent, unsaved.c = "", creds{} }()
	srv := oauthtest.New("r0", 0, nil)
	defer srv.Close()
	orig := refreshURL
	refreshURL = srv.URL
	defer func() { refreshURL = orig }()

	c, err := resolve("")
	if c.AccessToken != "access-1" || err == nil || !strings.Contains(err.Error(), "couldn't save") {
		t.Fatalf("first resolve = %q, %v; want the refreshed token plus a save warning", c.AccessToken, err)
	}
	c, _ = resolve("access-1") // the API rejects it: refresh from the in-memory login (still unsaved)
	if c.AccessToken != "access-2" {
		t.Fatalf("second resolve = %q; want a refresh from the unsaved rotated token", c.AccessToken)
	}
	if srv.Rejected() != 0 {
		t.Fatalf("the spent refresh token was replayed")
	}

	unsaved.spent, unsaved.c = "", creds{} // a new process: only the stale store
	_, err = resolve("")
	if err == nil || !strings.Contains(err.Error(), "sign in again with Claude Code") {
		t.Fatalf("stale-store resolve err = %v, want a clear sign-in-again error", err)
	}
	if srv.Rejected() != 1 {
		t.Fatalf("rejected = %d, want exactly one attempt (no loop)", srv.Rejected())
	}
}
