// Package server is the memcode gateway runtime — the first external surface of
// memcode's event/agent spine. It starts each configured channel, DURABLY records
// every accepted inbound message before acknowledging the provider, then a worker
// drains that inbox: each message runs as a detached agent job (crash-isolated
// subprocess, reusing internal/jobs) and the result is posted back. Gateway
// activity is logged to the main event store, but an inbound chat message is never
// turned into a project objective. Coding is one use of this loop, not what it's
// built around: an inbound message is just a task.
package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/channels"
	"github.com/memcode-ai/memcode/internal/channels/discord"
	"github.com/memcode-ai/memcode/internal/channels/slack"
	"github.com/memcode-ai/memcode/internal/channels/telegram"
	"github.com/memcode-ai/memcode/internal/events"
	gwconfig "github.com/memcode-ai/memcode/internal/gateway/config"
	"github.com/memcode-ai/memcode/internal/gateway/state"
	"github.com/memcode-ai/memcode/internal/jobs"
	"github.com/memcode-ai/memcode/internal/store"
	githubtrigger "github.com/memcode-ai/memcode/internal/triggers/github"
	"github.com/memcode-ai/memcode/internal/triggers/whatsapp"
)

// replySender is the one thing posting a result back needs: a Send. Both chat
// channels and webhook-driven surfaces (WhatsApp) satisfy it.
type replySender interface {
	Send(ctx context.Context, conversation string, msg channels.Outbound) error
}

const defaultWebhookAddr = ":8787"

// eventPayload is the JSON body of a gateway_* event in the main store.
type eventPayload struct {
	Channel      string `json:"channel"`
	Conversation string `json:"conversation,omitempty"`
	PrincipalID  string `json:"principal_id,omitempty"`
	MessageID    string `json:"message_id,omitempty"`
	JobID        string `json:"job_id,omitempty"`
	Status       string `json:"status,omitempty"`
	Reason       string `json:"reason,omitempty"`
}

// runtime holds the gateway's live wiring. It implements channels.Sink (Deliver),
// so adapters hand messages straight to it.
type runtime struct {
	root      string
	gw        *state.Store
	mainStore store.Store // main .memcode event log; may be nil (events best-effort)
	settings  gwconfig.Settings
	byName    map[string]replySender
	disp      *dispatcher
	out       io.Writer
	notify    chan struct{} // wakes the worker when a message is accepted
}

// Run starts every configured surface, then drains the durable inbox until ctx is
// cancelled. root is the project the agent operates in; mainStore is the project's
// event log (for gateway_* events); settings holds the non-secret gateway config.
func Run(ctx context.Context, root string, mainStore store.Store, settings gwconfig.Settings, out io.Writer) error {
	gw, err := state.Open(ctx, filepath.Join(root, ".memcode"))
	if err != nil {
		return fmt.Errorf("opening gateway state: %w", err)
	}
	defer gw.Close()
	_ = gw.PruneDone(ctx, time.Now().Add(-30*24*time.Hour))

	warnOpenSurfaces(settings, out)

	rt := &runtime{
		root:      root,
		gw:        gw,
		mainStore: mainStore,
		settings:  settings,
		byName:    make(map[string]replySender, 4),
		disp:      newDispatcher(),
		out:       out,
		notify:    make(chan struct{}, 1),
	}

	chs := channelsFrom(settings, gw, out)
	for _, ch := range chs {
		rt.byName[ch.Name()] = ch
		ch := ch
		go func() {
			if err := ch.Start(ctx, rt); err != nil && ctx.Err() == nil {
				fmt.Fprintf(out, "gateway: channel %s stopped: %v\n", ch.Name(), err)
			}
		}()
		fmt.Fprintf(out, "gateway: %s listening\n", ch.Name())
	}

	webhooks := startWebhooks(ctx, settings, rt, out)
	if len(chs) == 0 && !webhooks {
		return fmt.Errorf("no channels configured — run `memcode gateway setup` to add one")
	}

	rt.startSchedules(ctx) // time-triggered tasks feed the same inbox

	rt.runWorker(ctx) // blocks until ctx is cancelled
	return ctx.Err()
}

