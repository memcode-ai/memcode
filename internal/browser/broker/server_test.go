package broker

import (
	"path/filepath"
	"testing"
	"time"
)

// TestServerClientRoundTrip exercises the exact cross-process path a
// delegated worker uses: a Client talking over the Unix socket to a Server
// wrapping the gateway's *Broker*, not the in-process Broker methods
// directly. This is what makes existing-Chrome coordination possible across
// separate OS processes.
func TestServerClientRoundTrip(t *testing.T) {
	b := New()
	sock := filepath.Join(t.TempDir(), "broker.sock")
	srv, err := Serve(b, sock)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	c := NewClient(sock)
	if !c.Reachable() {
		t.Fatal("expected socket to be reachable")
	}

	lease, err := c.Acquire("agent-1", "run-1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if lease.Token == "" {
		t.Fatal("expected a lease token")
	}

	// A second, concurrent acquire must fail — exactly one worker may hold
	// existing-Chrome mutation rights at a time.
	if _, err := c.Acquire("agent-2", "run-2", time.Minute); err == nil {
		t.Fatal("expected concurrent acquire to be rejected")
	}

	if err := c.OwnPage(lease.Token, "tab-1"); err != nil {
		t.Fatal(err)
	}
	if !c.CanMutate(lease.Token, "tab-1") {
		t.Fatal("expected CanMutate to be true for the owning lease")
	}
	if c.CanMutate("wrong-token", "tab-1") {
		t.Fatal("expected CanMutate to be false for a wrong token")
	}

	if err := c.Release(lease.Token); err != nil {
		t.Fatal(err)
	}
	// Released: a new run may now acquire.
	if _, err := c.Acquire("agent-2", "run-2", time.Minute); err != nil {
		t.Fatalf("expected acquire after release to succeed: %v", err)
	}
}

func TestClientNotReachableWhenNoServer(t *testing.T) {
	c := NewClient(filepath.Join(t.TempDir(), "nonexistent.sock"))
	if c.Reachable() {
		t.Fatal("expected an unreachable socket to report not reachable")
	}
	if _, err := c.Acquire("a", "r", time.Minute); err == nil {
		t.Fatal("expected Acquire to fail closed when the broker isn't running")
	}
}
