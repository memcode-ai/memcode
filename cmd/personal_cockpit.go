package cmd

// The Personal Agents cockpit executor: typed pa_* operations over Personal
// Agent state, injected into the runtime (same seam as admin). Secrets and the
// model backend are out of scope here except for pa_wake/pa_answer, which spin
// up a provider for that single wake/resume.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/memcode-ai/memcode/internal/agent/tools"
	"github.com/memcode-ai/memcode/internal/atomicfile"
	gwconfig "github.com/memcode-ai/memcode/internal/gateway/config"
	"github.com/memcode-ai/memcode/internal/llm"
	"github.com/memcode-ai/memcode/internal/personal"
	"github.com/memcode-ai/memcode/internal/provider"
)

func paStore(ctx context.Context, agent string) (*personal.Store, string, error) {
	s, err := gwconfig.Load()
	if err != nil {
		return nil, "", err
	}
	a, ok := s.Agents[agent]
	if !ok || a.Kind != "personal" {
		return nil, "", fmt.Errorf("no Personal Agent %q", agent)
	}
	home, err := gwconfig.AgentHome(agent)
	if err != nil {
		return nil, "", err
	}
	st, err := personal.Open(ctx, home)
	if err != nil {
		return nil, "", err
	}
	return st, home, nil
}

func personalExecute(ctx context.Context, name string, input json.RawMessage) (string, error) {
	var in struct {
		Agent      string `json:"agent"`
		Action     string `json:"action"`
		Text       string `json:"text"`
		Document   string `json:"document"`
		Hash       string `json:"hash"`
		Type       string `json:"type"`
		Locator    string `json:"locator"`
		Mode       string `json:"mode"`
		ID         string `json:"id"`
		Kind       string `json:"kind"`
		Spec       string `json:"spec"`
		Answer     string `json:"answer"`
		DeleteHome string `json:"delete_home"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", err
	}
	if name == tools.PaOverview {
		return paOverview(ctx)
	}
	if strings.TrimSpace(in.Agent) == "" {
		return "", fmt.Errorf("an agent name is required")
	}
	st, home, err := paStore(ctx, in.Agent)
	if err != nil {
		return "", err
	}
	defer st.Close()
	switch name {
	case tools.PaObjective:
		return paObjective(ctx, st, in.Action, in.Text)
	case tools.PaPolicy:
		return paPolicy(ctx, st, home, in.Agent, in.Action, in.Document, in.Hash)
	case tools.PaResource:
		return paResource(ctx, st, in.Action, in.Type, in.Locator, in.Mode, in.ID)
	case tools.PaTrigger:
		return paTrigger(ctx, st, in.Action, in.Kind, in.Spec, in.ID)
	case tools.PaWake:
		return paWake(ctx, st, home, in.Agent)
	case tools.PaInbox:
		return paInbox(ctx, st, in.Agent)
	case tools.PaAnswer:
		return paAnswer(ctx, st, home, in.Agent, in.ID, in.Answer)
	case tools.PaHistory:
		return paHistory(ctx, st)
	case tools.PaLifecycle:
		return paLifecycle(ctx, in.Agent, in.Action, in.DeleteHome)
	}
	return "", fmt.Errorf("unknown personal tool %q", name)
}

func paOverview(ctx context.Context) (string, error) {
	s, err := gwconfig.Load()
	if err != nil {
		return "", err
	}
	var b strings.Builder
	var names []string
	for n, a := range s.Agents {
		if a.Kind == "personal" {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return "No Personal Agents. Create one with `memcode personal create <name> \"<objective>\"`.", nil
	}
	sortStrings(names)
	for _, n := range names {
		st, _, err := paStore(ctx, n)
		if err != nil {
			fmt.Fprintf(&b, "%s: error %v\n", n, err)
			continue
		}
		obj, hasObj, _ := st.GetObjective(ctx, "primary")
		pol, hasPol, _ := st.ApprovedPolicy(ctx, "primary")
		pend, _ := st.PendingInteractions(ctx, n)
		status := "no objective"
		if hasObj {
			status = obj.Status
		}
		polStr := "no approved policy"
		if hasPol {
			polStr = fmt.Sprintf("policy v%d", pol.Version)
		}
		fmt.Fprintf(&b, "%s: %s · %s · %s · %d pending question(s)\n", n, obj.Description, status, polStr, len(pend))
		st.Close()
	}
	return b.String(), nil
}

