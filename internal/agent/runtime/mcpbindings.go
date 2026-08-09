package runtime

// Progressive disclosure of MCP, the discovery half of "code execution with MCP": the model
// never receives the connected tools' schemas upfront. It sees (a) one compact `mcp`
// meta-tool (search / schema / call — defined in mcpToolDefs) whose bytes never vary with
// what's connected, and (b) a one-line-per-server index in the volatile facts block
// (~25 tokens/server, cache-free by construction). Tool names and schemas are fetched on
// demand through the functions here, which also back the mcp_code_exec bridge's search_tools /
// tool_schema script functions. Discovery is ungated catalog metadata; authorization happens
// at invocation (invokeMCP), never here — what a model can SEE and what a script may CALL
// are deliberately independent.

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// mcpSearchCap bounds a search reply — a 10,000-tool catalog must not undo the token savings
// disclosure exists for. The tail is counted, not silently dropped.
const mcpSearchCap = 100

// mcpIndexFact renders the per-server facts line: "github (12 tools), supabase (31 tools)".
// Server count is user-controlled and small; tool counts signal where to search. "" when
// nothing is connected (the fact is omitted entirely).
func (s *Session) mcpIndexFact() string {
	if s.mcp == nil {
		return ""
	}
	counts := map[string]int{}
	for _, t := range s.mcp.Tools() {
		counts[t.Server]++
	}
	if len(counts) == 0 {
		return ""
	}
	names := make([]string, 0, len(counts))
	for n := range counts {
		names = append(names, n)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, n := range names {
		parts = append(parts, fmt.Sprintf("%s (%s)", n, countNoun(counts[n], "tool", "tools")))
	}
	return strings.Join(parts, ", ")
}

// mcpSearch lists tools matching query (case-insensitive substring over name and
// description; empty query lists everything) as "name — description" lines.
func (s *Session) mcpSearch(query string) string {
	q := strings.ToLower(strings.TrimSpace(query))
	var lines []string
	total := 0
	for _, t := range s.mcp.Tools() {
		if q != "" && !strings.Contains(strings.ToLower(t.Name), q) &&
			!strings.Contains(strings.ToLower(t.Description), q) {
			continue
		}
		total++
		if total <= mcpSearchCap {
			lines = append(lines, t.Name+" — "+clip(firstLine(t.Description), 120))
		}
	}
	if total == 0 {
		if q == "" {
			return "no MCP tools connected."
		}
		return "no MCP tools match " + strconv.Quote(query) + " — try a broader query or an empty one to list all."
	}
	out := strings.Join(lines, "\n")
	if total > mcpSearchCap {
		out += fmt.Sprintf("\n…(%d more — refine the query)", total-mcpSearchCap)
	}
	return out
}

// mcpSchema returns one tool's description and full input schema — read before first use.
func (s *Session) mcpSchema(name string) string {
	t, ok := s.mcp.Lookup(name)
	if !ok {
		return "unknown MCP tool: " + name + " — find the exact name with a search first."
	}
	schema, err := json.Marshal(t.InputSchema)
	if err != nil || len(t.InputSchema) == 0 {
		schema = []byte("{}")
	}
	return t.Name + " — " + t.Description + "\ninput schema: " + string(schema)
}
