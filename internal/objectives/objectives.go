// Package objectives manages the human-authored goals that give the engine its
// sense of direction ("what we're trying to do"). In v1 objectives are created
// by people, never inferred — that avoids stale or hallucinated goals.
package objectives

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/memcode-ai/memcode/internal/store"
)

// Status values for an objective.
const (
	StatusProposed  = "proposed"
	StatusActive    = "active"
	StatusBlocked   = "blocked"
	StatusDone      = "done"
	StatusAbandoned = "abandoned"
)

var validStatus = map[string]bool{
	StatusProposed: true, StatusActive: true, StatusBlocked: true,
	StatusDone: true, StatusAbandoned: true,
}

// Objective is re-exported from store for callers that only need this package.
type Objective = store.Objective

// Service is the objective use-case layer over a Store.
type Service struct {
	store store.Store
	now   func() time.Time
}

// New returns a Service backed by s.
func New(s store.Store) *Service {
	return &Service{store: s, now: func() time.Time { return time.Now().UTC() }}
}

// Add creates a new objective. priority defaults sensibly; parent may be empty.
func (svc *Service) Add(ctx context.Context, title string, priority int, parentID string) (Objective, error) {
	if title == "" {
		return Objective{}, fmt.Errorf("objective title is required")
	}
	if parentID != "" {
		if _, ok, err := svc.store.GetObjective(ctx, parentID); err != nil {
			return Objective{}, err
		} else if !ok {
			return Objective{}, fmt.Errorf("parent objective %q not found", parentID)
		}
	}
	now := svc.now()
	o := Objective{
		ID:        newID(),
		Title:     title,
		Status:    StatusActive,
		Priority:  priority,
		ParentID:  parentID,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := svc.store.CreateObjective(ctx, o); err != nil {
		return Objective{}, err
	}
	return o, nil
}

// List returns all objectives, highest priority first.
func (svc *Service) List(ctx context.Context) ([]Objective, error) {
	return svc.store.ListObjectives(ctx)
}

// Current returns the objectives in flight (active or blocked).
func (svc *Service) Current(ctx context.Context) ([]Objective, error) {
	all, err := svc.store.ListObjectives(ctx)
	if err != nil {
		return nil, err
	}
	var out []Objective
	for _, o := range all {
		if o.Status == StatusActive || o.Status == StatusBlocked {
			out = append(out, o)
		}
	}
	return out, nil
}

// SetStatus transitions an objective to a new status.
func (svc *Service) SetStatus(ctx context.Context, id, status string) (Objective, error) {
	if !validStatus[status] {
		return Objective{}, fmt.Errorf("invalid status %q", status)
	}
	o, ok, err := svc.store.GetObjective(ctx, id)
	if err != nil {
		return Objective{}, err
	}
	if !ok {
		return Objective{}, fmt.Errorf("objective %q not found", id)
	}
	o.Status = status
	o.UpdatedAt = svc.now()
	if err := svc.store.UpdateObjective(ctx, o); err != nil {
		return Objective{}, err
	}
	return o, nil
}

// newID returns a short, collision-resistant objective id like "obj_9f3a1c".
func newID() string {
	var b [3]byte
	_, _ = rand.Read(b[:])
	return "obj_" + hex.EncodeToString(b[:])
}
