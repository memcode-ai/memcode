//go:build !membench

package membench

import "errors"

// LegacyAdapter is the stub that stands in when the binary is built WITHOUT
// `-tags membench`: the real pre-2026-07-25 substring scan (legacy.go) is
// bench-only history and must not ship in release binaries. The type keeps the
// exported surface (Adapters, cmd/bench) compiling; running it says why it
// isn't available instead of silently benching nothing.
type LegacyAdapter struct{}

func (LegacyAdapter) Name() string { return "legacy" }

func (LegacyAdapter) Rank(root string, q Question, docs []SessionDoc, k int) ([]string, error) {
	return nil, errors.New("legacy adapter is bench-only history — rebuild with -tags membench to run it")
}
