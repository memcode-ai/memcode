package broker

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

type Lease struct {
	ID, AgentID, RunID, Token string
	ExpiresAt                 time.Time
	OwnedPages                map[string]bool
}
type Broker struct {
	mu    sync.Mutex
	lease *Lease
}

func New() *Broker { return &Broker{} }
func (b *Broker) Acquire(agentID, runID string, ttl time.Duration) (Lease, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.lease != nil && time.Now().Before(b.lease.ExpiresAt) {
		return Lease{}, fmt.Errorf("browser control is leased to another run")
	}
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return Lease{}, err
	}
	l := Lease{ID: hex.EncodeToString(raw[:8]), AgentID: agentID, RunID: runID, Token: hex.EncodeToString(raw), ExpiresAt: time.Now().Add(ttl), OwnedPages: map[string]bool{}}
	b.lease = &l
	return l, nil
}
func (b *Broker) Authenticate(token string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lease != nil && time.Now().Before(b.lease.ExpiresAt) && token == b.lease.Token
}
func (b *Broker) OwnPage(token, page string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.lease == nil || token != b.lease.Token || !time.Now().Before(b.lease.ExpiresAt) {
		return fmt.Errorf("invalid browser lease")
	}
	b.lease.OwnedPages[page] = true
	return nil
}
func (b *Broker) CanMutate(token, page string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lease != nil && token == b.lease.Token && time.Now().Before(b.lease.ExpiresAt) && b.lease.OwnedPages[page]
}
func (b *Broker) Release(token string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.lease == nil || token != b.lease.Token {
		return false
	}
	b.lease = nil
	return true
}
func RedactHeaders(headers map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range headers {
		switch k {
		case "Authorization", "authorization", "Cookie", "cookie", "Set-Cookie", "set-cookie":
			out[k] = "[redacted]"
		default:
			out[k] = v
		}
	}
	return out
}
