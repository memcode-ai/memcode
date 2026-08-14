package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/memcode-ai/memcode/internal/sessionlog"
	"github.com/memcode-ai/memcode/internal/store"
)

var sessionCmd = &cobra.Command{
	Use:   "session",
	Short: "Inspect recorded agent sessions",
}

var sessionListCmd = &cobra.Command{
	Use:     "recent",
	Aliases: []string{"list"},
	Short:   "List recent agent sessions (newest first)",
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		st, _, err := openProject(ctx)
		if err != nil {
			return err
		}
		defer st.Close()

		sessions, err := loadSessions(ctx, st)
		if err != nil {
			return err
		}
		if len(sessions) == 0 {
			fmt.Println("No agent sessions yet. Run `memcode run \"<task>\"`.")
			return nil
		}
		for i := len(sessions) - 1; i >= 0; i-- { // newest first
			s := sessions[i]
			fmt.Printf("%s  %-5s  changed %d file(s)  %s\n", s.id, s.mode, len(s.filesChanged), s.task)
		}
		return nil
	},
}

var sessionShowCmd = &cobra.Command{
	Use:   "show [session-id]",
	Short: "Show the timeline of an agent session (latest if omitted)",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		st, _, err := openProject(ctx)
		if err != nil {
			return err
		}
		defer st.Close()

		sessions, err := loadSessions(ctx, st)
		if err != nil {
			return err
		}
		if len(sessions) == 0 {
			fmt.Println("No agent sessions yet.")
			return nil
		}
		var sel *sessionView
		if len(args) == 1 {
			for i := range sessions {
				if sessions[i].id == args[0] {
					sel = &sessions[i]
				}
			}
			if sel == nil {
				return fmt.Errorf("no session %q", args[0])
			}
		} else {
			sel = &sessions[len(sessions)-1] // latest
		}
		renderSession(*sel)
		return nil
	},
}

type sessionView struct {
	id           string
	task         string
	mode         string
	model        string
	headSHA      string
	dirtyBefore  int
	iterations   int
	filesChanged []string
	diffSummary  string
	timeline     []store.Event
}

// loadSessions groups the event log into agent sessions by session_id.
func loadSessions(ctx context.Context, st store.Store) ([]sessionView, error) {
	evs, err := st.ListEvents(ctx, store.EventFilter{})
	if err != nil {
		return nil, err
	}
	order := []string{}
	byID := map[string]*sessionView{}
	for _, e := range evs {
		p := decodePayload(e.Payload)
		id, _ := p["session_id"].(string)
		if id == "" {
			continue
		}
		sv, ok := byID[id]
		if !ok {
			sv = &sessionView{id: id}
			byID[id] = sv
			order = append(order, id)
		}
		sv.timeline = append(sv.timeline, e)
		switch e.Kind {
		case "agent_session_started":
			sv.task, _ = p["task"].(string)
			sv.mode, _ = p["mode"].(string)
			sv.model, _ = p["model"].(string)
			sv.headSHA, _ = p["head_sha"].(string)
			sv.dirtyBefore = len(asStrings(p["dirty_before"]))
		case "agent_session_finished":
			sv.iterations = asInt(p["iterations"])
			sv.filesChanged = asStrings(p["files_changed_by_agent"])
			sv.diffSummary, _ = p["diff_summary"].(string)
		}
	}
	out := make([]sessionView, 0, len(order))
	for _, id := range order {
		out = append(out, *byID[id])
	}
	return out, nil
}

func renderSession(s sessionView) {
	fmt.Printf("session %s\n", s.id)
	fmt.Printf("  task:     %s\n", s.task)
	fmt.Printf("  mode:     %-6s  model: %s\n", s.mode, s.model)
	fmt.Printf("  baseline: HEAD %s · %d file(s) already dirty\n", shortSHA(s.headSHA), s.dirtyBefore)

	fmt.Printf("\n  timeline:\n")
	for _, e := range s.timeline {
		p := decodePayload(e.Payload)
		fmt.Printf("    %-22s %s\n", e.Kind, eventDetail(e.Kind, p))
	}

	fmt.Printf("\n  result: %d iteration(s)\n", s.iterations)
	if len(s.filesChanged) == 0 {
		fmt.Printf("  agent changed no files\n")
	} else {
		fmt.Printf("  agent changed: %v\n", s.filesChanged)
		if s.diffSummary != "" {
			fmt.Printf("%s\n", indent(s.diffSummary, "    "))
		}
	}
}

func eventDetail(kind string, p map[string]any) string {
	switch kind {
	case "tool_called":
		if t, ok := p["tool"].(string); ok {
			return t
		}
	case "command_executed", "test_run":
		cmd, _ := p["command"].(string)
		return fmt.Sprintf("%s  (exit %d)", cmd, asInt(p["exit"]))
	case "file_edited":
		path, _ := p["path"].(string)
		if via, ok := p["via"].(string); ok {
			return path + "  (via " + via + ")"
		}
		return path
	case "agent_session_started":
		t, _ := p["task"].(string)
		return t
	}
	return ""
}

// --- payload helpers ---