func paObjective(ctx context.Context, st *personal.Store, action, text string) (string, error) {
	switch strings.ToLower(action) {
	case "show":
		o, ok, err := st.GetObjective(ctx, "primary")
		if err != nil || !ok {
			return "no objective", nil
		}
		return fmt.Sprintf("[%s] %s\nsuccess: %s", o.Status, o.Description, o.SuccessCriteria), nil
	case "set":
		_, ok, err := st.GetObjective(ctx, "primary")
		if err != nil {
			return "", err
		}
		if !ok {
			return "", fmt.Errorf("no primary objective; create the agent with one")
		}
		if err := st.SetObjectiveText(ctx, "primary", text); err != nil {
			return "", err
		}
		return "objective updated", nil
	}
	return "", fmt.Errorf("action must be show or set")
}

func paPolicy(ctx context.Context, st *personal.Store, home, agent, action, document, hash string) (string, error) {
	switch strings.ToLower(action) {
	case "show":
		p, ok, err := st.ApprovedPolicy(ctx, "primary")
		if err != nil {
			return "", err
		}
		if !ok {
			return "no approved policy — consequential work is blocked. Stage one with action=stage then approve.", nil
		}
		return fmt.Sprintf("approved policy v%d hash=%s\n%s", p.Version, p.Hash, string(p.Document)), nil
	case "stage":
		var doc personal.DelegationPolicy
		if err := json.Unmarshal([]byte(document), &doc); err != nil {
			return "", fmt.Errorf("document is not valid DelegationPolicy JSON: %w", err)
		}
		canon, h, err := personal.CanonicalPolicy(doc)
		if err != nil {
			return "", err
		}
		ver, err := st.NextPolicyVersion(ctx, "primary")
		if err != nil {
			return "", err
		}
		if err := st.InsertPolicy(ctx, personal.Policy{ID: "policy-" + h[:8], ObjectiveID: "primary", Version: ver, Document: canon, Hash: h, Status: "draft"}); err != nil {
			return "", err
		}
		// Persist the canonical doc to policies/<hash>.json (parity with the CLI).
		_ = os.MkdirAll(filepath.Join(home, "policies"), 0o700)
		_ = atomicfile.WriteFile(filepath.Join(home, "policies", h+".json"), canon, 0o600)
		return fmt.Sprintf("Draft policy v%d staged (hash %s). Approve with pa_policy action=approve hash=%s.", ver, h[:12], h), nil
	case "approve":
		pols, err := st.ListPolicies(ctx, "primary")
		if err != nil {
			return "", err
		}
		var match string
		for _, p := range pols {
			if p.Hash == hash || strings.HasPrefix(p.Hash, hash) {
				match = p.Hash
				break
			}
		}
		if match == "" {
			return "", fmt.Errorf("no policy matching %q", hash)
		}
		if err := st.ApprovePolicy(ctx, match); err != nil {
			return "", err
		}
		_ = st.SetObjectiveStatus(ctx, "primary", "active")
		return fmt.Sprintf("Approved policy %s; %s is now active.", match[:12], agent), nil
	}
	return "", fmt.Errorf("action must be show, stage, or approve")
}

