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
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/memcode-ai/memcode/internal/agent/tools"
	"github.com/memcode-ai/memcode/internal/atomicfile"
	"github.com/memcode-ai/memcode/internal/browser"
	"github.com/memcode-ai/memcode/internal/browser/broker"
	gwconfig "github.com/memcode-ai/memcode/internal/gateway/config"
	"github.com/memcode-ai/memcode/internal/llm"
	"github.com/memcode-ai/memcode/internal/mcp"
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
		Objective  string `json:"objective"`
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
	if name == tools.PaCreate {
		// The agent doesn't exist yet, so it can't go through paStore below
		// (which requires it to already be registered) — this is the one
		// operation that runs before that gate.
		return paCreate(ctx, in.Agent, in.Objective)
	}
	if name == tools.PaBrowserSetup {
		return paBrowserSetup(ctx) // agent-independent: gateway-wide broker/Chrome setup
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
		return paResource(ctx, st, home, in.Action, in.Type, in.Locator, in.Mode, in.ID)
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
	case tools.PaDoctor:
		return paDoctor(ctx, st, home, in.Agent)
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
		return "No Personal Agents yet. Ask what the user wants one to do, then call pa_create.", nil
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

// paCreate registers a new Personal Agent and its objective. Mirrors
// personalCreate (the CLI's `personal create`) — same steps, same order —
// since both are legitimate entry points to the same operation; this is the
// one the cockpit conversation actually uses.
func paCreate(ctx context.Context, name, objective string) (string, error) {
	name, objective = strings.TrimSpace(name), strings.TrimSpace(objective)
	if name == "" || objective == "" {
		return "", fmt.Errorf("agent name and objective are both required")
	}
	s, err := gwconfig.Load()
	if err != nil {
		return "", err
	}
	if s.Agents == nil {
		s.Agents = map[string]gwconfig.Agent{}
	}
	if _, ok := s.Agents[name]; ok {
		return "", fmt.Errorf("agent %q already exists", name)
	}
	s.Agents[name] = gwconfig.Agent{Kind: "personal"}
	if err := gwconfig.Save(s); err != nil {
		return "", err
	}
	home, err := gwconfig.AgentHome(name)
	if err != nil {
		return "", err
	}
	st, err := personal.Open(ctx, home)
	if err != nil {
		return "", err
	}
	defer st.Close()
	if err := st.CreateObjective(ctx, personal.Objective{ID: "primary", Description: objective, Status: "draft"}); err != nil {
		return "", err
	}
	_ = personal.WriteConfigMirror(ctx, home, st)
	return fmt.Sprintf("Created %s. Consequential work is blocked until a policy is staged and approved — gather what it needs (resources, toolsets, consequence classes, wake cadence), present the plan, then pa_policy stage + approve.", name), nil
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
			return "", fmt.Errorf("no primary objective — this agent doesn't exist yet; use pa_create, not pa_objective, for a brand-new one")
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
		_ = personal.WriteConfigMirror(ctx, home, st)
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
		_ = personal.WriteConfigMirror(ctx, home, st)
		return fmt.Sprintf("Approved policy %s; %s is now active.", match[:12], agent), nil
	}
	return "", fmt.Errorf("action must be show, stage, or approve")
}