// startSchedules runs each configured schedule on its cadence. A fire produces a
// Trusted synthetic message routed to the schedule's deliver_to, so it flows
// through the exact same Deliver → inbox → worker → reply path as a chat message.
func (r *runtime) startSchedules(ctx context.Context) {
	if len(r.settings.Schedules) == 0 {
		return
	}
	c := cron.New()
	added := 0
	for _, sch := range r.settings.Schedules {
		sch := sch
		spec, ok := scheduleSpec(sch)
		if !ok {
			fmt.Fprintf(r.out, "gateway: schedule %q skipped: set exactly one of every/cron\n", sch.Name)
			continue
		}
		ch, convo, ok := parseRoute(sch.DeliverTo)
		if !ok {
			fmt.Fprintf(r.out, "gateway: schedule %q skipped: deliver_to must be \"channel:conversation\"\n", sch.Name)
			continue
		}
		if _, err := c.AddFunc(spec, func() { r.fireSchedule(ctx, sch, ch, convo) }); err != nil {
			fmt.Fprintf(r.out, "gateway: schedule %q skipped: bad schedule %q: %v\n", sch.Name, spec, err)
			continue
		}
		fmt.Fprintf(r.out, "gateway: schedule %q → %s (%s)\n", sch.Name, sch.DeliverTo, spec)
		added++
	}
	if added == 0 {
		return
	}
	c.Start()
	go func() {
		<-ctx.Done()
		c.Stop()
	}()
}

// fireSchedule enqueues one scheduled run as a Trusted inbound. Each fire gets a
// unique id so the inbox dedup treats repeats as distinct work.
func (r *runtime) fireSchedule(ctx context.Context, sch gwconfig.Schedule, channel, conversation string) {
	inb := channels.Inbound{
		Channel:      channel,
		Conversation: conversation,
		Principal:    "schedule:" + sch.Name,
		Text:         sch.Task,
		Trusted:      true,
		MessageID:    fmt.Sprintf("cron:%s:%d", sch.Name, time.Now().UnixNano()),
	}
	if err := r.Deliver(ctx, inb); err != nil {
		fmt.Fprintf(r.out, "gateway: schedule %q enqueue failed: %v\n", sch.Name, err)
	}
}

// conversationSession derives a stable session id for a (channel, conversation)
// so every message in that conversation resumes the same agent session. It's
// deterministic, so no mapping needs to be stored; the child resumes it if the
// transcript exists and creates it under this id otherwise. Matches the "sess_"
// id shape the runtime uses.
func conversationSession(channel, conversation string) string {
	sum := sha256.Sum256([]byte(channel + ":" + conversation))
	return "sess_" + hex.EncodeToString(sum[:16])
}

// warnOpenSurfaces prints a prominent warning for settings that hand an
// autonomous agent to senders who aren't individually allow-listed. The
// destructive-command floor still holds (a gateway job has no approver, so
// dangerous/catastrophic commands are denied), but file edits and medium commands
// on your repo are real power — so make an open surface a loud, deliberate choice.
func warnOpenSurfaces(settings gwconfig.Settings, out io.Writer) {
	if settings.AllowAll {
		fmt.Fprintf(out, "gateway: WARNING allow_all is set — ANYONE on any configured channel can drive the agent in this repo\n")
	}
	for name, ch := range settings.Channels {
		open := false
		for _, p := range ch.AllowFrom {
			if p == "*" {
				open = true
			}
		}
		if open {
			fmt.Fprintf(out, "gateway: WARNING channels.%s.allow_from includes \"*\" — anyone who can reach %s can drive the agent\n", name, name)
		}
		if ch.RespondToAll {
			fmt.Fprintf(out, "gateway: WARNING channels.%s.respond_to_all is set — the agent acts on every group message, not only when mentioned\n", name)
		}
	}
}

// scheduleSpec turns a Schedule into a cron spec: a raw cron expression, or an
// "@every <duration>" from Every. Exactly one of the two must be set.
func scheduleSpec(sch gwconfig.Schedule) (string, bool) {
	switch {
	case sch.Cron != "" && sch.Every == "":
		return sch.Cron, true
	case sch.Every != "" && sch.Cron == "":
		return "@every " + sch.Every, true
	default:
		return "", false
	}
}

