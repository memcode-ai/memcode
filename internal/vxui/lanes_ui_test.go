package vxui

import (
	"testing"

	"github.com/memcode-ai/memcode/internal/provider"
)

// The hybrid contract for the picker: Endpoint() is ok only in EXCLUSIVE
// endpoint mode, so resolveEndpointModel (which force-pins the endpoint's
// first model at boot) can never clobber Automatic in a lane session. This
// pins the provider-side guarantee the UI depends on.
func TestLanesNeverLookLikeEndpointMode(t *testing.T) {
	t.Setenv(provider.EnvCredentials, "claude")
	t.Setenv(provider.EnvCredentialSource, "")
	t.Setenv("MEMCODE_API_TOKEN", "memcode_test")
	t.Setenv("MEMCODE_ENDPOINT_URL", "")
	l := provider.NewFromEnvLazy()
	if _, ok := l.Endpoint(); ok {
		t.Fatal("hybrid session reports exclusive endpoint mode — the picker would clobber Automatic")
	}
	if !l.Connected() {
		t.Fatal("hybrid session must report connected")
	}
}
