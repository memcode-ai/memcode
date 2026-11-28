package filelock

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// A second Acquire (separate descriptor, as another process would have) waits
// while the first holds the lock, times out with the context, and succeeds
// once the holder releases.
func TestAcquireExcludesAndTimesOut(t *testing.T) {
	p := filepath.Join(t.TempDir(), "locks", "x.lock")
	l1, err := Acquire(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := Acquire(ctx, p); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second Acquire while held = %v, want deadline exceeded", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("bounded wait took %v", time.Since(start))
	}
	done := make(chan error, 1)
	go func() {
		l2, err := Acquire(context.Background(), p)
		if err == nil {
			err = l2.Release()
		}
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	if err := l1.Release(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Acquire did not proceed after Release")
	}
	if err := l1.Release(); err != nil {
		t.Fatalf("double Release: %v", err)
	}
}

// TestHelperHoldLock is the subprocess for TestKilledHolderReleases: it takes
// the lock, says so, and then hangs until killed.
func TestHelperHoldLock(t *testing.T) {
	p := os.Getenv("FILELOCK_HELPER_PATH")
	if p == "" {
		t.Skip("helper process only")
	}
	if _, err := Acquire(context.Background(), p); err != nil {
		os.Stdout.WriteString("ERR " + err.Error() + "\n")
		os.Exit(1)
	}
	os.Stdout.WriteString("LOCKED\n")
	time.Sleep(time.Minute)
	os.Exit(0)
}

// A process SIGKILLed while holding the lock leaves nothing stuck: the OS
// drops it and the next Acquire proceeds.
func TestKilledHolderReleases(t *testing.T) {
	p := filepath.Join(t.TempDir(), "held.lock")
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperHoldLock$", "-test.count=1")
	cmd.Env = append(os.Environ(), "FILELOCK_HELPER_PATH="+p)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, _ := bufio.NewReader(out).ReadString('\n')
	if line != "LOCKED\n" {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("helper: %q", line)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	if _, err := Acquire(ctx, p); err == nil {
		cancel()
		t.Fatal("lock not held by the helper process")
	}
	cancel()
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	l, err := Acquire(ctx, p)
	if err != nil {
		t.Fatalf("Acquire after holder was killed: %v", err)
	}
	_ = l.Release()
}
