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
	mu        sync.RWMutex
	settings  gwconfig.Settings // guarded by mu; hot-reloaded from gateway.yaml (see maybeReload)
	byName    map[string]replySender
	disp      *dispatcher
	out       io.Writer
	notify    chan struct{} // wakes the worker when a message is accepted
}

// cfg returns the current settings snapshot. Policy fields (allow-lists,
// projects, agents, channel knobs) hot-reload when gateway.yaml changes — the
// pairing flow depends on an approval taking effect without a restart. Channel
// connections and schedules are wired at startup and do NOT hot-reload.
func (r *runtime) cfg() gwconfig.Settings {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.settings
}

// Run starts every configured surface, then drains the durable inbox until ctx is
// cancelled. root is the default project the agent operates in; mainStore is the
// gateway's (global) event log; settings holds the non-secret gateway config. The
// gateway's OWN durable state — inbox and singleton lock — lives at the global
// config dir, NOT under root/.memcode: it is gateway-operational, and a global
// singleton is what lets one daemon own single-consumer bot tokens.
func Run(ctx context.Context, root string, mainStore store.Store, settings gwconfig.Settings, out io.Writer) error {
	stateDir, err := gwconfig.Dir()
	if err != nil {
		return err
	}
	gw, err := state.Open(ctx, stateDir)
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

// conversationSession derives a stable session id for a (channel, conversation,
// persona) so every message in that conversation resumes the same agent session.
// The persona is part of the key: switching /agent switches to that persona's OWN
// transcript instead of inheriting the previous persona's, and switching back
// resumes where that persona left off. It's deterministic, so no mapping needs to
// be stored; the child resumes it if the transcript exists and creates it under
// this id otherwise. Matches the "sess_" id shape the runtime uses. (The project
// needs no key part: transcripts live under the project root, so a different
// project is a different store.)
func conversationSession(channel, conversation, agent string) string {
	key := channel + ":" + conversation
	if agent != "" {
		key += "#agent:" + agent
	}
	sum := sha256.Sum256([]byte(key))
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
		// A Trusted delivery (webhook, schedule) was routed here by CONFIG, not by a
		// sender picking a live channel — losing it silently would be a config bug
		// eating real work. Refuse so the webhook returns 5xx and the provider
		// retries after the config is fixed.
		if inb.Trusted {
			return fmt.Errorf("no route for channel %q — is it enabled?", inb.Channel)
		}
		fmt.Fprintf(r.out, "gateway: no route for channel %q — dropping message\n", inb.Channel)
		return nil
	}
	// Trigger gate: a group message runs only when the bot is addressed, unless the
	// channel responds to all. A direct message always triggers; a Trusted webhook
	// always triggers.
	cfg := r.cfg()
	if !inb.Trusted && !inb.IsDirect && !inb.Mentioned && !cfg.Get(inb.Channel).RespondToAll {
		r.event(ctx, events.KindGatewayMessageDropped, eventPayload{Channel: inb.Channel, Conversation: inb.Conversation, MessageID: inb.MessageID, Reason: "not addressed"})
		return nil
	}
	// Authorization: default-deny on stable id; a Trusted webhook skips this. An
	// unknown DIRECT sender gets a pairing code instead of pure silence — the
	// operator approves it with `memcode gateway pair approve`.
	if !inb.Trusted && !cfg.Allowed(inb.Channel, inb.Principal) {
		if inb.IsDirect {
			r.offerPairing(ctx, inb)
		} else {
			fmt.Fprintf(r.out, "gateway: %s message from unauthorized principal %q — ignoring (add it to channels.%s.allow_from)\n", inb.Channel, inb.Principal, inb.Channel)
		}
		r.event(ctx, events.KindGatewayUnauthorized, eventPayload{Channel: inb.Channel, Conversation: inb.Conversation, PrincipalID: inb.Principal, MessageID: inb.MessageID})
		return nil
	}
	if inb.MessageID == "" {
		// Can't dedup or durably key it; refuse rather than risk a loop.
		fmt.Fprintf(r.out, "gateway: %s message with no id — dropping\n", inb.Channel)
		return nil
	}
	// A /agent or /project command re-points the conversation for its SUBSEQUENT
	// tasks; it is control, not a task, so it is handled here and not enqueued.
	if r.handleCommand(ctx, inb) {
		return nil
	}
	// Snapshot the conversation's current persona + project at receipt, so a later
	// /project changes only the NEXT task, never this queued one.
	agent, project := r.resolveSelection(ctx, inb.Channel, inb.Conversation)
	fresh, err := r.gw.Accept(ctx, state.Item{
		Channel: inb.Channel, MessageID: inb.MessageID, Conversation: inb.Conversation,
		Principal: inb.Principal, Text: inb.Text, Trusted: inb.Trusted,
		Agent: agent, Project: project,
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

// runWorker drains the durable inbox: fresh messages become jobs, and finished
// jobs whose reply has not been delivered are retried. Both are keyed through an
// in-process guard so the same item is never worked twice at once, and both
// replay after a restart (pending jobs re-run, undelivered replies re-send).
// Blocks until ctx is cancelled.
func (r *runtime) runWorker(ctx context.Context) {
	var mu sync.Mutex
	inflight := map[string]bool{}
	claim := func(key string) bool {
		mu.Lock()
		defer mu.Unlock()
		if inflight[key] {
			return false
		}
		inflight[key] = true
		return true
	}
	release := func(key string) {
		mu.Lock()
		delete(inflight, key)
		mu.Unlock()
	}
	dispatch := func(it state.Item, run func()) {
		key := it.Channel + ":" + it.MessageID
		if !claim(key) {
			return
		}
		r.disp.submit(ctx, it.Channel+":"+it.Conversation, func() {
			run()
			release(key)
		})
	}

	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()

	cfgMtime := r.maybeReload(time.Time{}) // baseline: settings as of startup
	for {
		cfgMtime = r.maybeReload(cfgMtime) // pairing approvals / policy edits land without a restart
		items, err := r.gw.Pending(ctx)
		if err != nil && ctx.Err() == nil {
			fmt.Fprintf(r.out, "gateway: reading inbox: %v\n", err)
		}
		for _, it := range items {
			it := it
			dispatch(it, func() { r.runJob(ctx, it) })
		}
		// Undelivered replies: the job already ran, so only re-send.
		replies, err := r.gw.PendingReplies(ctx)
		if err != nil && ctx.Err() == nil {
			fmt.Fprintf(r.out, "gateway: reading outbound queue: %v\n", err)
		}
		for _, it := range replies {
			it := it
			dispatch(it, func() { r.deliverReply(ctx, it, it.Reply) })
		}
		select {
		case <-ctx.Done():
			return
		case <-r.notify:
		case <-tick.C:
		}
	}
}

// runJob runs one inbox item as a detached agent job and durably records the
// result. The item moves pending → replied the instant the job finishes, so a
// crash or a send failure never re-runs a completed job — only its delivery is
// retried (deliverReply). An interrupted job is still pending and re-runs on
// restart (at-least-once).
func (r *runtime) runJob(ctx context.Context, it state.Item) {
	ch := r.byName[it.Channel]
	if ch == nil {
		_ = r.gw.MarkDone(ctx, it.Channel, it.MessageID)
		return
	}
	// A gateway-triggered job has no TTY to answer approval prompts → Auto mode.
	// Continuity: a stable session id per conversation, so follow-up messages
	// resume the same session (the child does resume-or-create on this id). Tier
	// routes this channel to a stronger model when configured.
	settings := r.cfg()
	cfg := settings.Get(it.Channel)
	session := conversationSession(it.Channel, it.Conversation, it.Agent)
	// Resolve the snapshotted project id to its canonical root. The registry plus
	// the channel's project policy is the authorization boundary: an id that no
	// longer resolves — or that the channel is no longer allowed to use — falls
	// back to the gateway default rather than executing somewhere unauthorized.
	root := r.root
	if it.Project != "" {
		// The channel's project policy is re-checked at execution, not only at
		// snapshot: if it tightened while this task was queued, refuse — never
		// "helpfully" run the task somewhere the channel wasn't pointed.
		if !settings.ProjectAllowed(it.Channel, it.Project) {
			msg := fmt.Sprintf("Project %q is not allowed on this channel anymore; nothing was run.", it.Project)
			if serr := r.gw.SetReplied(ctx, it.Channel, it.MessageID, msg); serr != nil {
				fmt.Fprintf(r.out, "gateway: recording policy refusal for %s: %v\n", it.Channel, serr)
				return
			}
			r.deliverReply(ctx, it, msg)
			return
		}
		if resolved, rerr := settings.ResolveProject(it.Project); rerr == nil {
			root = resolved
		} else {
			fmt.Fprintf(r.out, "gateway: project %q for %s no longer resolves (%v); using default\n", it.Project, it.Channel, rerr)
		}
	}
	// Compose the snapshotted persona's context + skill roots and persist it keyed
	// by session; the spawned child self-discovers it (no jobs.Spawn signature
	// change). No persona → empty envelope → the coding engine runs exactly as the CLI.
	if err := writeContext(session, jobContextFor(it.Agent)); err != nil {
		fmt.Fprintf(r.out, "gateway: composing context for %s: %v\n", it.Channel, err)
	}
	job, err := jobs.Spawn(root, it.Text, string(permissions.ModeAuto), cfg.Tier, false, true, session)
	if err != nil {
		// A spawn failure won't succeed on replay; record the error as the reply so
		// it rides the same durable delivery path instead of being lost.
		msg := "Couldn't start that: " + err.Error()
		if serr := r.gw.SetReplied(ctx, it.Channel, it.MessageID, msg); serr != nil {
			fmt.Fprintf(r.out, "gateway: recording spawn failure for %s: %v\n", it.Channel, serr)
			return
		}
		r.deliverReply(ctx, it, msg)
		return
	}
	r.event(ctx, events.KindGatewayJobSpawned, eventPayload{Channel: it.Channel, Conversation: it.Conversation, PrincipalID: it.Principal, MessageID: it.MessageID, JobID: job.ID})
	fmt.Fprintf(r.out, "gateway: [%s] job %s ← %q\n", it.Channel, job.ID, truncate(it.Text, 60))

	reply := waitForJob(ctx, root, job.ID) // poll under the SAME root the job spawned into, not the default
	if strings.TrimSpace(reply) == "" {
		reply = "Done."
	}
	// Durable handoff: the job is finished and must never re-run, even if delivery
	// below fails or the process crashes. From here the reply is the worker's to
	// deliver. A rare DB write failure leaves the item pending and re-runs it.
	if err := r.gw.SetReplied(ctx, it.Channel, it.MessageID, reply); err != nil {
		fmt.Fprintf(r.out, "gateway: recording reply for %s failed: %v\n", it.Channel, err)
		return
	}
	r.deliverReply(ctx, it, reply)
}

// deliverReply sends a finished job's reply and, on success, marks the item done.
// A transient send failure is retried in-process a few times; if it still fails
// the item stays 'replied' and the worker retries it on a later tick and after a
// restart, so a result is never silently dropped.
func (r *runtime) deliverReply(ctx context.Context, it state.Item, reply string) {
	ch := r.byName[it.Channel]
	if ch == nil {
		_ = r.gw.MarkDone(ctx, it.Channel, it.MessageID) // channel gone; nothing to deliver to
		return
	}
	if strings.TrimSpace(reply) == "" {
		reply = "Done."
	}
	var sendErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Duration(attempt) * 500 * time.Millisecond):
			}
		}
		if sendErr = ch.Send(ctx, it.Conversation, channels.Outbound{Text: reply}); sendErr == nil {
			break
		}
	}
	if sendErr != nil {
		fmt.Fprintf(r.out, "gateway: reply to %s failed, will retry: %v\n", it.Channel, sendErr)
		r.event(ctx, events.KindGatewayResultPosted, eventPayload{Channel: it.Channel, Conversation: it.Conversation, MessageID: it.MessageID, Status: "reply_pending"})
		return // stays 'replied'; retried next tick
	}
	_ = r.gw.MarkDone(ctx, it.Channel, it.MessageID)
	r.event(ctx, events.KindGatewayResultPosted, eventPayload{Channel: it.Channel, Conversation: it.Conversation, MessageID: it.MessageID, Status: "ok"})
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

	// WhatsApp is built but stays inert until whatsapp.active is set — Meta business
	// verification is an external state the gateway can't observe. Mounted BEFORE
	// GitHub so github.reply_to can be validated against every live reply channel.
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

	if secret := strings.TrimSpace(os.Getenv(gwconfig.EnvGitHubSecret)); secret != "" {
		replyCh, _, ok := parseRoute(settings.Get("github").ReplyTo)
		switch {
		case !ok:
			fmt.Fprintf(out, "gateway: github disabled: set github.reply_to (e.g. telegram:123456) in gateway.yaml\n")
		case rt.byName[replyCh] == nil:
			// A route to a channel that isn't running would let GitHub ack work and
			// then drop the result — refuse to mount instead, so deliveries fail
			// visibly until the config is fixed.
			fmt.Fprintf(out, "gateway: github disabled: reply_to channel %q is not enabled\n", replyCh)
		default:
			mux.Handle("/webhook/github", githubtrigger.New(secret, settings.Get("github").ReplyTo).Handler(rt))
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
