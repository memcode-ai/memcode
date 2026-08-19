package jobs

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

func mustWriteMeta(t *testing.T, root string, job Job) {
	t.Helper()
	if err := os.MkdirAll(jobDir(root, job.ID), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeMeta(root, job); err != nil {
		t.Fatal(err)
	}
}

func TestHeartbeatRoundTrip(t *testing.T) {
	root := t.TempDir()
	job := Job{ID: "job_hb1", Task: "t", Mode: "auto", PID: os.Getpid(),
		Status: StatusRunning, StartedAt: time.Now().UTC()}
	mustWriteMeta(t, root, job)
	if err := Heartbeat(root, job.ID, "bash(go test ./...)", 1200, 4200); err != nil {
		t.Fatal(err)
	}
	got, err := load(root, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Activity != "bash(go test ./...)" || got.TokensIn != 1200 || got.TokensOut != 4200 {
		t.Errorf("heartbeat fields not persisted: %+v", got)
	}
	if got.HeartbeatAt.IsZero() {
		t.Error("HeartbeatAt not set")
	}
	if got.Status != StatusRunning || got.Task != "t" {
		t.Errorf("heartbeat disturbed other fields: %+v", got)
	}
}

// A heartbeat racing Finish/Stop must never resurrect a terminal record: the
// terminal status, exit code, and result all survive untouched.
func TestHeartbeatNeverTouchesTerminalJob(t *testing.T) {
	root := t.TempDir()
	for _, status := range []string{StatusDone, StatusFailed, StatusStopped} {
		job := Job{ID: "job_" + status, Task: "t", Mode: "auto", PID: 1,
			Status: status, ExitCode: 3, Result: "final answer",
			StartedAt: time.Now().UTC(), FinishedAt: time.Now().UTC()}
		mustWriteMeta(t, root, job)
		before, _ := os.ReadFile(metaPath(root, job.ID))
		if err := Heartbeat(root, job.ID, "late tick", 9, 9); err != nil {
			t.Fatalf("%s: %v", status, err)
		}
		after, _ := os.ReadFile(metaPath(root, job.ID))
		if string(before) != string(after) {
			t.Errorf("%s: heartbeat modified a terminal meta:\nbefore: %s\nafter:  %s", status, before, after)
		}
	}
}

// The other ordering: Finish landing after a heartbeat keeps the terminal
// outcome (heartbeat fields may remain — they're informational).
func TestFinishAfterHeartbeatWins(t *testing.T) {
	root := t.TempDir()
	job := Job{ID: "job_hb2", Task: "t", Mode: "auto", PID: os.Getpid(),
		Status: StatusRunning, StartedAt: time.Now().UTC()}
	mustWriteMeta(t, root, job)
	if err := Heartbeat(root, job.ID, "working", 1, 2); err != nil {
		t.Fatal(err)
	}
	if err := Finish(root, job.ID, 0, "done text"); err != nil {
		t.Fatal(err)
	}
	got, err := load(root, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusDone || got.Result != "done text" {
		t.Errorf("finish did not win: %+v", got)
	}
}

// Old metas without the heartbeat fields still parse, with zero values.
func TestLegacyMetaParses(t *testing.T) {
	legacy := []byte(`{"id":"job_old","task":"t","mode":"auto","pid":1,"status":"done","exit_code":0,"started_at":"2026-01-01T00:00:00Z"}`)
	var job Job
	if err := json.Unmarshal(legacy, &job); err != nil {
		t.Fatal(err)
	}
	if job.Activity != "" || job.TokensOut != 0 || !job.HeartbeatAt.IsZero() {
		t.Errorf("legacy meta produced non-zero heartbeat fields: %+v", job)
	}
}
