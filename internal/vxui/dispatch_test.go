package vxui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/memcode-ai/memcode/internal/jobs"
)

// A report-back agent (agent{background}) that transitions running→done yields a report-back
// carrying its persisted result, so the caller can feed it to the LLM. A non-report-back job
// (legacy dispatch) yields only the user notification, never a report-back.
func TestAgentReportBackOnFinish(t *testing.T) {
	root := t.TempDir()
	writeJob := func(j jobs.Job) {
		jd := filepath.Join(root, ".memcode", "jobs", j.ID)
		if err := os.MkdirAll(jd, 0o755); err != nil {
			t.Fatal(err)
		}
		b, _ := json.MarshalIndent(j, "", "  ")
		if err := os.WriteFile(filepath.Join(jd, "meta.json"), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	base := func(id string, rb bool) jobs.Job {
		// PID alive (our own) so List doesn't reconcile a "running" job to "stopped".
		return jobs.Job{ID: id, Task: "do work", Mode: "auto", ReportBack: rb, PID: os.Getpid(),
			Status: jobs.StatusRunning, StartedAt: time.Now().UTC()}
	}

	writeJob(base("job_rb", true))  // a report-back agent
	writeJob(base("job_ff", false)) // a fire-and-forget dispatch
	seen := seedSeenAgents(root)    // both seen as running

	// Both finish; the report-back one carries a result.
	rb := base("job_rb", true)
	rb.Status, rb.Result = jobs.StatusDone, "the finished output"
	writeJob(rb)
	ff := base("job_ff", false)
	ff.Status = jobs.StatusDone
	writeJob(ff)

	notes, backs := agentDoneNotifications(root, seen)
	if len(notes) != 2 {
		t.Errorf("both finished jobs should produce a user notification, got %d: %v", len(notes), notes)
	}
	if len(backs) != 1 {
		t.Fatalf("only the report-back agent should report back, got %d", len(backs))
	}
	if backs[0].ID != "job_rb" || backs[0].Result != "the finished output" {
		t.Errorf("report-back must carry the persisted result, got %+v", backs[0])
	}
}
