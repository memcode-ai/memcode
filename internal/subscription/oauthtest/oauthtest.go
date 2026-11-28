// Package oauthtest is a test double for an OAuth token endpoint whose refresh
// tokens are single-use and rotate, like the real vendors': a refresh token
// works exactly once, and replaying it is rejected with 400 invalid_grant. It
// counts refreshes so tests can assert that concurrent resolvers (goroutines
// or separate processes) spend a login exactly once.
package oauthtest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"
)

// Server is a rotating-refresh-token endpoint.
type Server struct {
	*httptest.Server

	mu       sync.Mutex
	valid    map[string]bool
	n        int
	refresh  int
	rejected int

	delay  time.Duration
	access func(n int) string
}

// New starts a server that accepts initialRefresh once. delay stalls each
// successful exchange (widening a race window); access mints the access token
// for the n-th refresh (1-based), nil meaning "access-<n>".
func New(initialRefresh string, delay time.Duration, access func(n int) string) *Server {
	if access == nil {
		access = func(n int) string { return fmt.Sprintf("access-%d", n) }
	}
	s := &Server{valid: map[string]bool{initialRefresh: true}, delay: delay, access: access}
	s.Server = httptest.NewServer(http.HandlerFunc(s.handle))
	return s
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	rt := r.Form.Get("refresh_token")
	s.mu.Lock()
	if r.Form.Get("grant_type") != "refresh_token" || !s.valid[rt] {
		s.rejected++
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"refresh token already used"}`))
		return
	}
	delete(s.valid, rt)
	s.n++
	s.refresh++
	n := s.n
	next := fmt.Sprintf("refresh-%d", n)
	s.valid[next] = true
	access := s.access(n)
	s.mu.Unlock()
	if s.delay > 0 {
		time.Sleep(s.delay)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token": access, "refresh_token": next, "expires_in": 6 * 3600,
	})
}

// Refreshes is the number of successful refresh exchanges.
func (s *Server) Refreshes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refresh
}

// Rejected is the number of refused (replayed or unknown) refresh tokens.
func (s *Server) Rejected() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rejected
}
