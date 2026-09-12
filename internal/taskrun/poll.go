package taskrun

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/memcode-ai/memcode/internal/task"
)

// Poll is the unattended entry point: given the tasks that exist and the
// current time, work out what is due and run it.
//
// It is a POLL rather than a set of timers on purpose. A timer that was not
// running when it should have fired leaves no trace, which is the whole problem
// on a laptop; recomputing due occurrences from the calendar and a persisted
// watermark gives the same answer whether the machine slept through one firing
// or a hundred, and gives it again identically after a restart.
//
// Safe to call from more than one process. Everything that must not happen
// twice is guarded durably — the occurrence index for runs, the lease for
// mutation — rather than by assuming a single caller.

// PollResult reports what one poll did.
type PollResult struct {
	Started []Run
	// Skipped counts occurrences deliberately not run, across all triggers.
	Skipped int
	// Errs holds per-task failures; one bad task never stops the others.
	Errs []error
}

// Poll fires everything due. It returns once every started run has finished,
// so a caller that wants concurrency across tasks runs Poll in a goroutine.
func (r *Runner) Poll(ctx context.Context, tasks []task.Task, root string, now time.Time) PollResult {
	var res PollResult
	for _, t := range tasks {
		if !t.IsEnabled() {
			continue
		}
		runs, skipped, errs := r.pollTask(ctx, t, root, now)
		res.Started = append(res.Started, runs...)
		res.Skipped += skipped
		res.Errs = append(res.Errs, errs...)
	}
	return res
}

func (r *Runner) pollTask(ctx context.Context, t task.Task, root string, now time.Time) ([]Run, int, []error) {
	var started []Run
	var errs []error
	skipped := 0

	// A paused task does not fire. It stopped because something needed a
	// person, and that has not changed just because the clock came round again
	// — firing anyway would rebuild the identical failure every week and bury
	// the decision under copies of itself.
	if p, ok, err := r.Store.PausedTask(ctx, t.Name, root); err != nil {
		errs = append(errs, fmt.Errorf("%s: %w", t.Name, err))
		return nil, 0, errs
	} else if ok {
		_ = p
		return nil, 0, nil
	}

	for _, tr := range t.Triggers {
		if tr.Manual {
			continue
		}
		key := TriggerKey(tr)
		last, err := r.Store.Watermark(ctx, t.Name, key)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", t.Name, err))
			continue
		}
		decision, err := Due(tr, last, now)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", t.Name, err))
			continue
		}
		skipped += decision.Dropped

		for i, at := range decision.Fire {
			// The backlog is attributed to the FIRST run of a catch-up batch, so
			// it is reported once rather than repeated on every run in the batch.
			backlog := 0
			if i == 0 {
				backlog = decision.Dropped
			}
			run, err := r.fireOccurrence(ctx, t, root, tr, at, now, backlog, decision.Why)
			if err != nil {
				if errors.Is(err, ErrOccupied) {
					// Another process already owns this occurrence. Not an error:
					// this is the idempotency boundary working.
					continue
				}
				errs = append(errs, fmt.Errorf("%s: %w", t.Name, err))
				continue
			}
			started = append(started, run)
		}

		// Advance the watermark even when nothing ran. A dropped occurrence that
		// stays unaccounted for is rediscovered as missed on every subsequent
		// poll, forever.
		if !decision.Advance.IsZero() {
			if err := r.Store.SetWatermark(ctx, t.Name, key, decision.Advance, now); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", t.Name, err))
			}
		}
	}
	return started, skipped, errs
}

// fireOccurrence creates and executes one logical occurrence.
func (r *Runner) fireOccurrence(ctx context.Context, t task.Task, root string, tr task.Trigger,
	at, now time.Time, backlog int, why string,
) (Run, error) {
	frozen, err := FreezeAt(t, root, Spec(tr).Kind(), OccurrenceID(tr, at), at, now, backlog)
	if err != nil {
		return Run{}, err
	}
	run, err := r.Store.Create(ctx, frozen)
	if err != nil {
		return Run{}, err // includes ErrOccupied
	}
	if why != "" {
		// Record WHY this run exists before it starts, so a recovered or
		// collapsed occurrence explains itself even if the run then crashes.
		_ = r.Store.Note(ctx, run.ID, why)
	}
	return r.Execute(ctx, run, t)
}