// Deliver applies gating and authorization, and durably records a message that
// should run. Returns nil once the provider may be acked (recorded, duplicate, or
// intentionally dropped); a non-nil error means it was NOT recorded, so the
// adapter must not ack.
func (r *runtime) Deliver(ctx context.Context, inb channels.Inbound) error {
	if r.byName[inb.Channel] == nil {
		fmt.Fprintf(r.out, "gateway: no route for channel %q — dropping message\n", inb.Channel)
		return nil
	}
	// Trigger gate: a group message runs only when the bot is addressed, unless the
	// channel responds to all. A direct message always triggers; a Trusted webhook
	// always triggers.
	if !inb.Trusted && !inb.IsDirect && !inb.Mentioned && !r.settings.Get(inb.Channel).RespondToAll {
		r.event(ctx, events.KindGatewayMessageDropped, eventPayload{Channel: inb.Channel, Conversation: inb.Conversation, MessageID: inb.MessageID, Reason: "not addressed"})
		return nil
	}
	// Authorization: default-deny on stable id; a Trusted webhook skips this.
	if !inb.Trusted && !r.settings.Allowed(inb.Channel, inb.Principal) {
		fmt.Fprintf(r.out, "gateway: %s message from unauthorized principal %q — ignoring (add it to channels.%s.allow_from)\n", inb.Channel, inb.Principal, inb.Channel)
		r.event(ctx, events.KindGatewayUnauthorized, eventPayload{Channel: inb.Channel, Conversation: inb.Conversation, PrincipalID: inb.Principal, MessageID: inb.MessageID})
		return nil
	}
	if inb.MessageID == "" {
		// Can't dedup or durably key it; refuse rather than risk a loop.
		fmt.Fprintf(r.out, "gateway: %s message with no id — dropping\n", inb.Channel)
		return nil
	}
	fresh, err := r.gw.Accept(ctx, state.Item{
		Channel: inb.Channel, MessageID: inb.MessageID, Conversation: inb.Conversation,
		Principal: inb.Principal, Text: inb.Text, Trusted: inb.Trusted,
	}, time.Now())
	if err != nil {
		return err // NOT durably recorded — adapter must not ack
	}
	if !fresh {
		return nil // duplicate delivery; already recorded
	}
	r.event(ctx, events.KindGatewayMessageReceived, eventPayload{Channel: inb.Channel, Conversation: inb.Conversation, PrincipalID: inb.Principal, MessageID: inb.MessageID})
	select {
	case r.notify <- struct{}{}: // wake the worker
	default:
	}
	return nil
}

// runWorker drains the durable inbox: it submits each pending item to its
// conversation's serial worker and processes it. On startup it also replays any
// items a prior crash left pending. Blocks until ctx is cancelled.
func (r *runtime) runWorker(ctx context.Context) {
	var mu sync.Mutex
	inflight := map[string]bool{}
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()

	for {
		items, err := r.gw.Pending(ctx)
		if err != nil && ctx.Err() == nil {
			fmt.Fprintf(r.out, "gateway: reading inbox: %v\n", err)
		}
		for _, it := range items {
			key := it.Channel + ":" + it.MessageID
			mu.Lock()
			if inflight[key] {
				mu.Unlock()
				continue
			}
			inflight[key] = true
			mu.Unlock()

			it := it
			r.disp.submit(ctx, it.Channel+":"+it.Conversation, func() {
				r.process(ctx, it)
				mu.Lock()
				delete(inflight, key)
				mu.Unlock()
			})
		}
		select {
		case <-ctx.Done():
			return
		case <-r.notify:
		case <-tick.C:
		}
	}
}

// process runs one inbox item as a detached agent job and posts the result. The
// item is marked done only after the job COMPLETES (before the reply is sent), so
// a restart re-runs an interrupted job (at-least-once) but never re-runs a job
// that already finished.
func (r *runtime) process(ctx context.Context, it state.Item) {
	ch := r.byName[it.Channel]
	if ch == nil {
		_ = r.gw.MarkDone(ctx, it.Channel, it.MessageID)
		return
	}
	// A gateway-triggered job has no TTY to answer approval prompts → Auto mode.
	// Continuity: a stable session id per conversation, so follow-up messages
	// resume the same session (the child does resume-or-create on this id). Tier
	// routes this channel to a stronger model when configured.
	tier := r.settings.Get(it.Channel).Tier
	job, err := jobs.Spawn(r.root, it.Text, string(permissions.ModeAuto), tier, false, true, conversationSession(it.Channel, it.Conversation))
	if err != nil {
		_ = ch.Send(ctx, it.Conversation, channels.Outbound{Text: "Couldn't start that: " + err.Error()})
		_ = r.gw.MarkDone(ctx, it.Channel, it.MessageID) // a spawn failure won't succeed on replay
		return
	}
	r.event(ctx, events.KindGatewayJobSpawned, eventPayload{Channel: it.Channel, Conversation: it.Conversation, PrincipalID: it.Principal, MessageID: it.MessageID, JobID: job.ID})
	fmt.Fprintf(r.out, "gateway: [%s] job %s ← %q\n", it.Channel, job.ID, truncate(it.Text, 60))

	reply := waitForJob(ctx, r.root, job.ID)
	if strings.TrimSpace(reply) == "" {
		reply = "Done."
	}
	// Job finished — never re-run it, even if the reply below fails or a crash
	// follows. (A failed outbound reply is not retried; a durable outbound queue
	// would be the next enhancement.)
	_ = r.gw.MarkDone(ctx, it.Channel, it.MessageID)

	status := "ok"
	if err := ch.Send(ctx, it.Conversation, channels.Outbound{Text: reply}); err != nil {
		fmt.Fprintf(r.out, "gateway: reply to %s failed: %v\n", it.Channel, err)
		status = "reply_failed"
	}
	r.event(ctx, events.KindGatewayResultPosted, eventPayload{Channel: it.Channel, Conversation: it.Conversation, MessageID: it.MessageID, JobID: job.ID, Status: status})
}

