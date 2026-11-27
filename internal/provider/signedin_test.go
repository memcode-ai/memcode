package provider

import "testing"

// SignedIn tracks the memcode gateway connection only: true after /login's
// SetCredentials, false after /logout's ClearCredentials, regardless of any
// subscription lane that keeps the provider Connected.
func TestLazySignedIn(t *testing.T) {
	l := &Lazy{}
	if l.SignedIn() {
		t.Fatal("a provider with no gateway client must not be signed in")
	}
	l.SetCredentials("http://127.0.0.1:1", "memcode_test")
	if !l.SignedIn() {
		t.Fatal("SetCredentials must sign in")
	}
	l.ClearCredentials()
	if l.SignedIn() {
		t.Fatal("ClearCredentials must sign out")
	}
}
