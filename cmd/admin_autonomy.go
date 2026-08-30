package cmd

// Admin handlers for agents that run unattended: the delegation policy that
// bounds them, the resources they may reach, on-demand wakes, the durable
// question inbox, the action journal, and health checks.
//
// These are ordinary admin tools, dispatched from adminExecute alongside
// gw_channel and gw_schedule. There is deliberately no separate cockpit: an
// autonomous agent is an agent with an objective, an approved policy, and
// permission to act on its own — not a different species with its own
// management surface.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/memcode-ai/memcode/internal/agent/autonomy"
	"github.com/memcode-ai/memcode/internal/atomicfile"
	"github.com/memcode-ai/memcode/internal/browser"
	"github.com/memcode-ai/memcode/internal/browser/broker"
	gwconfig "github.com/memcode-ai/memcode/internal/gateway/config"
	"github.com/memcode-ai/memcode/internal/llm"
	"github.com/memcode-ai/memcode/internal/mcp"
	"github.com/memcode-ai/memcode/internal/provider"
)

// agentStore opens the autonomy store for a configured agent. The store is
// created lazily, so an ordinary conversational agent never gets one.
func agentStore(ctx context.Context, agent string) (*autonomy.Store, string, gwconfig.Agent, error) {
	s, err := gwconfig.Load()
	if err != nil {
		return nil, "", gwconfig.Agent{}, err
	}
	a, ok := s.Agents[agent]
	if !ok {
		return nil, "", gwconfig.Agent{}, fmt.Errorf("no agent %q", agent)
	}
	home, err := gwconfig.AgentHome(agent)
	if err != nil {
		return nil, "", gwconfig.Agent{}, err
	}
	st, err := autonomy.Open(ctx, home)
	if err != nil {
		return nil, "", gwconfig.Agent{}, err
	}
	return st, home, a, nil
}

func gwPolicy(ctx context.Context, st *autonomy.Store, home, agent, action, document, hash string) (string, error) {
	switch strings.ToLower(action) {
	case "show":
		p, ok, err := st.ApprovedPolicy(ctx, "primary")
		if err != nil {
			return "", err
		}
		if !ok {
			return "no approved policy — consequential work is blocked. Stage one with action=stage, then approve it.", nil
		}
		return fmt.Sprintf("approved policy v%d hash=%s\n%s", p.Version, p.Hash, string(p.Document)), nil
	case "stage":
		var doc autonomy.DelegationPolicy
		if err := json.Unmarshal([]byte(document), &doc); err != nil {
			return "", fmt.Errorf("document is not valid DelegationPolicy JSON: %w", err)
		}
		canon, h, err := autonomy.CanonicalPolicy(doc)
		if err != nil {
			return "", err
		}
		ver, err := st.NextPolicyVersion(ctx, "primary")
		if err != nil {
			return "", err
		}
		if err := st.InsertPolicy(ctx, autonomy.Policy{ID: "policy-" + h[:8], ObjectiveID: "primary", Version: ver, Document: canon, Hash: h, Status: "draft"}); err != nil {
			return "", err
		}
		// The canonical bytes are kept beside the agent so the exact document a
		// human reviewed stays inspectable, keyed by the hash they approve.
		_ = os.MkdirAll(filepath.Join(home, "policies"), 0o700)
		_ = atomicfile.WriteFile(filepath.Join(home, "policies", h+".json"), canon, 0o600)
		_ = autonomy.WriteConfigMirror(ctx, home, st)
		return fmt.Sprintf("Draft policy v%d staged (hash %s). Show the user what it allows in plain language, then approve with gw_policy action=approve hash=%s.", ver, h[:12], h), nil
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
		_ = autonomy.WriteConfigMirror(ctx, home, st)
		return fmt.Sprintf("Approved policy %s for %s. It may now do consequential work within those bounds.", match[:12], agent), nil
	}
	return "", fmt.Errorf("action must be show, stage, or approve")
}

