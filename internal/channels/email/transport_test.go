package email

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"
)

// silentServer accepts connections and never says a word: the shape of a hung
// mail server that a timeout-less client would wait on forever.
func silentServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			c.Close()
		}
	})
	return ln.Addr().String()
}

func TestSMTPSendHonorsContext(t *testing.T) {
	c := New("me@example.com", "pw", "imap.example.com", silentServer(t), 0, "", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := c.smtpSend(ctx, "you@example.com", []byte("hi")); err == nil {
		t.Fatal("send to a silent server succeeded")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("smtp send ignored ctx: took %v", d)
	}
}

func TestIMAPDialHonorsContext(t *testing.T) {
	c := New("me@example.com", "pw", silentServer(t), "smtp.example.com", 0, "", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := c.dialIMAP(ctx); err == nil {
		t.Fatal("dial to a silent server succeeded")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("imap dial ignored ctx: took %v", d)
	}
}
