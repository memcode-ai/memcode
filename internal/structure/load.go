package structure

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"github.com/memcode-ai/memcode/internal/store"
)

const subsystemPrefix = "subsystem:"

// Load reconstructs the topology from the materialized projection (the
// subsystem entities and depends_on edges), plus repo-level metadata from the
// structural state snapshot. Read-only — it does not re-scan the filesystem.
func Load(ctx context.Context, s store.Store) (Result, error) {
	ents, err := s.ListEntities(ctx, "subsystem")
	if err != nil {
		return Result{}, err
	}

	var res Result
	for _, e := range ents {
		var sub Subsystem
		if len(e.Attrs) > 0 {
			_ = json.Unmarshal(e.Attrs, &sub)
		}
		if sub.Key == "" {
			sub.Key = e.Key
		}
		res.Subsystems = append(res.Subsystems, sub)
	}
	sort.Slice(res.Subsystems, func(i, j int) bool {
		return res.Subsystems[i].Key < res.Subsystems[j].Key
	})

	edges, err := s.ListEdges(ctx, store.EdgeFilter{Kind: "depends_on"})
	if err != nil {
		return Result{}, err
	}
	for _, e := range edges {
		res.Deps = append(res.Deps, Dependency{From: SubsystemKey(e.Src), To: SubsystemKey(e.Dst)})
	}
	sort.Slice(res.Deps, func(i, j int) bool {
		if res.Deps[i].From != res.Deps[j].From {
			return res.Deps[i].From < res.Deps[j].From
		}
		return res.Deps[i].To < res.Deps[j].To
	})

	// Repo-level metadata (docs, generated_at, root) lives in the snapshot.
	if st, ok, err := s.GetState(ctx, "repo", "structural"); err == nil && ok {
		var snap Result
		if json.Unmarshal(st.Body, &snap) == nil {
			res.Root = snap.Root
			res.GeneratedAt = snap.GeneratedAt
			res.Docs = snap.Docs
		}
	}
	return res, nil
}

// EntityID returns the entity id for a subsystem key.
func EntityID(key string) string { return subsystemPrefix + key }

// SubsystemKey strips the "subsystem:" prefix from an entity id.
func SubsystemKey(id string) string { return strings.TrimPrefix(id, subsystemPrefix) }
