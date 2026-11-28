package vxui

import "sync"

// spinnerTok identifies one running turn-spinner goroutine. At most one exists per
// appState: startSpinner claims the slot and the goroutine releases it when it stops,
// so chained turns (each calling startSpinner) never stack extra tickers.
type spinnerTok struct{}

var (
	spinnerMu    sync.Mutex
	spinnerOwner = map[*appState]*spinnerTok{}
)

// claimSpinner returns a token when no spinner is running for s, else nil.
func claimSpinner(s *appState) *spinnerTok {
	spinnerMu.Lock()
	defer spinnerMu.Unlock()
	if spinnerOwner[s] != nil {
		return nil
	}
	t := &spinnerTok{}
	spinnerOwner[s] = t
	return t
}

// releaseSpinner frees s's slot if tok still holds it (a stale release from an
// already-replaced spinner is a no-op).
func releaseSpinner(s *appState, tok *spinnerTok) {
	spinnerMu.Lock()
	defer spinnerMu.Unlock()
	if spinnerOwner[s] == tok {
		delete(spinnerOwner, s)
	}
}

// spinnerRunning reports whether a spinner goroutine holds s's slot (tests).
func spinnerRunning(s *appState) bool {
	spinnerMu.Lock()
	defer spinnerMu.Unlock()
	return spinnerOwner[s] != nil
}
