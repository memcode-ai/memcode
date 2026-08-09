package cmd

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/memcode-ai/memcode/internal/mcp"
)

// `memcode mcp` manages MCP servers the same way Claude Code documents it: three scopes
// (local/project/user), a checked-in .mcp.json for project scope, and — because that file is
// untrusted repo content — explicit approval before a project server is used.
var mcpCmd = &cobra.Command{
	Use:   "mcp",
	Short: "Manage MCP servers (Model Context Protocol tools)",
	Long: `Connect memcode to external tools/data via MCP servers.

Scopes (precedence: local > project > user):
  local    private to you in THIS project   (~/.memcode/mcp.json)
  project  shared via a checked-in .mcp.json (requires approval before use)
  user     available across all projects     (~/.memcode/mcp.json)

Examples:
  memcode mcp add --transport http supabase https://mcp.supabase.com/mcp --header "Authorization: Bearer ${SUPABASE_ACCESS_TOKEN}"
  memcode mcp add --transport stdio --env LOG=info filesystem -- npx -y @modelcontextprotocol/server-filesystem .
  memcode mcp list
  memcode mcp approve supabase            # trust a project-scoped server
  memcode mcp reset-project-choices`,
}

var mcpListCmd = &cobra.Command{
	Use:   "list",
	Short: "List configured MCP servers and their status",
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		st, cfg, err := openProject(ctx)
		if err != nil {
			return err
		}
		defer st.Close()

		servers := mcp.Resolve(cfg.Root)
		if len(servers) == 0 {
			fmt.Printf("No MCP servers configured. Add one with `memcode mcp add …`.\n")
			return nil
		}
		approvals := mcp.LoadApprovals(cfg.Root)
		for _, ss := range servers {
			fmt.Printf("  %-20s %-8s %-8s %s  %s\n",
				ss.Name, ss.Scope, ss.Config.Transport(), endpoint(ss.Config), statusLabel(ss, approvals))
		}
		return nil
	},
}

var mcpGetCmd = &cobra.Command{
	Use:   "get <name>",
	Short: "Show one MCP server's configuration and status",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		st, cfg, err := openProject(ctx)
		if err != nil {
			return err
		}
		defer st.Close()

		name := args[0]
		var found *mcp.ScopedServer
		for _, ss := range mcp.Resolve(cfg.Root) {
			if ss.Name == name {
				s := ss
				found = &s
				break
			}
		}
		if found == nil {
			return fmt.Errorf("no MCP server named %q", name)
		}
		approvals := mcp.LoadApprovals(cfg.Root)
		fmt.Printf("name:      %s\n", found.Name)
		fmt.Printf("scope:     %s\n", found.Scope)
		fmt.Printf("transport: %s\n", found.Config.Transport())
		if found.Config.URL != "" {
			fmt.Printf("url:       %s\n", found.Config.URL)
		}
		if found.Config.Command != "" {
			fmt.Printf("command:   %s\n", strings.TrimSpace(found.Config.Command+" "+strings.Join(found.Config.Args, " ")))
		}
		if keys := mapKeys(found.Config.Env); len(keys) > 0 {
			fmt.Printf("env:       %s\n", strings.Join(keys, ", "))
		}
		if keys := mapKeys(found.Config.Headers); len(keys) > 0 {
			fmt.Printf("headers:   %s\n", strings.Join(keys, ", "))
		}
		fmt.Printf("status:    %s\n", statusLabel(*found, approvals))
		return nil
	},
}

