package personal

import (
	"context"
	"encoding/json"
	"path/filepath"

	yaml "go.yaml.in/yaml/v4"

	"github.com/memcode-ai/memcode/internal/atomicfile"
)

// WriteConfigMirror regenerates config.yaml — ONE readable file in the agent's
// home holding the authority state that lives in the database: its policies
// (draft and approved) and its resource grants. This is what makes
// `ls ~/.memcode/agents/<name>/` show something a person can read, diff, and
// grep instead of only an opaque SQLite file, matching every other piece of
// memcode config (gateway.yaml, .mcp.json, MEMCODE.md, skills).
//
// The agent's objective, autonomy, browser mode, and pause state are NOT here:
// they are ordinary configuration in gateway.yaml, which is already a readable
// file. Mirroring them too would mean two places to look and two chances to
// disagree.
//
// This file is a MIRROR, not the source of truth — the DB stays authoritative
// for two reasons that are correctness, not habit:
//   - Policy approval is a deliberate hash-gated ceremony (see
//     ApprovePolicy): a Personal Agent runs unsupervised, so "the document a
//     human actually reviewed" must be pinned by hash, not re-derived from
//     whatever the file happens to say at wake time. Editing config.yaml's
//     policy section and having it silently take effect would defeat that.
//   - The action/trigger/interaction journal needs atomic claim/complete
//     semantics under concurrent access (the gateway wake loop and an admin
//     session can both touch the same agent) — a SQL transaction gives that
//     almost for free; a flat file would need to reinvent it (see the
//     atomicfile-write fix elsewhere in this package for how easily a plain
//     file write loses that property). So the run journal stays out of this
//     file entirely — read it with gw_journal.
//
// Called after every mutation to policies/resources (gw_policy, gw_grant),
// best-effort: a mirror failure never blocks the underlying write, which has
// already succeeded.
func WriteConfigMirror(ctx context.Context, home string, s *Store) error {
	type policyView struct {
		Hash     string         `yaml:"hash"`
		Status   string         `yaml:"status"`
		Version  int            `yaml:"version"`
		Approved bool           `yaml:"approved"`
		Document map[string]any `yaml:"document"`
	}
	type resourceView struct {
		ID         string `yaml:"id"`
		Type       string `yaml:"type"`
		Locator    string `yaml:"locator"`
		AccessMode string `yaml:"access_mode"`
		Status     string `yaml:"status"`
	}
	cfg := struct {
		Policies  []policyView   `yaml:"policies,omitempty"`
		Resources []resourceView `yaml:"resources,omitempty"`
	}{}

	policies, err := s.ListPolicies(ctx, "primary")
	if err != nil {
		return err
	}
	for _, p := range policies {
		var doc map[string]any
		_ = json.Unmarshal(p.Document, &doc)
		cfg.Policies = append(cfg.Policies, policyView{Hash: p.Hash, Status: p.Status, Version: p.Version, Approved: p.Status == "approved", Document: doc})
	}

	res, err := s.ListResources(ctx, "primary")
	if err != nil {
		return err
	}
	for _, r := range res {
		cfg.Resources = append(cfg.Resources, resourceView{ID: r.ID, Type: r.Type, Locator: r.Locator, AccessMode: r.AccessMode, Status: r.Status})
	}

	b, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	return atomicfile.WriteFile(filepath.Join(home, "config.yaml"), b, 0o600)
}
