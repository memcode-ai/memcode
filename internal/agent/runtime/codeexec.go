package runtime

// mcp_code_exec — programmatic MCP orchestration ("code execution with MCP").
//
// The model writes a short Python 3 script; the script calls the user's MCP
// tools — individually gated — as ordinary functions over an RPC bridge, and
// ONLY the script's stdout returns to the conversation. Intermediate results
// flow script-side and never enter the model's context — that is the entire
// point: a query-MCP-50-times-and-join workflow costs one tool result instead
// of fifty.
//
// The bridge surface is MCP-ONLY, by design. An earlier iteration also bridged
// the standard read-only repo tools (read_file, ripgrep, …), and the overlap
// was a measured disaster: given two ways to read a file, the model used
// the exec tool as a plain read proxy 35/35 times in one audited session — same
// tokens, plus a subprocess, minus the context-cache/eviction benefits of a
// direct call. The repo tools live on the direct path ONLY; mcp_code_exec exists
// for what has no direct path (orchestrating external MCP surfaces).
//
// MCP through the bridge deliberately punches through the sandbox's network
// denial: the bridge handlers run in the PARENT, so an approved MCP call is the
// run's only egress. That is why every MCP call goes through the same invokeMCP
// gate as a direct call (approving a call approves egress of its arguments),
// why the run keeps an external-call log, and why remote time is budgeted.
//
// Bridge transport: two inherited pipes (child fd 3 = requests out, fd 4 =
// responses in), JSON lines. Pipes survive the sandbox wrappers (sandbox-exec
// and bwrap both preserve inherited fds), need no sockets or ports, and no other
// process can connect to them. The Python side is a prelude prepended to the
// model's script; the Go side dispatches to the SAME session handler methods the
// normal tool path uses, so path resolution, secret masking, and per-tool output
// caps are identical to a direct call.
//
// Containment: the script always runs under the OS sandbox when the platform
// has one (Workspace policy + network denial — this is generated code, so
// unlike bash the sandbox is not opt-in here; MEMCODE_SANDBOX=0 remains the
// explicit escape hatch). The permission gate scales to the containment, the
// Anthropic model (their code_execution runs confirm-free in an isolated
// no-network sandbox): contained = Medium (an edit's blast radius — writes
// repo-confined, no egress), uncontained = Dangerous like browser_eval.
// Not parallel-safe, not offered read-only.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	stdruntime "runtime"
	"strings"
	"sync"
	"time"

	"github.com/memcode-ai/memcode/internal/agent/jobs"
	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/agent/tools"
	"github.com/memcode-ai/memcode/internal/atomicfile"
	"github.com/memcode-ai/memcode/internal/sandbox"
	"github.com/memcode-ai/memcode/internal/textutil"
)

const (
	// Time is budgeted per RESOURCE, not by one wall clock — a script blocked on an MCP
	// approval card must not die while the user thinks:
	//   codeExecCPULimit    — sandbox compute, kernel-enforced (ulimit -t on the child).
	//   codeExecMCPBudget   — cumulative remote-call TRANSPORT time (gate/prompt time excluded).
	//   codeExecTotalLimit  — everything including approval waits; the hard backstop.
	// Per-MCP-call timeouts stay the server's own (mcp.Manager applies them).
	codeExecCPULimit   = 120 // seconds (matches the old 2-minute ceiling for pure compute)
	codeExecMCPBudget  = 5 * time.Minute
	codeExecTotalLimit = 15 * time.Minute

	maxBridgeCalls  = 200     // runaway-loop backstop; scripts should need far fewer
	maxBridgeLine   = 4 << 20 // request-line scanner cap (args are small; results go the other way)
	maxBridgeReply  = 4 << 20 // single bridge reply cap — an oversized MCP result truncates with a marker
	maxScriptOutput = 1 << 20 // captured stdout/stderr cap before the final result truncate

	// codeExecMaxResult caps what the script's stdout returns to the model. This is
	// deliberately the READ ceiling (64KB, = maxFileRead), not the generic 8KB
	// tool-output cap: the stdout here is a distilled result the model explicitly
	// computed, not a raw dump — the token savings come from the intermediates
	// staying script-side, not from squeezing the answer.
	codeExecMaxResult = maxFileRead
)

