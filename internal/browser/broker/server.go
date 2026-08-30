package broker

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	gwconfig "github.com/memcode-ai/memcode/internal/gateway/config"
)

// SocketPath is the well-known location of the gateway-owned existing-Chrome
// broker socket — shared between the gateway (which Serves it) and any
// process that dials it as a Client, including a autonomous agent's delegated
// worker running as a standalone `memcode run` job, not just inside the
// gateway. Its absence (no gateway running) is exactly the fail-closed signal
// existing-Chrome delegation must respect — see ErrNotConnected.
func SocketPath() (string, error) {
	dir, err := gwconfig.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "browser-broker.sock"), nil
}

// Server exposes a Broker over a permission-protected local Unix socket, so a
// process OTHER than the one holding the *Broker* (a delegated worker, a
// separate OS process spawned via jobs.SpawnWithSpec) can Acquire/Release/
// OwnPage/CanMutate against the SAME broker instance the gateway owns. The
// broker itself must stay a single, long-lived, in-process object — cloning
// it per connection would defeat its whole purpose (one lease, one owner, at
// a time, for the user's ONE real Chrome).
//
// The socket is created with 0600 permissions inside a 0700 directory (see
// gwconfig.Dir), so only the user who started the gateway can reach it —
// that ownership check is the "permission-protected" half of the design
// doc's "gateway-owned broker and permission-protected local socket".
type Server struct {
	broker   *Broker
	listener net.Listener
	http     *http.Server
}

// Serve starts listening on socketPath (removing any stale socket file left
// by a prior crashed gateway) and returns once the listener is up; Close
// stops it. b is the SAME *Broker instance the gateway's own in-process
// callers (if any) use — there is exactly one broker per gateway process.
func Serve(b *Broker, socketPath string) (*Server, error) {
	_ = os.Remove(socketPath) // stale socket from a prior process; a live one would fail to bind anyway
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(socketPath, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	mux := http.NewServeMux()
	s := &Server{broker: b, listener: ln}
	mux.HandleFunc("/acquire", s.handleAcquire)
	mux.HandleFunc("/release", s.handleRelease)
	mux.HandleFunc("/own_page", s.handleOwnPage)
	mux.HandleFunc("/can_mutate", s.handleCanMutate)
	s.http = &http.Server{Handler: mux}
	go func() { _ = s.http.Serve(ln) }()
	return s, nil
}

func (s *Server) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := s.http.Shutdown(ctx)
	_ = os.Remove(s.listener.Addr().String())
	return err
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) handleAcquire(w http.ResponseWriter, r *http.Request) {
	var in struct {
		AgentID, RunID string
		TTLSeconds     int
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	ttl := time.Duration(in.TTLSeconds) * time.Second
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	lease, err := s.broker.Acquire(in.AgentID, in.RunID, ttl)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, lease)
}

func (s *Server) handleRelease(w http.ResponseWriter, r *http.Request) {
	var in struct{ Token string }
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"released": s.broker.Release(in.Token)})
}

func (s *Server) handleOwnPage(w http.ResponseWriter, r *http.Request) {
	var in struct{ Token, Page string }
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := s.broker.OwnPage(in.Token, in.Page); err != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"owned": true})
}

func (s *Server) handleCanMutate(w http.ResponseWriter, r *http.Request) {
	token, page := r.URL.Query().Get("token"), r.URL.Query().Get("page")
	writeJSON(w, http.StatusOK, map[string]bool{"can_mutate": s.broker.CanMutate(token, page)})
}

// ErrNotConnected is returned by a Client call when the socket itself is
// unreachable (no gateway running, or existing-Chrome never set up) — the
// caller's job is to fail closed on this, never to fall back to ephemeral.
var ErrNotConnected = errors.New("browser broker not reachable — is the gateway running with existing-Chrome configured?")