func gwGrant(ctx context.Context, st *autonomy.Store, home, action, rtype, locator, mode, id string) (string, error) {
	switch strings.ToLower(action) {
	case "grant":
		if rtype == "" {
			rtype = "filesystem" // the common case is just a path
		}
		if mode == "" {
			mode = "read"
		}
		if rtype == "filesystem" {
			canon, err := autonomy.CanonicalFilesystemGrant(locator)
			if err != nil {
				return "", fmt.Errorf("cannot grant: %w", err)
			}
			locator = canon
		}
		rid := fmt.Sprintf("res-%s-%d", rtype, time.Now().UnixNano())
		if err := st.InsertResource(ctx, autonomy.Resource{ID: rid, ObjectiveID: "primary", Type: rtype, Locator: locator, AccessMode: mode, AuthorizationSource: "admin", Status: "active"}); err != nil {
			return "", err
		}
		_ = autonomy.WriteConfigMirror(ctx, home, st)
		return fmt.Sprintf("Granted %s %s (%s) as %s.", rtype, locator, mode, rid), nil
	case "list":
		res, err := st.ListResources(ctx, "primary")
		if err != nil {
			return "", err
		}
		if len(res) == 0 {
			return "no resource grants — the agent can only reach its own home", nil
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
		_ = autonomy.WriteConfigMirror(ctx, home, st)
		return "revoked " + id + " (effective at the next dispatch)", nil
	}
	return "", fmt.Errorf("action must be grant, list, or revoke")
}

// gwWake runs one bounded wake on demand. Autonomy is NOT required here —
// being autonomous governs whether an agent wakes on its own, not whether a
// human may ask it to work now.
func gwWake(ctx context.Context, st *autonomy.Store, home, agent string, cfg gwconfig.Agent) (string, error) {
	if strings.TrimSpace(cfg.Objective) == "" {
		return "", fmt.Errorf("agent %q has no objective to advance — set one with gw_agent action=objective", agent)
	}
	if _, hasPol, err := st.ApprovedPolicy(ctx, "primary"); err != nil {
		return "", err
	} else if !hasPol {
		return "blocked: no approved policy — stage and approve one first (gw_policy)", nil
	}
	provider.LoadDotEnv()
	prov, err := provider.NewFromEnv()
	if err != nil {
		return "", fmt.Errorf("no model configured: %w", err)
	}
	ex := &autonomy.Executive{Store: st, Home: home, AgentID: agent, Objective: cfg.Objective, Runner: llm.NewRunner(prov)}
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
		fmt.Fprintf(&b, "suspended on %s — answer with gw_answer\n", out.InteractionID)
	}
	return b.String(), nil
}

func gwInbox(ctx context.Context, st *autonomy.Store, agent string) (string, error) {
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

func gwAnswer(ctx context.Context, st *autonomy.Store, home, agent, id, answer string, cfg gwconfig.Agent) (string, error) {
	in, ok, err := st.GetInteraction(ctx, id)
	if err != nil || !ok {
		return "", fmt.Errorf("no interaction %q", id)
	}
	if in.AgentID != agent {
		return "", fmt.Errorf("interaction %q belongs to %s", id, in.AgentID)
	}
	if in.Status != "pending" {
		return "", fmt.Errorf("interaction %q is not pending (already answered or cancelled) — answering again would re-run its side effects", id)
	}
	provider.LoadDotEnv()
	prov, err := provider.NewFromEnv()
	if err != nil {
		return "", fmt.Errorf("no model configured: %w", err)
	}
	ex := &autonomy.Executive{Store: st, Home: home, AgentID: agent, Objective: cfg.Objective, Runner: llm.NewRunner(prov)}
	// Resume FIRST, mark answered only after: a failed resume must stay
	// retryable rather than swallowing the answer.
	out, err := ex.ResumeSuspended(ctx, in, answer)
	if err != nil {
		return "", fmt.Errorf("resume failed (interaction still pending): %w", err)
	}
	if err := st.ResolveInteraction(ctx, id, answer); err != nil {
		return "", err
	}
	return fmt.Sprintf("answered %s; run %s → %s. %s", id, in.RunID, out.Status, out.Report), nil
}

func gwJournal(ctx context.Context, st *autonomy.Store) (string, error) {
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
		fmt.Fprintf(&b, "  %s %s %s → %s (policy %s)\n", a.CreatedAt.Format("15:04:05"), a.Kind, a.Target, a.Status, shortHash(a.PolicyHash))
	}
	return b.String(), nil
}