// codeExecPrelude is prepended to every script. It defines the bridge client and
// the MCP-only helper surface (flat > clever: complex generated-code APIs raise
// error rates). Names use _-prefixes so the model's script can't collide.
// gather() is the parallel-orchestration primitive: it sends every request
// before reading any reply, and the Go side dispatches them concurrently.
// There are deliberately NO helpers for the standard repo tools — see the
// header comment: the overlap made the exec tool a read proxy.
const codeExecPrelude = `import json as _json, os as _os
_req = _os.fdopen(3, "w", buffering=1)
_resp = _os.fdopen(4, "r")
_seq = [0]
_replies = {}
def _send(tool, args):
    _seq[0] += 1
    _req.write(_json.dumps({"id": _seq[0], "tool": tool, "args": {k: v for k, v in args.items() if v is not None}}) + "\n")
    return _seq[0]
def _recv(rid):
    while rid not in _replies:
        line = _resp.readline()
        if not line:
            raise RuntimeError("memcode bridge closed")
        msg = _json.loads(line)
        _replies[msg.get("id")] = msg
    msg = _replies.pop(rid)
    if msg.get("error"):
        raise RuntimeError(msg["error"])
    return msg.get("result", "")
def call(tool, **args):
    return _recv(_send(tool, args))
def gather(*calls):
    ids = [_send(tool, kwargs) for tool, kwargs in calls]
    return [_recv(i) for i in ids]
def search_tools(query=""): return call("search_tools", query=query)
def tool_schema(name): return call("tool_schema", name=name)
def mcp(tool, **args): return call(tool, **args)
`

func (s *Session) codeExec(ctx context.Context, input json.RawMessage) toolResult {
	var in tools.MCPCodeExecInput
	if err := json.Unmarshal(input, &in); err != nil {
		return errResult(err.Error())
	}
	script := strings.TrimSpace(in.Script)
	if script == "" {
		return errResult("mcp_code_exec needs a `script`.")
	}
	if stdruntime.GOOS == "windows" {
		return errResult("mcp_code_exec is not available on Windows (no fd-pipe bridge); call the mcp tool directly")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		return errResult("mcp_code_exec unavailable: python3 not found on PATH; call the mcp tool directly")
	}
	// Risk scales with CONTAINMENT, the Anthropic model (their code_execution
	// runs confirm-free in an isolated, no-network server sandbox; Claude Code
	// auto-runs sandboxed commands and prompts only for unsandboxed ones).
	// Contained here = writes confined to repo+tmp AND no network egress — the
	// same blast radius as an edit, so Medium (auto-runs in auto mode).
	// No platform sandbox (or MEMCODE_SANDBOX=0) = arbitrary code with read +
	// egress on the host: Dangerous, like browser_eval.
	policy := codeExecPolicy(s.readOnly, s.root)
	risk, containment := permissions.Dangerous, "UNSANDBOXED (no platform sandbox) — arbitrary code"
	if sandbox.Supported(policy) {
		risk, containment = permissions.Medium, "sandboxed: writes repo-confined, no network"
	}
	if ok, reason := s.gate(ctx, risk, false, ApprovalRequest{
		Title: "Run mcp_code_exec script", Label: "MCP code exec",
		Detail: containment + " · " + clip(firstLine(script), 60),
		Risk:   risk.String(),
	}); !ok {
		return errResult("mcp_code_exec denied: " + reason)
	}
	s.toolLine(true, "MCPCodeExec", clip(firstLine(script), 60), "", false)

	res := s.runBridgedScript(ctx, python, script)
	status := fmt.Sprintf("%s, %s bridged", countNoun(res.calls, "tool call", "tool calls"), byteCount(res.bridgedBytes))
	if n := len(res.mcpCalls); n > 0 {
		status += ", " + countNoun(n, "mcp call", "mcp calls")
	}
	if res.err != nil {
		s.toolLine(true, "MCPCodeExec", "", status+", failed", true)
		detail := strings.TrimSpace(res.stderr)
		if detail == "" {
			detail = strings.TrimSpace(res.stdout)
		}
		msg := "mcp_code_exec failed: " + res.err.Error() + "\n" + tail(detail, maxToolOutput)
		if len(res.mcpCalls) > 0 {
			// Partial external mutations must never be silent: the run died, but these
			// calls already reached their servers.
			msg += "\nMCP calls already made this run (may have taken effect server-side): " + strings.Join(res.mcpCalls, ", ")
		}
		return errResult(msg)
	}
	out := strings.TrimSpace(res.stdout)
	if out == "" {
		s.toolLine(true, "MCPCodeExec", "", status+", no output", true)
		return errResult("the script printed nothing — print() the distilled result")
	}
	s.toolLine(true, "MCPCodeExec", "", status, false)
	text := truncate(out, codeExecMaxResult)
	// Read-proxy nudge: a one-call script that prints ~everything it bridged is a
	// plain read wearing a subprocess — the direct tool is cheaper AND its output
	// stays cache-resident/evictable. (The audit session did this 35/35 times.)
	// Advisory only, the established nudge pattern (stallNudge/toolLeakNudge).
	if res.calls == 1 && res.bridgedBytes > 4096 && len(out) >= res.bridgedBytes*8/10 {
		text += "\n\n[note: this script made one tool call and printed nearly all of its output — call that tool directly next time; mcp_code_exec pays off only when the script returns a REDUCTION of what it reads]"
	}
	if note := s.saveScriptSkill(ctx, in.SaveSkill, in.SaveSkillDescription, script); note != "" {
		text += "\n\n" + note
	}
	// Central redaction happens in execute(); redact here too so a secret that
	// flowed through the script can't ride stdout past the per-tool masking.
	return textResult(s.redactor.Redact(text))
}