// event appends a gateway event to the main store, best-effort.
func (r *runtime) event(ctx context.Context, kind events.Kind, p eventPayload) {
	if r.mainStore == nil {
		return
	}
	_, _ = events.Append(ctx, r.mainStore, kind, "gateway", p)
}

// channelsFrom builds a live channel for each one whose secret is present in the
// environment. A channel whose constructor fails is logged and skipped.
func channelsFrom(settings gwconfig.Settings, gw *state.Store, out io.Writer) []channels.Channel {
	var chs []channels.Channel
	if tok := strings.TrimSpace(os.Getenv(gwconfig.EnvTelegramToken)); tok != "" {
		chs = append(chs, telegram.New(tok, gw))
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
// starts it, returning whether any were mounted. rt is the sink each trigger
// delivers into; WhatsApp also registers its Send in byName so replies route back.
func startWebhooks(ctx context.Context, settings gwconfig.Settings, rt *runtime, out io.Writer) bool {
	mux := http.NewServeMux()
	mounted := false

	if secret := strings.TrimSpace(os.Getenv(gwconfig.EnvGitHubSecret)); secret != "" {
		if _, _, ok := parseRoute(settings.Get("github").ReplyTo); !ok {
			fmt.Fprintf(out, "gateway: github disabled: set github.reply_to (e.g. telegram:123456) in gateway.yaml\n")
		} else {
			mux.Handle("/webhook/github", githubtrigger.New(secret, settings.Get("github").ReplyTo).Handler(rt))
			fmt.Fprintf(out, "gateway: github webhook on POST /webhook/github\n")
			mounted = true
		}
	}

	// WhatsApp is built but stays inert until whatsapp.active is set — Meta business
	// verification is an external state the gateway can't observe.
	token := strings.TrimSpace(os.Getenv(gwconfig.EnvWhatsAppToken))
	verify := strings.TrimSpace(os.Getenv(gwconfig.EnvWhatsAppVerify))
	appSecret := strings.TrimSpace(os.Getenv(gwconfig.EnvWhatsAppSecret))
	pn := strings.TrimSpace(settings.Get("whatsapp").PhoneNumberID)
	if pn != "" && token != "" && verify != "" {
		switch {
		case !settings.Get("whatsapp").Active:
			fmt.Fprintf(out, "gateway: whatsapp configured but inactive (set whatsapp.active: true after Meta verification)\n")
		case appSecret == "":
			fmt.Fprintf(out, "gateway: whatsapp inactive: set %s (Meta app secret) to verify inbound messages\n", gwconfig.EnvWhatsAppSecret)
		default:
			wc := whatsapp.New(pn, token, verify, appSecret)
			rt.byName[wc.Name()] = wc
			mux.Handle("/webhook/whatsapp", wc.Handler(rt))
			fmt.Fprintf(out, "gateway: whatsapp webhook on /webhook/whatsapp\n")
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

// parseRoute reports whether a usable "<channel>:<conversation>" reply route
// is configured for the GitHub trigger.
func parseRoute(replyTo string) (channel, conversation string, ok bool) {
	channel, conversation, ok = strings.Cut(strings.TrimSpace(replyTo), ":")
	channel, conversation = strings.TrimSpace(channel), strings.TrimSpace(conversation)
	if channel == "" || conversation == "" {
		return "", "", false
	}
	return channel, conversation, true
}

// waitForJob polls until the job leaves the running state and returns text to post
// back: the agent's result on success, or a pointer to the log on failure.
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
