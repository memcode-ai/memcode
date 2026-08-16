// Cron-job migration. Hermes persists jobs at ~/.hermes/cron/jobs.json in a
// documented shape that maps cleanly onto our schedules (its deliver target is
// literally our deliver_to format). OpenClaw's current versions keep jobs in an
// internal SQLite database whose schema is not published — guessing at it would
// produce silently wrong migrations, so those get an explicit recreate note —
// but its legacy ~/.openclaw/cron/jobs.json is parseable and carried over.
// Anything that can't be carried (script payloads, skill-injected jobs) becomes
// a note, never a silent drop.
package importer

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	gwconfig "github.com/memcode-ai/memcode/internal/gateway/config"
)

// hermesJob is the subset of a Hermes ~/.hermes/cron/jobs.json record we read.
type hermesJob struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Prompt   string   `json:"prompt"`
	Deliver  string   `json:"deliver"` // "telegram:-100123" — same shape as our deliver_to
	Enabled  bool     `json:"enabled"`
	State    string   `json:"state"` // scheduled | paused | completed | running
	Skills   []string `json:"skills"`
	Script   any      `json:"script"`
	Schedule struct {
		Kind string `json:"kind"` // cron | every | at | delay
		Expr string `json:"expr"`
	} `json:"schedule"`
}

// HermesSchedules reads a Hermes cron jobs.json and maps each job to a memcode
// schedule. Completed one-shots are skipped (they already ran); paused/disabled
// jobs carry over disabled so nothing starts firing that wasn't firing before.
func HermesSchedules(data []byte) ([]gwconfig.Schedule, []string) {
	var jobs []hermesJob
	if err := json.Unmarshal(data, &jobs); err != nil {
		// Some versions wrap the list: {"jobs": [...]}.
		var wrap struct {
			Jobs []hermesJob `json:"jobs"`
		}
		if err2 := json.Unmarshal(data, &wrap); err2 != nil || wrap.Jobs == nil {
			return nil, []string{fmt.Sprintf("cron: could not parse Hermes jobs.json (%v) — recreate jobs with `memcode gateway schedule add`", err)}
		}
		jobs = wrap.Jobs
	}
	var out []gwconfig.Schedule
	var notes []string
	for _, j := range jobs {
		name := slugify(firstNonEmpty(j.Name, j.ID))
		if name == "" || strings.TrimSpace(j.Prompt) == "" {
			notes = append(notes, "cron: skipped a Hermes job with no name or prompt")
			continue
		}
		if j.State == "completed" {
			continue // a finished one-shot; nothing left to migrate
		}
		if j.Script != nil {
			notes = append(notes, fmt.Sprintf("cron: Hermes job %q runs a script payload, which memcode schedules don't execute — recreate it as a saved script or a plain-language task", name))
			continue
		}
		sch := gwconfig.Schedule{
			Name:      name,
			Task:      strings.TrimSpace(j.Prompt),
			DeliverTo: strings.TrimSpace(j.Deliver),
			Disabled:  !j.Enabled || j.State == "paused",
		}
		expr := strings.TrimSpace(j.Schedule.Expr)
		switch j.Schedule.Kind {
		case "cron":
			sch.Cron = expr
		case "every":
			sch.Every = strings.TrimPrefix(expr, "every ")
		case "at", "delay", "once":
			t, err := parseFlexibleTime(expr)
			if err != nil {
				notes = append(notes, fmt.Sprintf("cron: Hermes one-shot %q has an unreadable time %q — recreate with `memcode gateway schedule add %s --at ...`", name, expr, name))
				continue
			}
			if !t.After(time.Now()) {
				continue // due date already passed; nothing to carry
			}
			sch.At = t.Format(time.RFC3339)
		default:
			notes = append(notes, fmt.Sprintf("cron: Hermes job %q has schedule kind %q, which didn't map — recreate it with `memcode gateway schedule add`", name, j.Schedule.Kind))
			continue
		}
		if sch.DeliverTo == "" {
			notes = append(notes, fmt.Sprintf("cron: Hermes job %q has no delivery target — imported disabled; set deliver_to and enable it", name))
			sch.DeliverTo = "telegram:set-me"
			sch.Disabled = true
		}
		if len(j.Skills) > 0 {
			notes = append(notes, fmt.Sprintf("cron: job %q used Hermes skills (%s); imported memcode skills join discovery automatically", name, strings.Join(j.Skills, ", ")))
		}
		out = append(out, sch)
	}
	return out, notes
}