// bridgedRun is the outcome of one scripted run: the script's own output plus
// how much tool traffic stayed script-side (the token savings, made visible).
// mcpCalls is the run's external-call log — partial mutations must never be
// silent, so on failure it rides the error back to the model.
type bridgedRun struct {
	stdout, stderr string
	calls          int
	bridgedBytes   int
	mcpCalls       []string
	err            error
}

func (s *Session) runBridgedScript(ctx context.Context, python, script string) bridgedRun {
	var res bridgedRun
	dir, err := os.MkdirTemp("", "memcode-codeexec-")
	if err != nil {
		res.err = err
		return res
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "script.py")
	if res.err = os.WriteFile(path, []byte(codeExecPrelude+"\n"+script+"\n"), 0o600); res.err != nil {
		return res
	}

	// child fd 3 = write requests, child fd 4 = read responses.
	reqR, reqW, err := os.Pipe()
	if err != nil {
		res.err = err
		return res
	}
	respR, respW, err := os.Pipe()
	if err != nil {
		reqR.Close()
		reqW.Close()
		res.err = err
		return res
	}

	// CPU is capped kernel-side (ulimit -t, inside the sandbox wrapper) so runaway compute
	// dies deterministically without a wall clock — the wall clock must stay generous
	// because it also covers human approval waits on bridged MCP calls.
	line := fmt.Sprintf("ulimit -t %d; %s %s", codeExecCPULimit, python, shq(path))
	if wrapped, ok := sandbox.Wrap(line, s.codeExecWrapPolicy()); ok {
		line = wrapped
	}
	cmd := shellCmd(line)
	// The script's own file I/O runs in a PERSISTENT scratch workspace (the
	// canonical code-execution pattern: cache intermediates across runs —
	// .memcode is gitignored, so nothing dirties the tree). Bridge tool paths
	// are unaffected: handlers resolve those against the repo root.
	cmd.Dir = filepath.Join(s.root, ".memcode", "codeexec")
	if err := os.MkdirAll(cmd.Dir, 0o755); err != nil {
		cmd.Dir = s.root
	}
	cmd.ExtraFiles = []*os.File{reqW, respR}
	outBuf := &cappedBuffer{max: maxScriptOutput}
	errBuf := &cappedBuffer{max: maxScriptOutput}
	cmd.Stdout = outBuf
	cmd.Stderr = errBuf

	rctx, cancel := context.WithTimeout(ctx, codeExecTotalLimit)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.serveBridge(rctx, reqR, respW, &res)
	}()

	runErr := jobs.RunForeground(rctx, cmd)
	// Close the parent copies of the child's ends so the bridge reader sees EOF,
	// then wait for it before reading the counters.
	reqW.Close()
	respR.Close()
	<-done
	reqR.Close()
	respW.Close()

	res.stdout = outBuf.String()
	res.stderr = errBuf.String()
	if runErr != nil {
		if rctx.Err() != nil && ctx.Err() == nil {
			res.err = fmt.Errorf("script exceeded the %s total time limit (compute is separately capped at %ds CPU)", codeExecTotalLimit, codeExecCPULimit)
		} else {
			res.err = runErr
		}
	}
	return res
}