func paResource(ctx context.Context, st *personal.Store, home, action, rtype, locator, mode, id string) (string, error) {
	switch strings.ToLower(action) {
	case "grant":
		if rtype == "" {
			rtype = "filesystem" // the common case — same inference the CLI's `resources add` uses
		}
		if mode == "" {
			mode = "read"
		}
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
		_ = personal.WriteConfigMirror(ctx, home, st)
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
		_ = personal.WriteConfigMirror(ctx, home, st)
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

func shortHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

func sandboxNote() string {
	if personal.SandboxAvailable() {
		return "hardened (bwrap)"
	}
	return "no bwrap — generated code runs fail-closed unless explicitly approved"
}

// paDoctor health-checks one agent: home layout, objective, approved policy,
// generated workspace, sandbox availability, trigger/pending-interaction
// counts. Returns a report string rather than a bool — the cockpit relays
// findings to the user itself, it doesn't need a separate pass/fail signal.
func paDoctor(ctx context.Context, st *personal.Store, home, agent string) (string, error) {
	var b strings.Builder
	check := func(label string, good bool, detail string) {
		mark := "ok"
		if !good {
			mark = "FAIL"
		}
		fmt.Fprintf(&b, "[%s] %s: %s\n", mark, label, detail)
	}
	for _, d := range []string{"policies", "workspace/generated", "workspace/scratch", "runs", ".memcode/sessions"} {
		_, err := os.Stat(filepath.Join(home, d))
		check("dir "+d, err == nil, filepath.Join(home, d))
	}
	obj, hasObj, _ := st.GetObjective(ctx, "primary")
	check("objective", hasObj, obj.Description)
	pol, hasPol, _ := st.ApprovedPolicy(ctx, "primary")
	check("approved policy", hasPol, func() string {
		if hasPol {
			return fmt.Sprintf("v%d %s", pol.Version, shortHash(pol.Hash))
		}
		return "none — consequential work blocked"
	}())
	if _, err := personal.InitializeGeneratedWorkspace(home); err != nil {
		check("generated workspace", false, err.Error())
	} else {
		check("generated workspace", true, "git initialized")
	}
	fmt.Fprintf(&b, "[info] sandbox: %s\n", sandboxNote())
	trigs, _ := st.ListTriggers(ctx)
	pend, _ := st.PendingInteractions(ctx, agent)
	fmt.Fprintf(&b, "triggers: %d, pending interactions: %d\n", len(trigs), len(pend))
	return b.String(), nil
}

// paBrowserSetup checks existing-Chrome delegation prerequisites and attempts
// a real, bounded connection — the same checks `memcode personal browser
// setup` used to run as a separate CLI command, now reachable the same way
// every other Personal Agent operation is: a tool call in the cockpit
// conversation, not a command the user has to know to type.
func paBrowserSetup(ctx context.Context) (string, error) {
	var b strings.Builder
	ok := true
	check := func(label string, good bool, detail string) {
		mark := "ok"
		if !good {
			mark = "FAIL"
			ok = false
		}
		fmt.Fprintf(&b, "[%s] %s: %s\n", mark, label, detail)
	}
	npx, err := exec.LookPath("npx")
	check("npx available", err == nil, func() string {
		if err != nil {
			return "not found on PATH — Node.js is required"
		}
		return npx
	}())
	sock, err := broker.SocketPath()
	if err != nil {
		check("broker socket path", false, err.Error())
	} else {
		reachable := broker.NewClient(sock).Reachable()
		check("gateway browser broker", reachable, func() string {
			if reachable {
				return sock
			}
			return "not reachable — start the gateway (memcode gateway run) first"
		}())
	}
	if !ok {
		b.WriteString("\nFix the above, then try again.")
		return b.String(), nil
	}
	b.WriteString("\nAttempting a connection to the running Chrome (10s timeout)...\n")
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	mgr := mcp.Connect(cctx, map[string]mcp.ServerConfig{
		"chrome-devtools": {Type: "stdio", Command: "npx", Args: []string{"-y", browser.ChromeDevToolsMCPPackage, "--autoConnect"}},
	}, mcp.Options{Version: "0.1.0"})
	defer mgr.Close()
	toolCount := len(mgr.Tools())
	if toolCount == 0 {
		b.WriteString("[FAIL] could not connect to Chrome\n")
		for _, e := range mgr.Errors() {
			fmt.Fprintf(&b, "  - %v\n", e)
		}
		b.WriteString("Tell the user: Chrome 144+, chrome://inspect/#remote-debugging toggled on, Chrome\n")
		b.WriteString("actually running, and click Allow if a dialog appears in Chrome — only they can.\n")
		return b.String(), nil
	}
	fmt.Fprintf(&b, "[ok] connected — %d browser tool(s) available. Existing-Chrome delegation is ready.\n", toolCount)
	return b.String(), nil
}
