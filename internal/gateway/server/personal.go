package server

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/memcode-ai/memcode/internal/channels"
	gwconfig "github.com/memcode-ai/memcode/internal/gateway/config"
	"github.com/memcode-ai/memcode/internal/llm"
	"github.com/memcode-ai/memcode/internal/personal"
	"github.com/memcode-ai/memcode/internal/provider"
)

const personalRoutePrefix = "personal:"

// personalChannel is the internal wake route for Personal Agents. It has no
// external sender; byName gets a discard entry so Deliver routes and runJob
// handles the executive inline.
const personalChannelName = "personal"

func hasPersonalAgents(settings gwconfig.Settings) bool {
	for _, agent := range settings.Agents {
		if agent.Kind == "personal" {
			return true
		}
	}
	return false
}

// personalWakeLoop polls each Personal Agent's durable triggers and enqueues a
// wake for any that are due. Claims are atomic (ClaimDueTrigger), so a fired
// trigger advances its next_due and cannot double-fire across restarts.
//
// It keeps one open *personal.Store per agent for the life of the loop instead
// of opening and closing a connection (full PRAGMA setup + migration check) on
// every 15s tick — that per-tick churn scaled with agent count and could make
// a tick's own wall time approach its own period. Only this goroutine touches
// the cache, so it needs no locking; stores are closed when ctx is done or an
// agent is removed from config.
func (r *runtime) personalWakeLoop(ctx context.Context) {
	stores := map[string]*personal.Store{}
	defer func() {
		for _, st := range stores {
			st.Close()
		}
	}()
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		r.fireDuePersonalTriggers(ctx, stores)
	}
}

func (r *runtime) fireDuePersonalTriggers(ctx context.Context, stores map[string]*personal.Store) {
	settings := r.cfg()
	now := time.Now().UTC()
	live := map[string]bool{}
	for id, agent := range settings.Agents {
		if agent.Kind != "personal" {
			continue
		}
		live[id] = true
		st := stores[id]
		if st == nil {
			home, err := gwconfig.AgentHome(id)
			if err != nil {
				continue
			}
			st, err = personal.Open(ctx, home)
			if err != nil {
				continue
			}
			stores[id] = st
		}
		due, err := st.DueTriggers(ctx, now)
		if err != nil {
			continue
		}
		for _, t := range due {
			// Atomic claim: only one gateway process advances the trigger.
			claimed, ok, err := st.ClaimDueTrigger(ctx, t.ID, now)
			if err != nil || !ok {
				continue
			}
			text := fmt.Sprintf("wake for trigger %s (%s)", claimed.ID, claimed.Kind)
			if err := r.enqueuePersonalWake(ctx, id, text); err != nil {
				fmt.Fprintf(r.out, "gateway: personal wake for %s failed: %v\n", id, err)
			}
		}
	}
	// An agent removed (or reconfigured away from kind=personal) since the last
	// tick: close and drop its cached connection rather than leaking it.
	for id, st := range stores {
		if !live[id] {
			st.Close()
			delete(stores, id)
		}
	}
}

func (r *runtime) enqueuePersonalWake(ctx context.Context, agentID, text string) error {
	a, ok := r.cfg().Agents[agentID]
	if !ok || a.Kind != "personal" {
		return fmt.Errorf("no Personal Agent %q", agentID)
	}
	return r.Deliver(ctx, channels.Inbound{Channel: personalChannelName, Conversation: agentID, Principal: personalRoutePrefix + agentID, Text: text, Trusted: true, MessageID: fmt.Sprintf("wake-%d", time.Now().UnixNano())})
}

// personalSink is the discard reply target for the internal personal channel:
// executive output is journaled in the agent home, so there is nothing to send.
type personalSink struct{ out io.Writer }

func (personalSink) Name() string { return personalChannelName }
func (s personalSink) Send(ctx context.Context, _ string, ob channels.Outbound) error {
	fmt.Fprintf(s.out, "gateway: personal: %s\n", truncate(ob.Text, 120))
	return nil
}

// runPersonalWake executes one Personal Agent executive wake inline and returns
// its report as the (discarded) reply. Policy-gated: no approved policy → a
// blocked report, never a run.
func (r *runtime) runPersonalWake(ctx context.Context, agentID string) string {
	home, err := gwconfig.AgentHome(agentID)
	if err != nil {
		return "error: " + err.Error()
	}
	st, err := personal.Open(ctx, home)
	if err != nil {
		return "error: " + err.Error()
	}
	defer st.Close()
	// Fail-closed FIRST: report blocked before constructing a model, so a missing
	// policy surfaces as policy (not a model/auth error) in the gateway log.
	if _, hasPol, err := st.ApprovedPolicy(ctx, "primary"); err != nil {
		return "error: " + err.Error()
	} else if !hasPol {
		return "[blocked] no approved policy"
	}
	provider.LoadDotEnv()
	prov, err := provider.NewFromEnv()
	if err != nil {
		return "error: no model configured: " + err.Error()
	}
	ex := &personal.Executive{Store: st, Home: home, AgentID: agentID, Runner: llm.NewRunner(prov)}
	out, err := ex.RunOnce(ctx)
	if err != nil {
		return "error: " + err.Error()
	}
	return fmt.Sprintf("[%s] %s", out.Status, out.Report)
}