func paResource(ctx context.Context, st *personal.Store, action, rtype, locator, mode, id string) (string, error) {
	switch strings.ToLower(action) {
	case "grant":
		if rtype == "filesystem" {
			canon, err := personal.CanonicalFilesystemGrant(locator)
			if err != nil {
				return "", fmt.Errorf("cannot grant: %w", err)
			}
			locator = canon
		}
		rid := fmt.Sprintf("res-%s-%d", rtype, time.Now().UnixNano())
		if err := st.InsertResource(ctx, personal.Resource{ID: rid, ObjectiveID: "primary", Type: rtype, Locator: locator, AccessMode: mode, AuthorizationSource: "cockpit", Status: "active"}); err != nil {
			return "", err
		}
		return fmt.Sprintf("Granted %s %s (%s) as %s.", rtype, locator, mode, rid), nil
	case "list":
		res, err := st.ListResources(ctx, "primary")
		if err != nil {
			return "", err
		}
		if len(res) == 0 {
			return "no resource grants — the agent can only use its own home", nil
		}
		var b strings.Builder
		for _, r := range res {
			fmt.Fprintf(&b, "%s: %s %s (%s) [%s]\n", r.ID, r.Type, r.Locator, r.AccessMode, r.Status)
		}
		return b.String(), nil
	case "revoke":
		if err := st.SetResourceStatus(ctx, id, "revoked"); err != nil {
			return "", err
		}
		return "revoked " + id, nil
	}
	return "", fmt.Errorf("action must be grant, list, or revoke")
}

func paTrigger(ctx context.Context, st *personal.Store, action, kind, spec, id string) (string, error) {
	switch strings.ToLower(action) {
	case "add":
		kindMap := map[string]string{"interval": "interval", "cron": "cron", "one-shot": "one_shot"}
		dbKind, ok := kindMap[strings.ToLower(kind)]
		if !ok {
			return "", fmt.Errorf("kind must be interval, cron, or one-shot")
		}
		now := time.Now().UTC()
		next, err := personal.NextDue(dbKind, spec, now)
		if err != nil {
			return "", fmt.Errorf("bad spec: %w", err)
		}
		tid := fmt.Sprintf("trig-%s-%d", dbKind, now.Unix())
		if err := st.CreateTrigger(ctx, personal.Trigger{ID: tid, ObjectiveID: "primary", Kind: dbKind, Spec: spec, NextDueAt: &next}); err != nil {
			return "", err
		}
		return fmt.Sprintf("Trigger %s added; next wake %s.", tid, next.Format(time.RFC3339)), nil
	case "list":
		trigs, err := st.ListTriggers(ctx)
		if err != nil {
			return "", err
		}
		if len(trigs) == 0 {
			return "no triggers", nil
		}
		var b strings.Builder
		for _, t := range trigs {
			next := "—"
			if t.NextDueAt != nil {
				next = t.NextDueAt.Format(time.RFC3339)
			}
			fmt.Fprintf(&b, "%s: %s %q next=%s [%s]\n", t.ID, t.Kind, t.Spec, next, t.Status)
		}
		return b.String(), nil
	case "pause", "resume":
		status := "paused"
		if strings.ToLower(action) == "resume" {
			status = "enabled"
		}
		if _, err := st.DB().ExecContext(ctx, `UPDATE triggers SET status=?,updated_at=? WHERE id=?`, status, time.Now().UTC().Format(time.RFC3339Nano), id); err != nil {
			return "", err
		}
		return fmt.Sprintf("trigger %s %s", id, status), nil
	}
	return "", fmt.Errorf("action must be add, list, pause, or resume")
}

func paWake(ctx context.Context, st *personal.Store, home, agent string) (string, error) {
	if _, hasPol, err := st.ApprovedPolicy(ctx, "primary"); err != nil {
		return "", err
	} else if !hasPol {
		return "blocked: no approved policy — stage and approve one first", nil
	}
	provider.LoadDotEnv()
	prov, err := provider.NewFromEnv()
	if err != nil {
		return "", fmt.Errorf("no model configured: %w", err)
	}
	ex := &personal.Executive{Store: st, Home: home, AgentID: agent, Runner: llm.NewRunner(prov)}
	out, err := ex.RunOnce(ctx)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "run %s: %s\n", out.RunID, out.Status)
	if out.Report != "" {
		b.WriteString(out.Report + "\n")
	}
	if out.InteractionID != "" {
		fmt.Fprintf(&b, "suspended on %s — answer with pa_answer\n", out.InteractionID)
	}
	return b.String(), nil
}

