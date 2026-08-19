package config

// Validated mutation helpers shared by every surface that edits gateway.yaml —
// the CLI (`memcode gateway schedule`, `memcode project`) and the admin
// session's tools (`memcode admin`). Both paths MUST go through these so their
// validation cannot drift: a schedule that parses on one surface parses on the
// other, and a project registration is symlink-safe everywhere.

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

// ParseAt accepts the ways people naturally write a one-shot time — a duration
// from now ("30m", "2h"), a local date-time ("2026-03-01T09:00"), or full
// RFC3339 — and returns the absolute RFC3339 timestamp that gets stored.
func ParseAt(s string, now time.Time) (string, error) {
	s = strings.TrimSpace(s)
	if d, err := time.ParseDuration(s); err == nil {
		if d <= 0 {
			return "", fmt.Errorf("at duration must be in the future")
		}
		return now.Add(d).Format(time.RFC3339), nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02 15:04"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			if !t.After(now) {
				return "", fmt.Errorf("at %q is in the past", s)
			}
			return t.Format(time.RFC3339), nil
		}
	}
	return "", fmt.Errorf("bad at %q: use a duration from now (30m, 2h) or a date-time (2026-03-01T09:00)", s)
}

// ValidateScheduleSpec checks that exactly one schedule form (cron/every/at) is
// set and that it parses. Returns the resolved at timestamp ("" unless at was
// used).
func ValidateScheduleSpec(cronExpr, every, at string, now time.Time) (string, error) {
	set := 0
	for _, v := range []string{cronExpr, every, at} {
		if strings.TrimSpace(v) != "" {
			set++
		}
	}
	switch {
	case set == 0:
		return "", fmt.Errorf("set cron (e.g. \"0 9 * * 1-5\"), every (e.g. 24h), or at (e.g. 30m or 2026-03-01T09:00)")
	case set > 1:
		return "", fmt.Errorf("set exactly one of cron, every, at")
	case cronExpr != "":
		if _, err := cron.ParseStandard(cronExpr); err != nil {
			return "", fmt.Errorf("bad cron %q: %w (5 fields: minute hour day-of-month month day-of-week)", cronExpr, err)
		}
		return "", nil
	case every != "":
		if _, err := time.ParseDuration(every); err != nil {
			return "", fmt.Errorf("bad every %q: %w (a Go duration like 30m or 24h)", every, err)
		}
		return "", nil
	default:
		return ParseAt(at, now)
	}
}

// ValidateDeliverTo checks the "channel:conversation" delivery address.
func ValidateDeliverTo(to string) error {
	if ch, convo, ok := strings.Cut(to, ":"); !ok || ch == "" || convo == "" {
		return fmt.Errorf("deliver_to must be \"channel:conversation\", e.g. telegram:123456")
	}
	return nil
}

// BuildSchedule validates every field of a new schedule and returns it ready
// to add: name and task required, exactly one parsing timing form, a
// well-formed deliver_to. The at form is resolved to its absolute RFC3339
// timestamp.
func BuildSchedule(name, cronExpr, every, at, tz, task, deliverTo, agent string, now time.Time) (Schedule, error) {
	name, task = strings.TrimSpace(name), strings.TrimSpace(task)
	cronExpr, every, at = strings.TrimSpace(cronExpr), strings.TrimSpace(every), strings.TrimSpace(at)
	deliverTo = strings.TrimSpace(deliverTo)
	if name == "" || task == "" {
		return Schedule{}, fmt.Errorf("a schedule needs a name and a task")
	}
	resolvedAt, err := ValidateScheduleSpec(cronExpr, every, at, now)
	if err != nil {
		return Schedule{}, err
	}
	if err := ValidateDeliverTo(deliverTo); err != nil {
		return Schedule{}, err
	}
	return Schedule{
		Name: name, Cron: cronExpr, Every: every, At: resolvedAt, TZ: strings.TrimSpace(tz),
		Task: task, DeliverTo: deliverTo, Agent: strings.TrimSpace(agent),
	}, nil
}

// AddSchedule appends a schedule, refusing a duplicate name — replacing one is
// an explicit remove-then-add (or edit) on every surface.
func (s *Settings) AddSchedule(sc Schedule) error {
	for _, existing := range s.Schedules {
		if existing.Name == sc.Name {
			return fmt.Errorf("schedule %q already exists — edit it, or remove it first to replace it", sc.Name)
		}
	}
	s.Schedules = append(s.Schedules, sc)
	return nil
}

// RegisterProject canonicalizes path (symlink-safe, ~-expanded, must be a
// directory), derives the id from the directory name when empty, and registers
// the project — refusing an id that is already registered to a DIFFERENT path,
// so a re-add of the same directory is idempotent but a collision never
// silently rebinds an id. The first registered project becomes the default.
func (s *Settings) RegisterProject(id, path string) (string, string, error) {
	root, err := CanonicalRoot(path)
	if err != nil {
		return "", "", err
	}
	if id = strings.TrimSpace(id); id == "" {
		id = filepath.Base(root)
	}
	if existing, ok := s.Projects[id]; ok && existing.Path != root {
		return "", "", fmt.Errorf("a different project is already registered as %q (%s); rename the directory or edit gateway.yaml", id, existing.Path)
	}
	if s.Projects == nil {
		s.Projects = map[string]Project{}
	}
	s.Projects[id] = Project{Path: root, Enabled: true}
	if s.DefaultProject == "" {
		s.DefaultProject = id // first registered project becomes the gateway default
	}
	return id, root, nil
}
