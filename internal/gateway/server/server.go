// Package server is the memcode gateway runtime — the first external surface of
// memcode's event/objective/agent spine. It starts each configured channel,
// receives inbound messages, runs each as a detached agent job (crash-isolated
// subprocess, reusing internal/jobs), and posts the result back to the
// originating channel. Coding is one use of this loop, not what it's built
// around: an inbound message is just a task, whatever the task is.
package server

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/channels"
	"github.com/memcode-ai/memcode/internal/channels/telegram"
	"github.com/memcode-ai/memcode/internal/jobs"
)

// Config configures a gateway run.
type Config struct {
	Root string // project root the agent operates in
}

// Run starts every configured channel and blocks until ctx is cancelled,
// returning ctx.Err(). It fails fast if no channel is configured.
func Run(ctx context.Context, cfg Config, out io.Writer) error {
	chs := enabledChannels()
	if len(chs) == 0 {
		return fmt.Errorf("no channels configured — set a bot token in the global .env (e.g. MEMCODE_TELEGRAM_BOT_TOKEN)")
	}

	byName := make(map[string]channels.Channel, len(chs))
	inbound := make(chan channels.Inbound, 64)
	for _, ch := range chs {
		byName[ch.Name()] = ch
		ch := ch
		go func() {
			if err := ch.Start(ctx, inbound); err != nil && ctx.Err() == nil {
				fmt.Fprintf(out, "gateway: channel %s stopped: %v\n", ch.Name(), err)
			}
		}()
		fmt.Fprintf(out, "gateway: %s listening\n", ch.Name())
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case inb := <-inbound:
			go handle(ctx, cfg.Root, byName[inb.Channel], inb, out)
		}
	}
}

// enabledChannels builds a channel per configured token. v1: Telegram only;
// the environment is already populated from the global .env by the caller.
func enabledChannels() []channels.Channel {
	var chs []channels.Channel
	if tok := strings.TrimSpace(os.Getenv("MEMCODE_TELEGRAM_BOT_TOKEN")); tok != "" {
		chs = append(chs, telegram.New(tok))
	}
	return chs
}

// handle runs one inbound message as a detached agent job and posts the result
// back to its channel. Jobs are subprocesses (a hung/panicking run can't wedge
// the gateway or other channels); we poll to completion. Failures are reported
// to the user, never silently dropped.
func handle(ctx context.Context, root string, ch channels.Channel, inb channels.Inbound, out io.Writer) {
	if ch == nil {
		return
	}
	// A gateway-triggered job has no TTY to answer approval prompts → Auto mode.
	job, err := jobs.Spawn(root, inb.Text, string(permissions.ModeAuto), "", false, true)
	if err != nil {
		_ = ch.Send(ctx, inb.Conversation, channels.Outbound{Text: "Couldn't start that: " + err.Error()})
		return
	}
	fmt.Fprintf(out, "gateway: [%s] job %s ← %q\n", inb.Channel, job.ID, truncate(inb.Text, 60))

	reply := waitForJob(ctx, root, job.ID)
	if strings.TrimSpace(reply) == "" {
		reply = "Done."
	}
	if err := ch.Send(ctx, inb.Conversation, channels.Outbound{Text: reply}); err != nil {
		fmt.Fprintf(out, "gateway: reply to %s failed: %v\n", inb.Channel, err)
	}
}

// waitForJob polls until the job leaves the running state and returns text to
// post back: the agent's result on success, or a pointer to the log on failure.
func waitForJob(ctx context.Context, root, id string) string {
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return "Interrupted before it finished."
		case <-tick.C:
			j, err := jobs.Get(root, id)
			if err != nil {
				return "Lost track of the job: " + err.Error()
			}
			switch j.Status {
			case jobs.StatusDone:
				return j.Result
			case jobs.StatusFailed, jobs.StatusStopped:
				if strings.TrimSpace(j.Result) != "" {
					return j.Result
				}
				return fmt.Sprintf("That task didn't complete (%s). Details in .memcode/jobs/%s/log", j.Status, id)
			}
		}
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