func gwDoctor(ctx context.Context, st *autonomy.Store, home, agent string, cfg gwconfig.Agent) (string, error) {
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
	check("objective", cfg.Objective != "", orElse(cfg.Objective, "none — gw_agent action=objective"))
	check("autonomous", cfg.Autonomous, map[bool]string{
		true:  "may run unattended",
		false: "on-demand only (gw_wake); gw_agent action=autonomous to change",
	}[cfg.Autonomous])
	if cfg.Paused {
		fmt.Fprintf(&b, "[info] paused: no unattended wakes will fire\n")
	}
	pol, hasPol, _ := st.ApprovedPolicy(ctx, "primary")
	check("approved policy", hasPol, func() string {
		if hasPol {
			return fmt.Sprintf("v%d %s", pol.Version, shortHash(pol.Hash))
		}
		return "none — consequential work blocked"
	}())
	if _, err := autonomy.InitializeGeneratedWorkspace(home); err != nil {
		check("generated workspace", false, err.Error())
	} else {
		check("generated workspace", true, "git initialized")
	}
	fmt.Fprintf(&b, "[info] sandbox: %s\n", sandboxNote())
	if cfg.Browser == gwconfig.BrowserExistingChrome {
		sock, err := broker.SocketPath()
		reachable := err == nil && broker.NewClient(sock).Reachable()
		check("existing-Chrome broker", reachable, orElse(map[bool]string{true: sock}[reachable], "not reachable — browser work will fail closed (gw_browser)"))
	}
	trigs, _ := st.ListTriggers(ctx)
	pend, _ := st.PendingInteractions(ctx, agent)
	fmt.Fprintf(&b, "self-scheduled wakes: %d, pending questions: %d\n", len(trigs), len(pend))
	return b.String(), nil
}

// gwBrowser checks the prerequisites for driving the user's OWN Chrome and
// attempts a real, bounded connection. It cannot click Chrome's consent dialog
// — that is the user's step, by design.
func gwBrowser(ctx context.Context) (string, error) {
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
	check("npx available", err == nil, orElse(npx, "not found on PATH — Node.js is required"))
	sock, err := broker.SocketPath()
	if err != nil {
		check("broker socket path", false, err.Error())
	} else {
		reachable := broker.NewClient(sock).Reachable()
		check("gateway browser broker", reachable, orElse(map[bool]string{true: sock}[reachable], "not reachable — start the gateway (memcode gateway run) first"))
	}
	if !ok {
		b.WriteString("\nFix the above, then try again.")
		return b.String(), nil
	}
	b.WriteString("\nAttempting a connection to the running Chrome (10s timeout)...\n")
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	mgr := mcpConnectChrome(cctx)
	defer mgr.Close()
	toolCount := len(mgr.Tools())
	if toolCount == 0 {
		b.WriteString("[FAIL] could not connect to Chrome\n")
		for _, e := range mgr.Errors() {
			fmt.Fprintf(&b, "  - %v\n", e)
		}
		b.WriteString("Tell the user to check: Chrome 144+, Remote Debugging toggled on at\n")
		b.WriteString("chrome://inspect/#remote-debugging, Chrome actually running, and to click\n")
		b.WriteString("Allow if a dialog appears — only they can do that last step.\n")
		return b.String(), nil
	}
	fmt.Fprintf(&b, "[ok] connected — %d browser tool(s) available. Existing-Chrome work is ready.\n", toolCount)
	return b.String(), nil
}

func shortHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

func sandboxNote() string {
	if autonomy.SandboxAvailable() {
		return "hardened (bwrap)"
	}
	return "no bwrap — generated code runs fail-closed unless explicitly approved"
}

func orElse(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

// mcpConnectChrome starts chrome-devtools-mcp in --autoConnect mode, which
// attaches to an ALREADY-RUNNING Chrome rather than launching one. The version
// is pinned in internal/browser so the check here and the agent's real browser
// runs speak to the same server.
func mcpConnectChrome(ctx context.Context) *mcp.Manager {
	return mcp.Connect(ctx, map[string]mcp.ServerConfig{
		"chrome-devtools": {Type: "stdio", Command: "npx", Args: []string{"-y", browser.ChromeDevToolsMCPPackage, "--autoConnect"}},
	}, mcp.Options{Version: "0.1.0"})
}
