package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	agentrt "github.com/memcode-ai/memcode/internal/agent/runtime"
	appconfig "github.com/memcode-ai/memcode/internal/config"
	gwconfig "github.com/memcode-ai/memcode/internal/gateway/config"
	"github.com/memcode-ai/memcode/internal/llm"
	"github.com/memcode-ai/memcode/internal/personal"
	"github.com/memcode-ai/memcode/internal/provider"
	"github.com/memcode-ai/memcode/internal/store"
	"github.com/memcode-ai/memcode/internal/vxui"
	"github.com/spf13/cobra"
)

var personalCmd = &cobra.Command{
	Use: "personal", Short: "Manage long-lived Personal Agents",
	Long: `Open the Personal Agents cockpit — an interactive session (like ` + "`memcode admin`" + `)
for managing your long-lived Personal Agents by conversation: objectives,
policies, resources, triggers, wakes, pending questions, and lifecycle.

Subcommands (run, inbox, answer, policy, resources, triggers, history, doctor,
pause/resume/stop/delete) are the same operations in scriptable form.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runPersonalCockpit(cmd.Context())
	},
}

// runPersonalCockpit opens the interactive Personal Agents session. It mirrors
// `memcode admin`: a TUI over the memcode home, with the pa_* typed tools and no
// repo/coding tools. Personal Agent state lives under ~/.memcode/agents/<id>/.
func runPersonalCockpit(ctx context.Context) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	root := filepath.Join(home, ".memcode")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	if _, err := appconfig.Init(root, false); err != nil {
		return err
	}
	cfg, err := appconfig.Load(root)
	if err != nil {
		return err
	}
	st, err := store.Open(ctx, storePath(cfg.Root))
	if err != nil {
		return err
	}
	defer st.Close()

	provider.LoadDotEnv()
	maybeRunFirstRunWizard(ctx, cfg)
	var endpoints []provider.Endpoint
	if ep, ok := cfg.ResolveEndpoint(); ok {
		endpoints = append(endpoints, ep)
	}
	prov := provider.NewFromEnvLazy(endpoints...)
	model := provider.EffectiveModel(cfg.Models.Coder)
	sess := agentrt.New(st, llm.NewRunner(prov), cfg.Root, model, permissions.ModeAsk, os.Stdout)
	sess.SetPersonal(personalExecute)
	if ep, onEndpoint := prov.Endpoint(); onEndpoint {
		sess.SetPin(ep.Model, provider.CatalogWindow(ep.Model))
	} else {
		sess.SetVendor(cfg.Vendor)
		sess.SetPin(cfg.PinnedModel, cfg.PinnedWindow)
	}
	sess.SetServingDefault(cfg.ServingDefault)
	return vxui.Run(ctx, sess, cfg.Theme)
}

func personalStore(ctx context.Context, name string) (*personal.Store, error) {
	st, _, err := personalStoreHome0(ctx, name)
	return st, err
}

// personalStoreHome returns the open store and the agent's home path.
func personalStoreHome(cmd *cobra.Command, name string) (*personal.Store, string, error) {
	return personalStoreHome0(cmd.Context(), name)
}

func personalStoreHome0(ctx context.Context, name string) (*personal.Store, string, error) {
	s, err := gwconfig.Load()
	if err != nil {
		return nil, "", err
	}
	a, ok := s.Agents[name]
	if !ok || a.Kind != "personal" {
		return nil, "", fmt.Errorf("no Personal Agent %q", name)
	}
	home, err := gwconfig.AgentHome(name)
	if err != nil {
		return nil, "", err
	}
	st, err := personal.Open(ctx, home)
	if err != nil {
		return nil, "", err
	}
	return st, home, nil
}

func personalCreate(cmd *cobra.Command, args []string) error {
	name, objective := args[0], strings.Join(args[1:], " ")
	s, err := gwconfig.Load()
	if err != nil {
		return err
	}
	if s.Agents == nil {
		s.Agents = map[string]gwconfig.Agent{}
	}
	if _, ok := s.Agents[name]; ok {
		return fmt.Errorf("agent %q already exists", name)
	}
	s.Agents[name] = gwconfig.Agent{Kind: "personal"}
	if err := gwconfig.Save(s); err != nil {
		return err
	}
	home, err := gwconfig.AgentHome(name)
	if err != nil {
		return err
	}
	st, err := personal.Open(cmd.Context(), home)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.CreateObjective(cmd.Context(), personal.Objective{ID: "primary", Description: objective, Status: "draft"}); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Created Personal Agent %s. Consequential work remains blocked until its delegation policy is approved.\n", name)
	return nil
}

func personalList(cmd *cobra.Command, args []string) error {
	s, err := gwconfig.Load()
	if err != nil {
		return err
	}
	var names []string
	for n, a := range s.Agents {
		if a.Kind == "personal" {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintln(cmd.OutOrStdout(), n)
	}
	return nil
}
func personalShow(cmd *cobra.Command, args []string) error {
	st, err := personalStore(cmd.Context(), args[0])
	if err != nil {
		return err
	}
	defer st.Close()
	os, err := st.ListObjectives(cmd.Context())
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "Personal Agent: %s\n", args[0])
	for _, o := range os {
		fmt.Fprintf(out, "- [%s] %s\n", o.Status, o.Description)
	}
	return nil
}
func personalStatus(status string) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		st, err := personalStore(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		defer st.Close()
		if err := st.SetObjectiveStatus(cmd.Context(), "primary", status); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%s: %s\n", args[0], status)
		return nil
	}
}

func personalRun(cmd *cobra.Command, args []string) error {
	st, home, err := personalStoreHome(cmd, args[0])
	if err != nil {
		return err
	}
	defer st.Close()
	// Fail-closed FIRST: if no approved policy, report blocked before any model
	// is constructed, so the operator sees the real blocker (policy, not auth).
	if _, hasPol, err := st.ApprovedPolicy(cmd.Context(), "primary"); err != nil {
		return err
	} else if !hasPol {
		fmt.Fprintln(cmd.OutOrStdout(), "blocked: no approved policy — run `memcode personal policy set` then `approve-policy`")
		return nil
	}
	provider.LoadDotEnv()
	prov, err := provider.NewFromEnv()
	if err != nil {
		return fmt.Errorf("no model configured (set MEMCODE_API_TOKEN or an API key): %w", err)
	}
	ex := &personal.Executive{Store: st, Home: home, AgentID: args[0], Runner: llm.NewRunner(prov)}
	out, err := ex.RunOnce(cmd.Context())
	if err != nil {
		return err
	}
	w := cmd.OutOrStdout()
	fmt.Fprintf(w, "run %s: %s\n", out.RunID, out.Status)
	if out.Report != "" {
		fmt.Fprintln(w, out.Report)
	}
	if out.NextWakeAt != nil {
		fmt.Fprintf(w, "next wake: %s\n", out.NextWakeAt.Format(time.RFC3339))
	}
	if out.InteractionID != "" {
		fmt.Fprintf(w, "suspended: answer with `memcode personal answer %s %s <answer...>`\n", args[0], out.InteractionID)
	}
	return nil
}

func personalInbox(cmd *cobra.Command, args []string) error {
	st, _, err := personalStoreHome(cmd, args[0])
	if err != nil {
		return err
	}
	defer st.Close()
	inter, err := personal.PendingInteractions(st, args[0])
	if err != nil {
		return err
	}
	w := cmd.OutOrStdout()
	if len(inter) == 0 {
		fmt.Fprintln(w, "inbox empty — no pending questions")
		return nil
	}
	for _, in := range inter {
		fmt.Fprintf(w, "- %s [%s] %s\n", in.ID, in.Kind, in.Question)
	}
	return nil
}

func personalAnswer(cmd *cobra.Command, args []string) error {
	st, home, err := personalStoreHome(cmd, args[0])
	if err != nil {
		return err
	}
	defer st.Close()
	id := args[1]
	answer := strings.Join(args[2:], " ")
	in, ok, err := personal.GetInteraction(st, id)
	if err != nil || !ok {
		return fmt.Errorf("no pending interaction %q", id)
	}
	if in.AgentID != args[0] {
		return fmt.Errorf("interaction %q belongs to %s, not %s", id, in.AgentID, args[0])
	}
	if in.Status != "pending" {
		return fmt.Errorf("interaction %q is not pending (already answered or cancelled) — refusing to re-run its resume", id)
	}
	// Resume FIRST with the model; only mark the interaction answered after the
	// resumed run reaches a terminal state, so a failed resume stays retryable.
	provider.LoadDotEnv()
	prov, err := provider.NewFromEnv()
	if err != nil {
		return fmt.Errorf("no model configured: %w", err)
	}
	ex := &personal.Executive{Store: st, Home: home, AgentID: args[0], Runner: llm.NewRunner(prov)}
	out, err := ex.ResumeSuspended(cmd.Context(), in, answer)
	if err != nil {
		return fmt.Errorf("resume failed (interaction still pending): %w", err)
	}
	if err := personal.ResolveInteraction(st, id, answer); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "interaction %s answered; run %s → %s\n", id, in.RunID, out.Status)
	if out.Report != "" {
		fmt.Fprintln(cmd.OutOrStdout(), out.Report)
	}
	return nil
}
func personalDelete(cmd *cobra.Command, args []string) error {
	name := args[0]
	destructive, _ := cmd.Flags().GetBool("delete-home")
	s, err := gwconfig.Load()
	if err != nil {
		return err
	}
	a, ok := s.Agents[name]
	if !ok || a.Kind != "personal" {
		return fmt.Errorf("no Personal Agent %q", name)
	}
	delete(s.Agents, name)
	if err := gwconfig.Save(s); err != nil {
		return err
	}
	if destructive {
		home, _ := gwconfig.AgentHome(name)
		if err := os.RemoveAll(home); err != nil {
			return err
		}
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Removed %s from gateway configuration; home deleted=%v.\n", name, destructive)
	return nil
}

func init() {
	create := &cobra.Command{Use: "create <name> <objective...>", Args: cobra.MinimumNArgs(2), RunE: personalCreate}
	list := &cobra.Command{Use: "list", Args: cobra.NoArgs, RunE: personalList}
	show := &cobra.Command{Use: "show <name>", Args: cobra.ExactArgs(1), RunE: personalShow}
	pause := &cobra.Command{Use: "pause <name>", Args: cobra.ExactArgs(1), RunE: personalStatus("paused")}
	resume := &cobra.Command{Use: "resume <name>", Args: cobra.ExactArgs(1), RunE: personalStatus("active")}
	stop := &cobra.Command{Use: "stop <name>", Args: cobra.ExactArgs(1), RunE: personalStatus("stopped")}
	run := &cobra.Command{Use: "run <name>", Args: cobra.ExactArgs(1), RunE: personalRun}
	inbox := &cobra.Command{Use: "inbox <name>", Args: cobra.ExactArgs(1), RunE: personalInbox}
	answer := &cobra.Command{Use: "answer <name> <interaction-id> <answer...>", Args: cobra.MinimumNArgs(3), RunE: personalAnswer}
	deleteCmd := &cobra.Command{Use: "delete <name>", Args: cobra.ExactArgs(1), RunE: personalDelete}
	deleteCmd.Flags().Bool("delete-home", false, "also permanently delete the agent home")
	personalCmd.AddCommand(create, list, show, run, inbox, answer, pause, resume, stop, personalPolicyCmd, personalApprovePolicyCmd, personalResourcesCmd, personalTriggersCmd, deleteCmd)
	rootCmd.AddCommand(personalCmd)
}
