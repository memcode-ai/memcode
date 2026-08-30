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

// agentChannelName is the internal wake route for an agent running unattended.
// It has no external sender — output is journaled in the agent home — so byName
// gets a discard sink purely so Deliver can route and runJob can pick the work
// up. A schedule targets it with `deliver_to: agent:<name>`, which is what lets
// the ORDINARY schedules: mechanism drive an autonomous wake instead of needing
// a second scheduler.
const agentChannelName = "agent"

const agentRoutePrefix = "agent:"

// hasAutonomousAgents reports whether any configured agent may run unattended.
func hasAutonomousAgents(settings gwconfig.Settings) bool {
	for _, agent := range settings.Agents {
		if agent.Autonomous {
			return true
		}
	}
	return false
}

// autonomousWakeLoop fires an agent's own self-scheduled wakes — the ones it
// asked for from inside a run via schedule_wake ("come back in 45 minutes").
// Human-authored recurring cadence does NOT come through here: that is an
// ordinary `schedules:` entry delivering to agent:<name>, handled by
// applySchedules like every other schedule. Splitting them this way is what
// removes the second cron implementation while still letting an agent control
// its own timing.
//
// Claims are atomic (ClaimDueTrigger), so a fired wake advances its next_due
// and cannot double-fire across restarts or across two gateway processes.
//
// It keeps one open *personal.Store per agent for the life of the loop instead
// of opening and closing a connection (full PRAGMA setup + migration check) on
// every tick — that per-tick churn scaled with agent count and could make a
// tick's own wall time approach its own period. Only this goroutine touches the
// cache, so it needs no locking; stores are closed when ctx is done or an agent
// stops being autonomous.
func (r *runtime) autonomousWakeLoop(ctx context.Context) {
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
		r.fireDueSelfWakes(ctx, stores)
	}
}

func (r *runtime) fireDueSelfWakes(ctx context.Context, stores map[string]*personal.Store) {
	settings := r.cfg()
	now := time.Now().UTC()
	live := map[string]bool{}
	for id, agent := range settings.Agents {
		// Paused stops future unattended wakes without deleting anything; the
		// store stays cached so resuming costs nothing.
		if !agent.Autonomous {
			continue
		}
		live[id] = true
		if agent.Paused {
			continue
		}
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
			// Atomic claim: only one gateway process advances the wake.
			claimed, ok, err := st.ClaimDueTrigger(ctx, t.ID, now)
			if err != nil || !ok {
				continue
			}
			text := fmt.Sprintf("wake for %s (%s)", claimed.ID, claimed.Kind)
			if err := r.enqueueAgentWake(ctx, id, text); err != nil {
				fmt.Fprintf(r.out, "gateway: wake for %s failed: %v\n", id, err)
			}
		}
	}
	// An agent that stopped being autonomous since the last tick: close and drop
	// its cached connection rather than leaking it.
	for id, st := range stores {
		if !live[id] {
			st.Close()
			delete(stores, id)
		}
	}
}

func (r *runtime) enqueueAgentWake(ctx context.Context, agentID, text string) error {
	a, ok := r.cfg().Agents[agentID]
	if !ok || !a.Autonomous {
		return fmt.Errorf("agent %q is not configured to run unattended", agentID)
	}
	return r.Deliver(ctx, channels.Inbound{Channel: agentChannelName, Conversation: agentID, Principal: agentRoutePrefix + agentID, Text: text, Trusted: true, MessageID: fmt.Sprintf("wake-%d", time.Now().UnixNano())})
}

// agentSink is the discard reply target for the internal agent-wake channel:
// an unattended run's output is journaled in the agent home, so there is
// nothing to send anywhere.
type agentSink struct{ out io.Writer }

func (agentSink) Name() string { return agentChannelName }
func (s agentSink) Send(ctx context.Context, _ string, ob channels.Outbound) error {
	fmt.Fprintf(s.out, "gateway: agent: %s\n", truncate(ob.Text, 120))
	return nil
}

// runAutonomousWake executes one unattended wake inline and returns its report
// as the (discarded) reply. Fails closed: an agent that is not autonomous, or
// has no approved policy, gets a blocked report rather than a run.
func (r *runtime) runAutonomousWake(ctx context.Context, agentID string) string {
	a, ok := r.cfg().Agents[agentID]
	if !ok || !a.Autonomous {
		return "[blocked] agent is not configured to run unattended"
	}
	if a.Paused {
		return "[blocked] agent is paused"
	}
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