// serveBridge answers the script's tool requests, one JSON line each way, until
// the request pipe closes. Builtin requests dispatch CONCURRENTLY (bounded by
// the same cap as normal parallel tool batches) so a script's gather() fans out
// for real — the PTC pattern; the builtin whitelist is read-only, so this is
// safe. MCP requests route through the SAME invokeMCP gate as a direct call
// (the user's first Execute covers the tool for the rest of the run, so loops
// prompt once) and run SERIALLY per server — concurrent calls against an
// external service are not a race worth having. Replies carry ids and may
// return out of order; the Python side routes by id.
func (s *Session) serveBridge(ctx context.Context, req *os.File, resp *os.File, res *bridgedRun) {
	grants := newMCPRunGrants()
	var serverLocks sync.Map // server name → *sync.Mutex (serial per server)
	var mu sync.Mutex        // guards resp writes + res counters
	var wg sync.WaitGroup
	sem := make(chan struct{}, maxParallelTools)
	reply := func(id int, result, errText string) {
		mu.Lock()
		defer mu.Unlock()
		writeBridgeReply(resp, id, result, errText)
	}
	countBytes := func(n int) {
		mu.Lock()
		res.bridgedBytes += n
		mu.Unlock()
	}
	sc := bufio.NewScanner(req)
	sc.Buffer(make([]byte, 64*1024), maxBridgeLine)
	for sc.Scan() {
		var r struct {
			ID   int             `json:"id"`
			Tool string          `json:"tool"`
			Args json.RawMessage `json:"args"`
		}
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			// A malformed request line carries no id the Python side could match a
			// reply to — an id-0 reply just parked the error in _replies while the
			// script waited forever on its own id (until the 15m total limit).
			// Close the response pipe instead: the prelude's readline sees EOF and
			// raises "memcode bridge closed", failing the run loudly and promptly.
			s.printf("⚠ mcp_code_exec bridge: malformed request (%v) — closing the bridge\n", err)
			mu.Lock()
			resp.Close()
			mu.Unlock()
			break
		}
		mu.Lock()
		res.calls++
		over := res.calls > maxBridgeCalls
		mu.Unlock()
		if over {
			reply(r.ID, "", fmt.Sprintf("bridge call limit reached (%d)", maxBridgeCalls))
			continue
		}
		if ctx.Err() != nil {
			reply(r.ID, "", "cancelled")
			break
		}
		args := r.Args
		if len(args) == 0 {
			args = json.RawMessage("{}")
		}
		switch {
		case r.Tool == "search_tools" || r.Tool == "tool_schema":
			// Discovery is ungated catalog metadata and cheap — answered inline.
			var in struct{ Query, Name string }
			if err := json.Unmarshal(args, &in); err != nil {
				s.printf("⚠ malformed tool input (%v) — using defaults\n", err)
			}
			var out string
			if r.Tool == "search_tools" {
				out = s.mcpSearch(in.Query)
			} else {
				out = s.mcpSchema(in.Name)
			}
			countBytes(len(out))
			reply(r.ID, out, "")
		case s.mcp.Has(r.Tool):
			wg.Add(1)
			go func(id int, name string, args json.RawMessage) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				s.bridgeMCPCall(ctx, id, name, args, grants, &serverLocks, &mu, res, reply)
			}(r.ID, r.Tool, args)
		default:
			reply(r.ID, "", "tool not available in mcp_code_exec: "+r.Tool)
		}
	}
	wg.Wait()
}

// bridgeMCPCall runs one MCP request from a script: serial per server, bounded by the run's
// cumulative remote-time budget (transport time only — a pending approval card costs nothing
// here), gated by invokeMCP exactly like a direct call, reply truncated at maxBridgeReply,
// and logged to the run's external-call trail.
func (s *Session) bridgeMCPCall(ctx context.Context, id int, name string, args json.RawMessage,
	grants *mcpRunGrants, serverLocks *sync.Map, mu *sync.Mutex, res *bridgedRun, reply func(int, string, string)) {
	t, ok := s.mcp.Lookup(name)
	if !ok {
		reply(id, "", "tool not available in mcp_code_exec: "+name)
		return
	}
	lockAny, _ := serverLocks.LoadOrStore(t.Server, &sync.Mutex{})
	lock := lockAny.(*sync.Mutex)
	lock.Lock()
	defer lock.Unlock()
	grants.mu.Lock()
	overBudget := grants.callTime > codeExecMCPBudget
	grants.mu.Unlock()
	if overBudget {
		reply(id, "", fmt.Sprintf("mcp time budget for this run exhausted (%s) — distill what you already have", codeExecMCPBudget))
		return
	}
	tr := s.invokeMCP(ctx, mcpOriginBridge, name, args, grants)
	text := tr.text()
	if n := len(text); n > maxBridgeReply {
		text = textutil.ClipBytes(text, maxBridgeReply) + fmt.Sprintf("\n…(truncated, %d bytes total)", n)
	}
	outcome := "ok"
	if tr.isError {
		outcome = "error"
	}
	mu.Lock()
	res.bridgedBytes += len(text)
	res.mcpCalls = append(res.mcpCalls, t.Server+"/"+t.Raw+" "+outcome)
	mu.Unlock()
	if tr.isError {
		reply(id, "", text)
	} else {
		reply(id, text, "")
	}
}

