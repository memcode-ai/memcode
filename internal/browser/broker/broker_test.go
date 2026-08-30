package broker

import (
	"testing"
	"time"
)

func TestLeaseAuthenticationOwnershipAndRelease(t *testing.T) {
	b := New()
	l, err := b.Acquire("agent", "run", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !b.Authenticate(l.Token) {
		t.Fatal("valid token denied")
	}
	if b.CanMutate(l.Token, "existing-user-tab") {
		t.Fatal("unowned tab allowed")
	}
	if err := b.OwnPage(l.Token, "owned-tab"); err != nil {
		t.Fatal(err)
	}
	if !b.CanMutate(l.Token, "owned-tab") {
		t.Fatal("owned tab denied")
	}
	if _, err := b.Acquire("other", "run", time.Minute); err == nil {
		t.Fatal("concurrent lease accepted")
	}
	if !b.Release(l.Token) || b.Authenticate(l.Token) {
		t.Fatal("lease not released")
	}
}
func TestHeaderRedaction(t *testing.T) {
	got := RedactHeaders(map[string]string{"Authorization": "secret", "Cookie": "session", "Accept": "json"})
	if got["Authorization"] != "[redacted]" || got["Cookie"] != "[redacted]" || got["Accept"] != "json" {
		t.Fatalf("headers=%v", got)
	}
}
