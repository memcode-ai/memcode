package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
)

// Project-scoped servers come from a checked-in .mcp.json — repo content memcode must not
// trust by default (a teammate, or a malicious PR, could point one at anything). So, exactly
// like Claude Code, memcode requires explicit approval before connecting a project server, and
// remembers the choice keyed to the server's CONFIG: if the .mcp.json entry later changes, the
// recorded hash no longer matches and approval is required again. Local/user servers are added
// by you and need no approval. Choices persist in <root>/.memcode/mcp-approvals.json and are
// cleared with `memcode mcp reset-project-choices`.
//
// The same record also carries INVOCATION grants ("Execute and remember" / "Don't ask again
// for <server>" on the call card). Connect trust and call permission are different decisions:
// adding a server only lets it advertise tools; every call still prompts until remembered here.
// Grants key to the same config hash, so any config edit resets them along with connect trust.

// Decision is a remembered approval choice for a project-scoped server.
type Decision string

const (
	Approved Decision = "approved"
	Rejected Decision = "rejected"
)

type approvalRecord struct {
	Decision Decision `json:"decision"`
	Hash     string   `json:"hash"` // config hash the decision was made against
	// Invocation grants (see the header): every call, or specific remembered tools (raw
	// server-side names). Plaintext on purpose — the file is the user's to inspect and edit.
	CallsAll  bool     `json:"calls_all,omitempty"`
	CallTools []string `json:"call_tools,omitempty"`
}

// Approvals is the persisted set of project-server choices for one project.
type Approvals map[string]approvalRecord

// ApprovalsPath is where a project's MCP approval choices live.
func ApprovalsPath(root string) string {
	return filepath.Join(root, ".memcode", "mcp-approvals.json")
}

// LoadApprovals reads the remembered choices (absent file → empty, not an error).
func LoadApprovals(root string) Approvals {
	a := Approvals{}
	b, err := os.ReadFile(ApprovalsPath(root))
	if err != nil {
		return a
	}
	_ = json.Unmarshal(b, &a)
	if a == nil {
		a = Approvals{}
	}
	return a
}

// Status reports a project server's standing given the current config: "approved" only when a
// stored approval matches the live config hash; "rejected" when explicitly rejected (and still
// matching); otherwise "pending" (never decided, or the config changed since).
func (a Approvals) Status(name string, cfg ServerConfig) Decision {
	rec, ok := a[name]
	if !ok || rec.Hash != ConfigHash(cfg) {
		return "" // pending
	}
	return rec.Decision
}

// SaveApproval records a decision for a project server, hashed against its current config.
// A same-config re-approve keeps any invocation grants; a rejection or config change drops them.
func SaveApproval(root, name string, cfg ServerConfig, d Decision) error {
	a := LoadApprovals(root)
	h := ConfigHash(cfg)
	rec := approvalRecord{Decision: d, Hash: h}
	if old, ok := a[name]; ok && old.Hash == h && d == Approved {
		rec.CallsAll, rec.CallTools = old.CallsAll, old.CallTools
	}
	a[name] = rec
	return writeJSON(ApprovalsPath(root), a)
}

// CallAllowed reports whether invoking rawTool on the named server was remembered. False the
// moment the config hash stops matching — grants die with the config they were made against.
func (a Approvals) CallAllowed(name string, cfg ServerConfig, rawTool string) bool {
	rec, ok := a[name]
	if !ok || rec.Hash != ConfigHash(cfg) {
		return false
	}
	return rec.CallsAll || slices.Contains(rec.CallTools, rawTool)
}

// RememberCalls persists an invocation grant: rawTool == "" remembers the whole server
// ("Don't ask again for <server>"), otherwise the one tool ("Execute and remember").
// For a local/user server this creates the record (those scopes are never connect-gated,
// so the record exists purely to carry grants); a stale-hash record is replaced, not merged.
func RememberCalls(root, name string, cfg ServerConfig, rawTool string) error {
	a := LoadApprovals(root)
	h := ConfigHash(cfg)
	rec, ok := a[name]
	if !ok || rec.Hash != h {
		rec = approvalRecord{Decision: Approved, Hash: h}
	}
	if rawTool == "" {
		rec.CallsAll, rec.CallTools = true, nil
	} else if !rec.CallsAll && !slices.Contains(rec.CallTools, rawTool) {
		rec.CallTools = append(rec.CallTools, rawTool)
	}
	a[name] = rec
	return writeJSON(ApprovalsPath(root), a)
}

// ResetApprovals clears all project-server choices (Claude Code's reset-project-choices).
func ResetApprovals(root string) error {
	err := os.Remove(ApprovalsPath(root))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// ConfigHash is a stable digest of a server's RAW config, so any edit to its .mcp.json entry
// invalidates a prior approval (re-prompting), while environment changes (which don't alter the
// committed config) do not.
func ConfigHash(cfg ServerConfig) string {
	b, _ := json.Marshal(cfg)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}
