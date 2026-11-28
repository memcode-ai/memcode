package mcp

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func shortTimeouts(t *testing.T, connect, authorize time.Duration) {
	t.Helper()
	oc, oa := connectTimeout, authorizeTimeout
	connectTimeout, authorizeTimeout = connect, authorize
	t.Cleanup(func() { connectTimeout, authorizeTimeout = oc, oa })
}

// A browser authorization that outlasts the connect budget is NOT cut off by it: the
// dial switches to the authorization budget while the flow runs.
func TestAwaitConnectAuthorizationGetsItsOwnBudget(t *testing.T) {
	shortTimeouts(t, 100*time.Millisecond, 2*time.Second)
	ch := make(chan dialResult, 1)
	events := make(chan bool, 8)
	var stopped atomic.Bool
	go func() {
		events <- true
		time.Sleep(400 * time.Millisecond) // the person signs in: 4x the connect budget
		events <- false
		ch <- dialResult{}
	}()
	if _, err := awaitConnect(context.Background(), ch, events, func() { stopped.Store(true) }); err != nil {
		t.Fatalf("authorization within its budget failed: %v", err)
	}
	if stopped.Load() {
		t.Fatal("successful dial was stopped")
	}
}

// The authorization budget is still bounded, and says what happened.
func TestAwaitConnectAuthorizationTimesOut(t *testing.T) {
	shortTimeouts(t, 50*time.Millisecond, 150*time.Millisecond)
	events := make(chan bool, 1)
	events <- true
	var stopped atomic.Bool
	_, err := awaitConnect(context.Background(), make(chan dialResult), events, func() { stopped.Store(true) })
	if err == nil || !strings.Contains(err.Error(), "authorization was not completed") || !stopped.Load() {
		t.Fatalf("err = %v stopped=%v, want a stopped authorization timeout", err, stopped.Load())
	}
}

// Without an authorization, the plain connect budget applies; ctx cancels at any time.
func TestAwaitConnectBoundedAndCancellable(t *testing.T) {
	shortTimeouts(t, 50*time.Millisecond, time.Minute)
	if _, err := awaitConnect(context.Background(), make(chan dialResult), nil, func() {}); err == nil || !strings.Contains(err.Error(), "connect timed out") {
		t.Fatalf("hung connect err = %v", err)
	}
	shortTimeouts(t, time.Minute, time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan bool, 1)
	events <- true
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	if _, err := awaitConnect(ctx, make(chan dialResult), events, func() {}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled authorization err = %v, want context.Canceled", err)
	}
}