func writeBridgeReply(w *os.File, id int, result, errText string) {
	reply := map[string]any{"id": id}
	if errText != "" {
		reply["error"] = errText
	} else {
		reply["result"] = result
	}
	b, err := json.Marshal(reply)
	if err != nil {
		b = []byte(`{"error":"bridge marshal failure"}`)
	}
	_, _ = w.Write(append(b, '\n'))
}

// codeExecPolicy always requests containment WITH network denial (this is
// generated code — unlike bash the sandbox is not opt-in and scripts get no
// egress), honoring only the explicit off switch. The gate above scales to
// whether this policy can actually engage (sandbox.Supported).
func codeExecPolicy(readOnly bool, root string) sandbox.Policy {
	switch strings.ToLower(os.Getenv(sandbox.EnvVar)) {
	case "0", "off", "false":
		return sandbox.Policy{Mode: sandbox.Off}
	}
	if readOnly {
		return sandbox.Policy{Mode: sandbox.ReadOnly, Root: root, DenyNetwork: true}
	}
	return sandbox.Policy{Mode: sandbox.Workspace, Root: root, DenyNetwork: true}
}

func (s *Session) codeExecWrapPolicy() sandbox.Policy { return codeExecPolicy(s.readOnly, s.root) }

var skillSlugRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// saveScriptSkill persists a proven script as a reusable skill under
// .memcode/skills/<slug>/SKILL.md — the procedural-memory half of the loop. Only
// on request (the tool description scopes save_skill to an explicit user ask),
// only after a successful run, and gated like an edit (Medium) since it's an
// in-repo, git-recoverable file write. Returns a note for the tool result, or ""
// when no save was requested.
func (s *Session) saveScriptSkill(ctx context.Context, slug, desc, script string) string {
	if slug == "" {
		return ""
	}
	if !skillSlugRe.MatchString(slug) {
		return "skill not saved: save_skill must be a lowercase-hyphen slug (got " + clip(slug, 40) + ")"
	}
	if strings.TrimSpace(desc) == "" {
		return "skill not saved: save_skill_description is required"
	}
	if ok, reason := s.gate(ctx, permissions.Medium, false, ApprovalRequest{
		Title: "Save script as skill " + slug, Label: "Save skill", Detail: clip(desc, 80),
		Risk: permissions.Medium.String(),
	}); !ok {
		return "skill not saved: " + reason
	}
	dir := filepath.Join(s.root, ".memcode", "skills", slug)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "skill not saved: " + err.Error()
	}
	body := "---\nname: " + slug + "\ndescription: " + strings.TrimSpace(desc) + "\n---\n\n" +
		"Run this investigation with the mcp_code_exec tool. Adapt paths/queries to the ask, keep the printed result compact.\n\n" +
		"```python\n" + script + "\n```\n"
	if err := atomicfile.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
		return "skill not saved: " + err.Error()
	}
	s.toolLine(true, "Write", filepath.Join(".memcode", "skills", slug, "SKILL.md"), "skill saved", false)
	return "saved as skill `" + slug + "` (.memcode/skills/" + slug + "/SKILL.md)"
}

// cappedBuffer keeps the first max bytes and drops the rest — a script that
// prints megabytes must not balloon memory; the result is truncated anyway.
type cappedBuffer struct {
	buf     []byte
	max     int
	dropped bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if room := b.max - len(b.buf); room > 0 {
		if len(p) > room {
			b.buf = append(b.buf, p[:room]...)
			b.dropped = true
		} else {
			b.buf = append(b.buf, p...)
		}
	} else if len(p) > 0 {
		b.dropped = true
	}
	return len(p), nil
}

func (b *cappedBuffer) String() string {
	if b.dropped {
		return string(b.buf) + "\n…(truncated)"
	}
	return string(b.buf)
}

// byteCount renders a byte count human-readably for the tool status line.
func byteCount(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1fMB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1fKB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%dB", n)
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

// shq single-quotes s for POSIX shells (matches sandbox.shq; local copy since
// that helper is unexported and this file builds the python invocation line).
func shq(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
