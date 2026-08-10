package cmd

import (
	"context"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/memcode-ai/memcode/internal/agent/introspect"
	"github.com/memcode-ai/memcode/internal/agent/runtime"
	"github.com/memcode-ai/memcode/internal/agent/secrets"
	"github.com/memcode-ai/memcode/internal/llm"
	"github.com/memcode-ai/memcode/internal/provider"
)

// `memcode mcp serve` turns memcode into an MCP SERVER: it exposes THIS project's
// memory — semantic recall, the project overview, learned claims, work history,
// and the code-location oracle — as tools any MCP client can call. So a Claude
// Code / Cursor / Codex user can give their existing agent memcode's memory of
// the repo without switching agents: add one stdio server and their agent can
// ask "what did we decide about X" and "where does Y live" and get memcode's
// answer.
//
// The memory tools are OFFLINE (no model, no network, no memcode account) — they
// read the local .memcode store — so `serve` runs anywhere memcode has indexed a
// project. Transport is stdio: the process speaks newline-delimited JSON-RPC on
// stdin/stdout and MUST keep stdout clean of everything else (openProject's
// notices go to stderr).
var mcpServeCmd = &cobra.Command{
	Use:   "serve",
	Short: "Serve this project's memory to MCP clients (Claude Code, Cursor, …) over stdio",
	Long: `Expose memcode's memory of THIS project as MCP tools over stdio, so another
agent (Claude Code, Cursor, Codex, any MCP client) can recall decisions, read the
project overview, list learned claims, search work history, and locate code —
without switching away from the agent it already uses.

Add it to a client that speaks MCP, e.g. Claude Code:
  claude mcp add memcode -- memcode mcp serve

The memory tools are read-only and run offline against the local .memcode store.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		st, cfg, err := openProject(ctx)
		if err != nil {
			return err
		}
		defer st.Close()

		// A provider is constructed for the engine's shape, but the tools we
		// register are the offline commands — they never dial it, so `serve`
		// works signed-out.
		prov := provider.NewFromEnvLazy()
		eng := introspect.New(introspect.Deps{
			Root:     cfg.Root,
			Store:    st,
			Runner:   llm.NewRunner(prov),
			Prov:     prov,
			Redactor: secrets.NewFromEnv(),
		})

		srv := mcpsdk.NewServer(&mcpsdk.Implementation{
			Name:    "memcode",
			Title:   "memcode project memory",
			Version: rootCmd.Version,
		}, nil)
		registerMemoryTools(srv, eng, cfg.Root)

		// Run until the client disconnects or the context is cancelled. All
		// protocol I/O is on stdio; nothing else may write to stdout.
		return srv.Run(ctx, &mcpsdk.StdioTransport{})
	},
}

// intelText adapts an introspect command (text + isError) to an MCP tool result.
func intelText(text string, isErr bool) (*mcpsdk.CallToolResult, any, error) {
	return &mcpsdk.CallToolResult{
		IsError: isErr,
		Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: text}},
	}, nil, nil
}

// registerMemoryTools installs the read-only memory tools. Each wraps the same
// introspection the `memcode` agent uses on itself, so an external agent gets an
// identical answer to `memcode <command>`.
func registerMemoryTools(srv *mcpsdk.Server, eng *introspect.Engine, root string) {
	type recallIn struct {
		Query string `json:"query" jsonschema:"what to look for in this project's memory — a decision, a bug, a feature, a name"`
	}
	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "memcode_recall",
		Description: "Search this project's memory (past decisions, learned facts, work history) for anything relevant to a query. Use before asking the user to re-explain the repo.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in recallIn) (*mcpsdk.CallToolResult, any, error) {
		text, isErr := eng.Intelligence(ctx, "recall", in.Query)
		return intelText(text, isErr)
	})

	type empty struct{}
	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "memcode_overview",
		Description: "A concise overview of what this project is, how it's structured, and how it fits together — memcode's cached understanding of the repo.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, _ empty) (*mcpsdk.CallToolResult, any, error) {
		text, isErr := eng.Intelligence(ctx, "overview", "")
		return intelText(text, isErr)
	})

	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "memcode_claims",
		Description: "The durable facts memcode has learned about this project — invariants, conventions, and constraints worth honoring before making changes.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, _ empty) (*mcpsdk.CallToolResult, any, error) {
		text, isErr := eng.Intelligence(ctx, "claims", "")
		return intelText(text, isErr)
	})

	type sessionIn struct {
		Query string `json:"query,omitempty" jsonschema:"optional filter to find specific past work; omit for the most recent sessions"`
	}
	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "memcode_session",
		Description: "Recent work history on this project — what was done, when, and why. Use to answer 'what were we working on' or 'what changed here recently'.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in sessionIn) (*mcpsdk.CallToolResult, any, error) {
		text, isErr := eng.Intelligence(ctx, "session", in.Query)
		return intelText(text, isErr)
	})

	type codeIn struct {
		Query string `json:"query" jsonschema:"what you're looking for, in words — e.g. 'where is the auth token verified'"`
		Scope string `json:"scope,omitempty" jsonschema:"optional subdirectory to limit the search to"`
	}
	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "memcode_locate",
		Description: "Locate where something lives in this codebase — a deterministic ranked search (no LLM) that returns the most relevant files and lines for a plain-language query.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in codeIn) (*mcpsdk.CallToolResult, any, error) {
		text, ok := runtime.CodeQuery(ctx, root, in.Query, in.Scope)
		return intelText(text, !ok)
	})
}

func init() {
	mcpCmd.AddCommand(mcpServeCmd)
}
