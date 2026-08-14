package server

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeSTT struct{ text string }

func (f fakeSTT) Transcribe(_ context.Context, path, _ string) (string, error) {
	return f.text, nil
}

func TestTranscribeAudioComposesTask(t *testing.T) {
	dir := t.TempDir()
	// One audio file, one image in the spool.
	for _, name := range []string{"aa.ogg", "bb.png"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	rt := &runtime{mediaDir: dir, stt: fakeSTT{text: "fix the login bug"}, out: io.Discard}

	// Voice-only message: the transcript IS the task; the image stays attached.
	task, rest, missing := rt.transcribeAudio(context.Background(), "", []string{"aa.ogg", "bb.png"})
	if task != "fix the login bug" || missing {
		t.Errorf("task = %q missing=%v", task, missing)
	}
	if len(rest) != 1 || rest[0] != "bb.png" {
		t.Errorf("rest = %v", rest)
	}

	// Text + voice: transcript is appended, labeled.
	task, _, _ = rt.transcribeAudio(context.Background(), "context here", []string{"aa.ogg"})
	if task == "context here" || !strings.Contains(task, "[transcribed voice note]") {
		t.Errorf("composed task = %q", task)
	}

	// No STT configured → missing reported, audio dropped, non-audio kept.
	rt.stt = nil
	task, rest, missing = rt.transcribeAudio(context.Background(), "", []string{"aa.ogg", "bb.png"})
	if !missing || task != "" || len(rest) != 1 {
		t.Errorf("no-stt: task=%q rest=%v missing=%v", task, rest, missing)
	}
}
