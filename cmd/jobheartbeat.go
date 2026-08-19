package cmd

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/memcode-ai/memcode/internal/agent/runtime"
	"github.com/memcode-ai/memcode/internal/jobs"
)

// startJobHeartbeat wires the session's tool-notify tap into a ~1s meta.json
// heartbeat for job id, so frontends can show what this detached agent is
// doing right now (activity · tokens). The returned stop func is SYNCHRONOUS:
// it detaches the tap, stops the ticker goroutine, and blocks until that
// goroutine has fully exited — no heartbeat write can occur after stop()
// returns. Callers run stop() before jobs.Finish, so the terminal record can
// never be trampled by a late tick.
func startJobHeartbeat(ctx context.Context, sess *runtime.Session, root, id string) (stop func()) {
	var act atomic.Value
	act.Store("starting…")
	sess.SetToolNotify(func(label string) { act.Store(label) })

	hbCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	beat := func() {
		in, out := sess.Tokens()
		_ = jobs.Heartbeat(root, id, act.Load().(string), int64(in), int64(out))
	}
	go func() {
		defer close(done)
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-t.C:
				beat()
			}
		}
	}()
	return func() {
		sess.SetToolNotify(nil)
		cancel()
		<-done
		beat() // one final flush — Heartbeat itself refuses non-running jobs
	}
}
