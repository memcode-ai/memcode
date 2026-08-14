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
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/channels"
	"github.com/memcode-ai/memcode/internal/channels/discord"
	"github.com/memcode-ai/memcode/internal/channels/slack"
	"github.com/memcode-ai/memcode/internal/channels/telegram"
	gwconfig "github.com/memcode-ai/memcode/internal/gateway/config"
	"github.com/memcode-ai/memcode/internal/jobs"
	githubtrigger "github.com/memcode-ai/memcode/internal/triggers/github"
)

// defaultWebhookAddr is where the inbound webhook server listens when a
// webhook-driven trigger (GitHub, later WhatsApp) is enabled but no address is set.
const defaultWebhookAddr = ":8787"

// Run starts every configured surface — chat channels and inbound webhook
// triggers — and blocks until ctx is cancelled, returning ctx.Err(). It fails
// fast if nothing is configured. root is the project the agent operates in;
// settings holds the non-secret gateway config (secrets come from the
// environment, loaded from the global .env upstream).
func Run(ctx context.Context, root string, settings gwconfig.Settings, out io.Writer) error {
	chs := channelsFrom(settings, out)

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

	webhooks := startWebhooks(ctx, settings, inbound, out)
	if len(chs) == 0 && !webhooks {
		return fmt.Errorf("no channels configured — run `memcode gateway setup` to add one")
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case inb := <-inbound:
			go handle(ctx, root, byName[inb.Channel], inb, out)
		}
	}
}

// channelsFrom builds a live channel for each one whose secret is present in the
// environment. settings carries the non-secret knobs a channel needs (unused by
// Telegram/Discord, which need only their token). A channel whose constructor
// fails is logged and skipped, never fatal to the others.
func channelsFrom(settings gwconfig.Settings, out io.Writer) []channels.Channel {
	var chs []channels.Channel
	if tok := strings.TrimSpace(os.Getenv(gwconfig.EnvTelegramToken)); tok != "" {
		chs = append(chs, telegram.New(tok))
	}
	if tok := strings.TrimSpace(os.Getenv(gwconfig.EnvDiscordToken)); tok != "" {
		if ch, err := discord.New(tok); err != nil {
			fmt.Fprintf(out, "gateway: discord disabled: %v\n", err)
		} else {
			chs = append(chs, ch)
		}
	}
	app := strings.TrimSpace(os.Getenv(gwconfig.EnvSlackAppToken))
	bot := strings.TrimSpace(os.Getenv(gwconfig.EnvSlackBotToken))
	if app != "" && bot != "" {
		chs = append(chs, slack.New(app, bot))
	}
	return chs
}

// startWebhooks mounts each configured inbound trigger on an HTTP server and
// starts it, returning whether any were mounted. The server shuts down when ctx
// is cancelled. GitHub is the only trigger today; WhatsApp mounts here too once
// it's active.
func startWebhooks(ctx context.Context, settings gwconfig.Settings, inbound chan<- channels.Inbound, out io.Writer) bool {
	mux := http.NewServeMux()
	mounted := false

	if secret := strings.TrimSpace(os.Getenv(gwconfig.EnvGitHubSecret)); secret != "" {
		if _, _, ok := githubReplyRoute(settings.GitHub.ReplyTo); !ok {
			fmt.Fprintf(out, "gateway: github disabled: set github.reply_to (e.g. telegram:123456) in gateway.yaml\n")
		} else {
			mux.Handle("/webhook/github", githubtrigger.New(secret, settings.GitHub.ReplyTo).Handler(inbound))
			fmt.Fprintf(out, "gateway: github webhook on POST /webhook/github\n")
			mounted = true
		}
	}
	if !mounted {
		return false
	}

	addr := strings.TrimSpace(settings.Webhook.Addr)
	if addr == "" {
		addr = defaultWebhookAddr
	}
	srv := &http.Server{Addr: addr, Handler: mux}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(out, "gateway: webhook server stopped: %v\n", err)
		}
	}()
	fmt.Fprintf(out, "gateway: webhooks listening on %s\n", addr)
	return true
}

// githubReplyRoute reports whether a usable "<channel>:<conversation>" reply
// route is configured for the GitHub trigger.
func githubReplyRoute(replyTo string) (channel, conversation string, ok bool) {
	channel, conversation, ok = strings.Cut(strings.TrimSpace(replyTo), ":")
	channel, conversation = strings.TrimSpace(channel), strings.TrimSpace(conversation)
	if channel == "" || conversation == "" {
		return "", "", false
	}
	return channel, conversation, true
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
