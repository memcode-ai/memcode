package personal

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"

	yaml "go.yaml.in/yaml/v4"

	"github.com/memcode-ai/memcode/internal/atomicfile"
)

// WriteConfigMirror regenerates the agent home's human-readable config files
// from the current DB state: objective.md, policy.yaml, resources.yaml. This
// is what makes `ls ~/.memcode/agents/<name>/` show something a person can
// actually read, diff, and grep, instead of only a personal.db blob reachable
// through bespoke CLI commands — every OTHER piece of memcode config
// (gateway.yaml, .mcp.json, CLAUDE.md, skills) is a plain file; Personal
// Agents' setup/config surface should be too.
//
// These files are a MIRROR, not the source of truth — the DB stays
// authoritative for two reasons that are correctness, not habit:
//   - Policy approval is a deliberate hash-gated ceremony (see
//     ApprovePolicy): a Personal Agent runs unsupervised, so "the document a
//     human actually reviewed" must be pinned by hash, not re-derived from
//     whatever a file happens to say at wake time. Editing policy.yaml and
//     having it silently take effect would defeat that.
//   - The action/trigger/interaction journal needs atomic claim/complete
//     semantics under concurrent access (the gateway wake loop, the CLI, and
//     the cockpit can all touch the same agent) — a SQL transaction gives
//     that almost for free; flat files would need to reinvent it (see the
//     atomicfile-write fix elsewhere in this package for how easily a plain
//     file write loses that property).
//
// So: objective/policy/resources — the SETUP a human decides — mirror out as
// files for inspection. The RUN journal stays in personal.db. Called after
// every mutation to those three (CreateObjective, InsertResource,
// ApprovePolicy, etc.) — best-effort: a mirror failure never blocks the
// underlying DB write, which already succeeded.
func WriteConfigMirror(ctx context.Context, home string, s *Store) error {
	obj, hasObj, err := s.GetObjective(ctx, "primary")
	if err != nil {
		return err
	}
	if hasObj {
		md := fmt.Sprintf("# Objective\n\n%s\n\n**Status:** %s\n", obj.Description, obj.Status)
		if obj.SuccessCriteria != "" {
			md += fmt.Sprintf("\n**Success criteria:** %s\n", obj.SuccessCriteria)
		}
		if err := atomicfile.WriteFile(filepath.Join(home, "objective.md"), []byte(md), 0o600); err != nil {
			return err
		}
	}

	policies, err := s.ListPolicies(ctx, "primary")
	if err != nil {
		return err
	}
	type policyView struct {
		Hash, Status string
		Version      int
		Approved     bool
		Document     map[string]any `yaml:"document"`
	}
	var pv []policyView
	for _, p := range policies {
		var doc map[string]any
		_ = json.Unmarshal(p.Document, &doc)
		pv = append(pv, policyView{Hash: p.Hash, Status: p.Status, Version: p.Version, Approved: p.Status == "approved", Document: doc})
	}
	if pb, err := yaml.Marshal(map[string]any{"policies": pv}); err == nil {
		_ = atomicfile.WriteFile(filepath.Join(home, "policy.yaml"), pb, 0o600)
	}

	res, err := s.ListResources(ctx, "primary")
	if err != nil {
		return err
	}
	type resourceView struct {
		ID, Type, Locator, AccessMode, Status string
	}
	var rv []resourceView
	for _, r := range res {
		rv = append(rv, resourceView{ID: r.ID, Type: r.Type, Locator: r.Locator, AccessMode: r.AccessMode, Status: r.Status})
	}
	if rb, err := yaml.Marshal(map[string]any{"resources": rv}); err == nil {
		_ = atomicfile.WriteFile(filepath.Join(home, "resources.yaml"), rb, 0o600)
	}
	return nil
}