func paInbox(ctx context.Context, st *personal.Store, agent string) (string, error) {
	inter, err := st.PendingInteractions(ctx, agent)
	if err != nil {
		return "", err
	}
	if len(inter) == 0 {
		return "inbox empty — no pending questions", nil
	}
	var b strings.Builder
	for _, in := range inter {
		fmt.Fprintf(&b, "%s [%s] %s\n", in.ID, in.Kind, in.Question)
	}
	return b.String(), nil
}

func paAnswer(ctx context.Context, st *personal.Store, home, agent, id, answer string) (string, error) {
	in, ok, err := st.GetInteraction(ctx, id)
	if err != nil || !ok {
		return "", fmt.Errorf("no pending interaction %q", id)
	}
	if in.AgentID != agent {
		return "", fmt.Errorf("interaction %q belongs to %s", id, in.AgentID)
	}
	provider.LoadDotEnv()
	prov, err := provider.NewFromEnv()
	if err != nil {
		return "", fmt.Errorf("no model configured: %w", err)
	}
	ex := &personal.Executive{Store: st, Home: home, AgentID: agent, Runner: llm.NewRunner(prov)}
	out, err := ex.ResumeSuspended(ctx, in, answer)
	if err != nil {
		return "", fmt.Errorf("resume failed (interaction still pending): %w", err)
	}
	if err := st.ResolveInteraction(ctx, id, answer); err != nil {
		return "", err
	}
	return fmt.Sprintf("answered %s; run %s → %s. %s", id, in.RunID, out.Status, out.Report), nil
}

func paHistory(ctx context.Context, st *personal.Store) (string, error) {
	runs, err := st.ListRuns(ctx, "primary", 10)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "runs (%d):\n", len(runs))
	for _, r := range runs {
		fmt.Fprintf(&b, "  %s [%s] %s\n", r.ID, r.Status, r.CreatedAt.Format(time.RFC3339))
	}
	actions, _ := st.ListActions(ctx, "primary", 20)
	fmt.Fprintf(&b, "actions (%d):\n", len(actions))
	for _, a := range actions {
		fmt.Fprintf(&b, "  %s %s %s → %s\n", a.CreatedAt.Format("15:04:05"), a.Kind, a.Target, a.Status)
	}
	return b.String(), nil
}

func paLifecycle(ctx context.Context, agent, action, deleteHome string) (string, error) {
	s, err := gwconfig.Load()
	if err != nil {
		return "", err
	}
	a, ok := s.Agents[agent]
	if !ok || a.Kind != "personal" {
		return "", fmt.Errorf("no Personal Agent %q", agent)
	}
	switch strings.ToLower(action) {
	case "pause", "resume", "stop":
		st, _, err := paStore(ctx, agent)
		if err != nil {
			return "", err
		}
		defer st.Close()
		status := map[string]string{"pause": "paused", "resume": "active", "stop": "stopped"}[strings.ToLower(action)]
		if err := st.SetObjectiveStatus(ctx, "primary", status); err != nil {
			return "", err
		}
		return fmt.Sprintf("%s: %s", agent, status), nil
	case "delete":
		delete(s.Agents, agent)
		if err := gwconfig.Save(s); err != nil {
			return "", err
		}
		if strings.EqualFold(deleteHome, "true") {
			home, _ := gwconfig.AgentHome(agent)
			if err := os.RemoveAll(home); err != nil {
				return "", err
			}
		}
		return fmt.Sprintf("Removed %s (home deleted=%s).", agent, deleteHome), nil
	}
	return "", fmt.Errorf("action must be pause, resume, stop, or delete")
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
