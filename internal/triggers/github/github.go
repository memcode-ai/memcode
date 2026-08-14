// Package github is the gateway's GitHub trigger: an inbound webhook receiver,
// not a chat channel. GitHub is an event SOURCE — a failing CI run becomes an
// agent task, and the result is routed to a chat conversation the user
// configured (ReplyTo, e.g. "telegram:123456"). Deliveries are authenticated by
// HMAC-SHA256 over the raw body, de-duplicated on the X-GitHub-Delivery id, and
// filtered so memcode's own bot/branches never trigger a loop.
package github

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/memcode-ai/memcode/internal/channels"
)

// maxBody caps the webhook payload we read (GitHub payloads are well under this).
const maxBody = 2 << 20 // 2 MiB

// Trigger handles GitHub webhook deliveries.
type Trigger struct {
	secret  []byte
	replyTo string // "<channel>:<conversation>", where the result is posted
	dedup   *dedup
}

// New builds a GitHub trigger. secret verifies delivery signatures; replyTo
// names the chat conversation the agent's result is routed to.
func New(secret, replyTo string) *Trigger {
	return &Trigger{secret: []byte(secret), replyTo: strings.TrimSpace(replyTo), dedup: newDedup(2048)}
}

// Handler returns the webhook HTTP handler. It validates the signature, drops
// duplicates and events we don't act on, and forwards actionable events as an
// Inbound routed to the configured reply conversation.
func (t *Trigger) Handler(inbound chan<- channels.Inbound) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
		if err != nil {
			http.Error(w, "read error", http.StatusBadRequest)
			return
		}
		if !verifySignature(t.secret, r.Header.Get("X-Hub-Signature-256"), body) {
			http.Error(w, "bad signature", http.StatusUnauthorized)
			return
		}
		delivery := r.Header.Get("X-GitHub-Delivery")
		if delivery != "" && t.dedup.seenBefore(delivery) {
			w.WriteHeader(http.StatusOK) // already processed — ack and ignore
			return
		}

		ch, convo, ok := parseReplyTo(t.replyTo)
		if !ok {
			// No route configured; acknowledge so GitHub doesn't retry.
			w.WriteHeader(http.StatusAccepted)
			return
		}
		task, ok := taskFromEvent(r.Header.Get("X-GitHub-Event"), body)
		if !ok {
			w.WriteHeader(http.StatusNoContent) // not an event we act on
			return
		}

		// MessageID carries the delivery id so the router's durable dedup also
		// guards against re-runs across a restart (the in-memory dedup above does
		// not survive one — hardened separately).
		inb := channels.Inbound{Channel: ch, Conversation: convo, Principal: "github", Text: task, MessageID: "github:" + delivery}
		select {
		case inbound <- inb:
			w.WriteHeader(http.StatusAccepted)
		case <-r.Context().Done():
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	})
}

// verifySignature checks GitHub's "sha256=<hex>" HMAC header against the body.
func verifySignature(secret []byte, header string, body []byte) bool {
	if len(secret) == 0 {
		return false
	}
	want, ok := strings.CutPrefix(header, "sha256=")
	if !ok {
		return false
	}
	wantMAC, err := hex.DecodeString(want)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hmac.Equal(wantMAC, mac.Sum(nil))
}

// parseReplyTo splits "telegram:123456" into channel and conversation.
func parseReplyTo(s string) (channel, conversation string, ok bool) {
	channel, conversation, ok = strings.Cut(s, ":")
	channel, conversation = strings.TrimSpace(channel), strings.TrimSpace(conversation)
	if channel == "" || conversation == "" {
		return "", "", false
	}
	return channel, conversation, true
}

// workflowRun is the subset of a workflow_run payload we read.
type workflowRun struct {
	Action      string `json:"action"`
	WorkflowRun struct {
		Name       string `json:"name"`
		Conclusion string `json:"conclusion"`
		HTMLURL    string `json:"html_url"`
		HeadBranch string `json:"head_branch"`
	} `json:"workflow_run"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	Sender struct {
		Login string `json:"login"`
	} `json:"sender"`
}

// taskFromEvent turns an actionable GitHub event into an agent task, or ok=false
// if the event isn't one we act on (or originates from memcode itself). v1 acts
// on a completed workflow_run that failed.
func taskFromEvent(event string, body []byte) (string, bool) {
	if event != "workflow_run" {
		return "", false
	}
	var p workflowRun
	if err := json.Unmarshal(body, &p); err != nil {
		return "", false
	}
	if p.Action != "completed" || p.WorkflowRun.Conclusion != "failure" {
		return "", false
	}
	if isMemcodeActor(p.Sender.Login) || strings.HasPrefix(p.WorkflowRun.HeadBranch, "memcode/") {
		return "", false // don't act on our own bot or fix branches — avoids loops
	}
	task := fmt.Sprintf(
		"GitHub CI failed: workflow %q failed on %s (branch %s). Investigate the failure and propose a fix.",
		p.WorkflowRun.Name, p.Repository.FullName, p.WorkflowRun.HeadBranch,
	)
	if p.WorkflowRun.HTMLURL != "" {
		task += "\n" + p.WorkflowRun.HTMLURL
	}
	return task, true
}

func isMemcodeActor(login string) bool {
	l := strings.ToLower(login)
	return l == "memcode[bot]" || l == "memcode"
}

// dedup is a bounded set of recently-seen delivery ids.
type dedup struct {
	mu    sync.Mutex
	seen  map[string]struct{}
	order []string
	cap   int
}

func newDedup(capacity int) *dedup {
	return &dedup{seen: make(map[string]struct{}, capacity), cap: capacity}
}

// seenBefore records id and reports whether it had already been seen.
func (d *dedup) seenBefore(id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.seen[id]; ok {
		return true
	}
	if len(d.order) >= d.cap {
		oldest := d.order[0]
		d.order = d.order[1:]
		delete(d.seen, oldest)
	}
	d.seen[id] = struct{}{}
	d.order = append(d.order, id)
	return false
}