// ocJob is the subset of a legacy OpenClaw ~/.openclaw/cron/jobs.json record we
// read. Field names per OpenClaw's automation docs.
type ocJob struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Message  string `json:"message"`
	Command  string `json:"command"`
	Script   any    `json:"script"`
	Channel  string `json:"channel"`
	To       string `json:"to"`
	Enabled  *bool  `json:"enabled"`
	Schedule struct {
		Kind string `json:"kind"` // at | every | cron | on-exit | stream
		Expr string `json:"expr"`
		At   string `json:"at"`
		Cron string `json:"cron"`
	} `json:"schedule"`
}

// OpenClawSchedules reads a legacy OpenClaw cron jobs.json. Current OpenClaw
// versions store jobs in an internal database instead; ImportOpenClawSchedules
// handles detecting that and telling the user what to do.
func OpenClawSchedules(data []byte) ([]gwconfig.Schedule, []string) {
	var jobs []ocJob
	if err := json.Unmarshal(data, &jobs); err != nil {
		var wrap struct {
			Jobs []ocJob `json:"jobs"`
		}
		if err2 := json.Unmarshal(data, &wrap); err2 != nil || wrap.Jobs == nil {
			return nil, []string{fmt.Sprintf("cron: could not parse OpenClaw jobs.json (%v) — run `openclaw automations list` and recreate with `memcode gateway schedule add`", err)}
		}
		jobs = wrap.Jobs
	}
	var out []gwconfig.Schedule
	var notes []string
	for _, j := range jobs {
		name := slugify(firstNonEmpty(j.Name, j.ID))
		recreate := func(why string) {
			notes = append(notes, fmt.Sprintf("cron: OpenClaw job %q %s — recreate it with `memcode gateway schedule add`", name, why))
		}
		if name == "" {
			notes = append(notes, "cron: skipped an OpenClaw job with no name or id")
			continue
		}
		if j.Script != nil || strings.TrimSpace(j.Command) != "" {
			recreate("runs a command/script payload, which memcode schedules don't execute")
			continue
		}
		if strings.TrimSpace(j.Message) == "" {
			recreate("has no message payload")
			continue
		}
		sch := gwconfig.Schedule{
			Name:      name,
			Task:      strings.TrimSpace(j.Message),
			DeliverTo: strings.TrimSpace(j.Channel) + ":" + strings.TrimSpace(j.To),
			Disabled:  j.Enabled != nil && !*j.Enabled,
		}
		switch j.Schedule.Kind {
		case "cron":
			sch.Cron = firstNonEmpty(j.Schedule.Cron, j.Schedule.Expr)
		case "every":
			sch.Every = strings.TrimPrefix(firstNonEmpty(j.Schedule.Expr), "every ")
		case "at":
			t, err := parseFlexibleTime(firstNonEmpty(j.Schedule.At, j.Schedule.Expr))
			if err != nil {
				recreate("has an unreadable one-shot time")
				continue
			}
			if !t.After(time.Now()) {
				continue // already due; nothing to carry
			}
			sch.At = t.Format(time.RFC3339)
		default:
			recreate(fmt.Sprintf("has schedule kind %q, which didn't map", j.Schedule.Kind))
			continue
		}
		if j.Channel == "" || j.To == "" {
			notes = append(notes, fmt.Sprintf("cron: OpenClaw job %q has no delivery route — imported disabled; set deliver_to and enable it", name))
			sch.DeliverTo = "telegram:set-me"
			sch.Disabled = true
		}
		out = append(out, sch)
	}
	return out, notes
}

// ImportOpenClawSchedules finds OpenClaw cron jobs under dir: the legacy
// cron/jobs.json is migrated; a jobs database (current versions) can't be read
// safely, so it becomes an explicit instruction instead of a silent gap.
func ImportOpenClawSchedules(dir string) ([]gwconfig.Schedule, []string) {
	if data, err := os.ReadFile(filepath.Join(dir, "cron", "jobs.json")); err == nil {
		return OpenClawSchedules(data)
	}
	if hasFile(filepath.Join(dir, "cron")) || hasFile(filepath.Join(dir, "openclaw.db")) {
		return nil, []string{"cron: your OpenClaw automations live in its internal database, which can't be read directly — run `openclaw automations list` and recreate each with `memcode gateway schedule add` (see memcode.ai/docs/agents/examples for ready-made recipes)"}
	}
	return nil, nil
}

// ImportHermesSchedules finds Hermes cron jobs under dir (~/.hermes).
func ImportHermesSchedules(dir string) ([]gwconfig.Schedule, []string) {
	data, err := os.ReadFile(filepath.Join(dir, "cron", "jobs.json"))
	if err != nil {
		return nil, nil // no cron jobs; nothing to migrate, nothing to report
	}
	return HermesSchedules(data)
}

func hasFile(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// slugify makes a job name safe as a schedule name (lowercase, dashes).
func slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	return strings.TrimSuffix(b.String(), "-")
}

// parseFlexibleTime accepts the timestamp shapes both tools write.
func parseFlexibleTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02 15:04"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized time %q", s)
}
