package github

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/memcode-ai/memcode/internal/channels"
)

func sign(secret, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestVerifySignature(t *testing.T) {
	body := []byte(`{"hello":"world"}`)
	good := sign("s3cr3t", string(body))
	if !verifySignature([]byte("s3cr3t"), good, body) {
		t.Error("valid signature rejected")
	}
	if verifySignature([]byte("wrong"), good, body) {
		t.Error("signature verified under wrong secret")
	}
	if verifySignature([]byte("s3cr3t"), "sha256=deadbeef", body) {
		t.Error("bad hex accepted")
	}
	if verifySignature([]byte("s3cr3t"), "", body) {
		t.Error("empty header accepted")
	}
	if verifySignature(nil, good, body) {
		t.Error("empty secret accepted")
	}
}

func TestParseReplyTo(t *testing.T) {
	for _, tt := range []struct {
		in                string
		wantCh, wantConvo string
		wantOK            bool
	}{
		{"telegram:123456", "telegram", "123456", true},
		{" telegram : 123 ", "telegram", "123", true},
		{"telegram:", "", "", false},
		{":123", "", "", false},
		{"nope", "", "", false},
		{"", "", "", false},
	} {
		ch, convo, ok := parseReplyTo(tt.in)
		if ok != tt.wantOK || ch != tt.wantCh || convo != tt.wantConvo {
			t.Errorf("parseReplyTo(%q) = (%q,%q,%v), want (%q,%q,%v)", tt.in, ch, convo, ok, tt.wantCh, tt.wantConvo, tt.wantOK)
		}
	}
}

func mkRun(action, conclusion, branch, sender string) string {
	var p workflowRun
	p.Action = action
	p.WorkflowRun.Name = "CI"
	p.WorkflowRun.Conclusion = conclusion
	p.WorkflowRun.HeadBranch = branch
	p.WorkflowRun.HTMLURL = "https://github.com/o/r/actions/runs/1"
	p.Repository.FullName = "o/r"
	p.Sender.Login = sender
	b, _ := json.Marshal(p)
	return string(b)
}

func TestTaskFromEvent(t *testing.T) {
	if _, ok := taskFromEvent("push", []byte(`{}`)); ok {
		t.Error("non-workflow_run event acted on")
	}
	if _, ok := taskFromEvent("workflow_run", []byte(mkRun("completed", "success", "main", "alice"))); ok {
		t.Error("successful run acted on")
	}
	if _, ok := taskFromEvent("workflow_run", []byte(mkRun("requested", "failure", "main", "alice"))); ok {
		t.Error("non-completed action acted on")
	}
	if _, ok := taskFromEvent("workflow_run", []byte(mkRun("completed", "failure", "memcode/fix-1", "alice"))); ok {
		t.Error("memcode/* branch acted on (loop risk)")
	}
	if _, ok := taskFromEvent("workflow_run", []byte(mkRun("completed", "failure", "main", "memcode[bot]"))); ok {
		t.Error("memcode bot actor acted on (loop risk)")
	}
	task, ok := taskFromEvent("workflow_run", []byte(mkRun("completed", "failure", "main", "alice")))
	if !ok {
		t.Fatal("failing run on main not acted on")
	}
	if !strings.Contains(task, "o/r") || !strings.Contains(task, "main") {
		t.Errorf("task missing context: %q", task)
	}
}

func TestDedup(t *testing.T) {
	d := newDedup(2)
	if d.seenBefore("a") {
		t.Error("first sighting reported as seen")
	}
	if !d.seenBefore("a") {
		t.Error("second sighting not reported as seen")
	}
	d.seenBefore("b")
	d.seenBefore("c") // evicts "a"
	if d.seenBefore("a") {
		t.Error("evicted id still reported as seen")
	}
}

func TestHandler(t *testing.T) {
	secret := "s3cr3t"
	tr := New(secret, "telegram:42")
	inbound := make(chan channels.Inbound, 1)
	h := tr.Handler(inbound)

	body := mkRun("completed", "failure", "main", "alice")
	post := func(sig, delivery, event, b string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/webhook/github", strings.NewReader(b))
		req.Header.Set("X-Hub-Signature-256", sig)
		req.Header.Set("X-GitHub-Delivery", delivery)
		req.Header.Set("X-GitHub-Event", event)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}

	// Bad signature → 401, nothing forwarded.
	if rr := post("sha256=00", "d1", "workflow_run", body); rr.Code != http.StatusUnauthorized {
		t.Fatalf("bad sig: got %d", rr.Code)
	}
	// Good delivery → 202 and an Inbound routed to telegram:42.
	if rr := post(sign(secret, body), "d2", "workflow_run", body); rr.Code != http.StatusAccepted {
		t.Fatalf("good delivery: got %d", rr.Code)
	}
	select {
	case inb := <-inbound:
		if inb.Channel != "telegram" || inb.Conversation != "42" {
			t.Errorf("routed to %s:%s, want telegram:42", inb.Channel, inb.Conversation)
		}
	default:
		t.Fatal("no inbound forwarded")
	}
	// Duplicate delivery id → 200 and NOT forwarded again.
	if rr := post(sign(secret, body), "d2", "workflow_run", body); rr.Code != http.StatusOK {
		t.Fatalf("dup delivery: got %d", rr.Code)
	}
	select {
	case <-inbound:
		t.Fatal("duplicate delivery forwarded")
	default:
	}
}