var mcpAddCmd = &cobra.Command{
	Use:   "add [flags] <name> [<url> | -- <command> [args...]]",
	Short: "Add an MCP server",
	Args:  cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		st, cfg, err := openProject(ctx)
		if err != nil {
			return err
		}
		defer st.Close()

		scope, err := parseScope(cmd)
		if err != nil {
			return err
		}
		transport, _ := cmd.Flags().GetString("transport")
		envPairs, _ := cmd.Flags().GetStringArray("env")
		headerPairs, _ := cmd.Flags().GetStringArray("header")
		timeout, _ := cmd.Flags().GetInt("timeout")

		sc := mcp.ServerConfig{Timeout: timeout}
		if env, err := parsePairs(envPairs, "="); err != nil {
			return fmt.Errorf("--env: %w", err)
		} else {
			sc.Env = env
		}
		if hdr, err := parsePairs(headerPairs, ":"); err != nil {
			return fmt.Errorf("--header: %w", err)
		} else {
			sc.Headers = hdr
		}

		name := args[0]
		dash := cmd.ArgsLenAtDash()
		if dash >= 0 { // stdio: everything after `--` is the command
			cmdArgs := args[dash:]
			if len(cmdArgs) == 0 {
				return fmt.Errorf("stdio server needs a command after `--`")
			}
			sc.Type = "stdio"
			sc.Command = cmdArgs[0]
			sc.Args = cmdArgs[1:]
		} else { // remote: name + url
			if len(args) < 2 {
				return fmt.Errorf("a remote server needs a url (or use `-- <command>` for stdio)")
			}
			sc.Type = orDefaultStr(transport, "http")
			sc.URL = args[1]
		}

		if err := mcp.AddServer(cfg.Root, scope, name, sc); err != nil {
			return err
		}
		fmt.Printf("added %q (%s scope, %s)\n", name, scope, sc.Transport())
		if scope == mcp.ScopeProject {
			fmt.Printf("note: project servers require approval — run `memcode mcp approve %s` before use.\n", name)
		}
		return nil
	},
}

var mcpAddJSONCmd = &cobra.Command{
	Use:   "add-json <name> <json>",
	Short: "Add an MCP server from a JSON config blob",
	Long: `Add a server by pasting its JSON config — handy for copying a server's documented
config verbatim, or for fields the flags don't cover (headersHelper, timeout, auth).

Example:
  memcode mcp add-json --scope project sentry '{"type":"http","url":"https://mcp.sentry.dev/mcp"}'`,
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		st, cfg, err := openProject(ctx)
		if err != nil {
			return err
		}
		defer st.Close()

		scope, err := parseScope(cmd)
		if err != nil {
			return err
		}
		var sc mcp.ServerConfig
		if err := json.Unmarshal([]byte(args[1]), &sc); err != nil {
			return fmt.Errorf("invalid JSON: %w", err)
		}
		if err := mcp.AddServer(cfg.Root, scope, args[0], sc); err != nil {
			return err
		}
		fmt.Printf("added %q (%s scope, %s)\n", args[0], scope, sc.Transport())
		if scope == mcp.ScopeProject {
			fmt.Printf("note: project servers require approval — run `memcode mcp approve %s` before use.\n", args[0])
		}
		return nil
	},
}

var mcpRemoveCmd = &cobra.Command{
	Use:   "remove <name>",
	Short: "Remove an MCP server",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		st, cfg, err := openProject(ctx)
		if err != nil {
			return err
		}
		defer st.Close()

		name := args[0]
		scope, err := scopeOf(cmd, cfg.Root, name)
		if err != nil {
			return err
		}
		ok, err := mcp.RemoveServer(cfg.Root, scope, name)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("no MCP server named %q in %s scope", name, scope)
		}
		fmt.Printf("removed %q (%s scope)\n", name, scope)
		return nil
	},
}

var mcpApproveCmd = &cobra.Command{
	Use:   "approve <name>",
	Short: "Approve a project-scoped server so memcode will use it",
	Args:  cobra.ExactArgs(1),
	RunE:  func(cmd *cobra.Command, args []string) error { return decideProject(cmd, args[0], mcp.Approved) },
}

var mcpRejectCmd = &cobra.Command{
	Use:   "reject <name>",
	Short: "Reject a project-scoped server (it won't be used)",
	Args:  cobra.ExactArgs(1),
	RunE:  func(cmd *cobra.Command, args []string) error { return decideProject(cmd, args[0], mcp.Rejected) },
}