func decodePayload(raw json.RawMessage) map[string]any {
	m := map[string]any{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &m)
	}
	return m
}

func asStrings(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, x := range arr {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func asInt(v any) int {
	if f, ok := v.(float64); ok {
		return int(f)
	}
	return 0
}

func shortSHA(sha string) string {
	if sha == "" {
		return "(none)"
	}
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

func indent(s, pad string) string {
	out := ""
	for i, line := range splitLines(s) {
		if i > 0 {
			out += "\n"
		}
		out += pad + line
	}
	return out
}

func splitLines(s string) []string {
	var lines []string
	cur := ""
	for _, r := range s {
		if r == '\n' {
			lines = append(lines, cur)
			cur = ""
			continue
		}
		cur += string(r)
	}
	lines = append(lines, cur)
	return lines
}

// --- episodic log (.memcode/sessions/<id>/) subcommands ---

var sessionRecapCmd = &cobra.Command{
	Use:   "recap",
	Short: "Checklist recap of the latest session — what we did (dig deeper with recent/search)",
	RunE: func(cmd *cobra.Command, args []string) error {
		st, cfg, err := openProject(cmd.Context())
		if err != nil {
			return err
		}
		defer st.Close()
		recs, err := sessionlog.LatestRecent(cfg.Root, 0)
		if err != nil {
			return err
		}
		fmt.Println(sessionlog.RenderRecap(sessionlog.Recap(recs)))
		return nil
	},
}

var sessionRecentCmd = &cobra.Command{
	Use:     "current",
	Aliases: []string{"latest"},
	Short:   "Show the latest/current session's activity (episodic log)",
	RunE: func(cmd *cobra.Command, args []string) error {
		st, cfg, err := openProject(cmd.Context())
		if err != nil {
			return err
		}
		defer st.Close()
		recs, err := sessionlog.LatestRecent(cfg.Root, 40)
		if err != nil {
			return err
		}
		printSessionRecords(recs)
		return nil
	},
}

var sessionSearchCmd = &cobra.Command{
	Use:   "search <query>",
	Short: "Search every session's episodic log",
	Args:  cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		st, cfg, err := openProject(cmd.Context())
		if err != nil {
			return err
		}
		defer st.Close()
		recs, err := sessionlog.Search(cfg.Root, strings.Join(args, " "), 30)
		if err != nil {
			return err
		}
		printSessionRecords(recs)
		return nil
	},
}

var sessionCommitsCmd = &cobra.Command{
	Use:   "commits",
	Short: "Show git commit/push actions recorded across sessions",
	RunE: func(cmd *cobra.Command, args []string) error {
		st, cfg, err := openProject(cmd.Context())
		if err != nil {
			return err
		}
		defer st.Close()
		recs, err := sessionlog.Commits(cfg.Root)
		if err != nil {
			return err
		}
		printSessionRecords(recs)
		return nil
	},
}

var sessionSidequestsCmd = &cobra.Command{
	Use:   "sidequests",
	Short: "Show the latest session's user requests (sidequests), in order",
	RunE: func(cmd *cobra.Command, args []string) error {
		st, cfg, err := openProject(cmd.Context())
		if err != nil {
			return err
		}
		defer st.Close()
		recs, err := sessionlog.LatestRecent(cfg.Root, 0)
		if err != nil {
			return err
		}
		for _, q := range sessionlog.Reduce(recs).Requests {
			fmt.Println("› " + q)
		}
		return nil
	},
}

func printSessionRecords(recs []sessionlog.Record) {
	if len(recs) == 0 {
		fmt.Println("(nothing recorded)")
		return
	}
	for _, r := range recs {
		ts := r.TS.Local().Format("Jan 2 15:04")
		switch r.Kind {
		case sessionlog.KindUserMessage:
			fmt.Printf("%s  › %s\n", ts, clipCLI(r.Text, 160))
		case sessionlog.KindAssistantMessage:
			fmt.Printf("%s  memcode: %s\n", ts, clipCLI(r.Text, 160))
		case sessionlog.KindToolCall:
			fmt.Printf("%s  ⏺ %s %s\n", ts, r.Tool, clipCLI(r.Input, 120))
		case sessionlog.KindApproval:
			fmt.Printf("%s  ✓ %s: %s\n", ts, r.Decision, clipCLI(r.Text, 120))
		case sessionlog.KindToolResult:
			mark := "⎿"
			if r.IsError {
				mark = "⚠"
			}
			fmt.Printf("%s  %s %s\n", ts, mark, clipCLI(r.Content, 120))
		case sessionlog.KindSessionStarted:
			fmt.Printf("%s  ▸ session start (%s, %s)\n", ts, r.Model, r.Mode)
		case sessionlog.KindSessionFinished:
			fmt.Printf("%s  ■ session end\n", ts)
		}
	}
}

func clipCLI(s string, max int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i] + " …"
	}
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}

func init() {
	sessionCmd.AddCommand(sessionListCmd, sessionShowCmd, sessionRecapCmd,
		sessionRecentCmd, sessionSearchCmd, sessionCommitsCmd, sessionSidequestsCmd)
	rootCmd.AddCommand(sessionCmd)
}