var mcpResetCmd = &cobra.Command{
	Use:   "reset-project-choices",
	Short: "Clear all approve/reject choices for project-scoped servers",
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		st, cfg, err := openProject(ctx)
		if err != nil {
			return err
		}
		defer st.Close()
		if err := mcp.ResetApprovals(cfg.Root); err != nil {
			return err
		}
		fmt.Println("cleared project-server approval choices")
		return nil
	},
}

func decideProject(cmd *cobra.Command, name string, d mcp.Decision) error {
	ctx := cmd.Context()
	st, cfg, err := openProject(ctx)
	if err != nil {
		return err
	}
	defer st.Close()

	sc, ok := mcp.ProjectServers(cfg.Root)[name]
	if !ok {
		return fmt.Errorf("no project-scoped server named %q in %s", name, mcp.ProjectFile(cfg.Root))
	}
	if err := mcp.SaveApproval(cfg.Root, name, sc, d); err != nil {
		return err
	}
	fmt.Printf("%s %q\n", d, name)
	return nil
}

// statusLabel renders a server's standing: trusted scopes are always "ready"; project servers
// reflect their approval state against the current config.
func statusLabel(ss mcp.ScopedServer, approvals mcp.Approvals) string {
	if ss.Scope != mcp.ScopeProject {
		return "ready"
	}
	switch approvals.Status(ss.Name, ss.Config) {
	case mcp.Approved:
		return "approved"
	case mcp.Rejected:
		return "rejected"
	default:
		return "⏸ pending approval"
	}
}

func endpoint(sc mcp.ServerConfig) string {
	if sc.URL != "" {
		return sc.URL
	}
	return strings.TrimSpace(sc.Command + " " + strings.Join(sc.Args, " "))
}

// parseScope reads --scope, defaulting to local (Claude Code's default).
func parseScope(cmd *cobra.Command) (mcp.Scope, error) {
	v, _ := cmd.Flags().GetString("scope")
	switch mcp.Scope(v) {
	case mcp.ScopeLocal, mcp.ScopeProject, mcp.ScopeUser:
		return mcp.Scope(v), nil
	case "":
		return mcp.ScopeLocal, nil
	}
	return "", fmt.Errorf("invalid --scope %q (want local|project|user)", v)
}

// scopeOf resolves the scope to act on for remove: an explicit --scope, else the scope that
// currently defines the server.
func scopeOf(cmd *cobra.Command, root, name string) (mcp.Scope, error) {
	if v, _ := cmd.Flags().GetString("scope"); v != "" {
		return parseScope(cmd)
	}
	for _, ss := range mcp.Resolve(root) {
		if ss.Name == name {
			return ss.Scope, nil
		}
	}
	return "", fmt.Errorf("no MCP server named %q", name)
}

func parsePairs(pairs []string, sep string) (map[string]string, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(pairs))
	for _, p := range pairs {
		k, v, ok := strings.Cut(p, sep)
		if !ok {
			return nil, fmt.Errorf("expected KEY%sVALUE, got %q", sep, p)
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return out, nil
}

func mapKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func orDefaultStr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func init() {
	mcpAddCmd.Flags().String("transport", "http", "transport for a remote server: http|sse (stdio is implied by `-- <command>`)")
	mcpAddCmd.Flags().String("scope", "local", "scope: local|project|user")
	mcpAddCmd.Flags().StringArray("env", nil, "environment variable KEY=VALUE for a stdio server (repeatable)")
	mcpAddCmd.Flags().StringArray("header", nil, "HTTP header \"Name: value\" for a remote server (repeatable)")
	mcpAddCmd.Flags().Int("timeout", 0, "per-server tool-call timeout in milliseconds")
	mcpAddJSONCmd.Flags().String("scope", "local", "scope: local|project|user")
	mcpRemoveCmd.Flags().String("scope", "", "scope to remove from: local|project|user (default: wherever it's defined)")

	mcpCmd.AddCommand(mcpListCmd, mcpGetCmd, mcpAddCmd, mcpAddJSONCmd, mcpRemoveCmd, mcpApproveCmd, mcpRejectCmd, mcpResetCmd)
	rootCmd.AddCommand(mcpCmd)
}
