package server

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gwconfig "github.com/memcode-ai/memcode/internal/gateway/config"
	"github.com/memcode-ai/memcode/internal/gateway/state"
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

type fakeTTS struct{ called int }

func (f *fakeTTS) Speak(_ context.Context, _ string) ([]byte, error) {
	f.called++
	return []byte("OggS-fake"), nil
}

// voice_replies is a deliberate opt-in: default off, in_kind only when the
// task carried a voice note, always speaks everything.
func TestMaybeSpeakPolicy(t *testing.T) {
	dir := t.TempDir()
	tts := &fakeTTS{}
	rt := &runtime{mediaDir: dir, tts: tts, out: io.Discard}
	itVoice := state.Item{Channel: "telegram", Attachments: []string{"aa.ogg"}}
	itText := state.Item{Channel: "telegram"}

	// Default: off, even for a voice note.
	if p := rt.maybeSpeak(context.Background(), itVoice, "done"); p != "" || tts.called != 0 {
		t.Errorf("default must be off: %q %d", p, tts.called)
	}
	rt.settings = gwconfig.Settings{Channels: map[string]gwconfig.Channel{"telegram": {VoiceReplies: "in_kind"}}}
	if p := rt.maybeSpeak(context.Background(), itText, "done"); p != "" {
		t.Errorf("in_kind must not speak for text-only tasks: %q", p)
	}
	p := rt.maybeSpeak(context.Background(), itVoice, "done")
	if p == "" || tts.called != 1 {
		t.Fatalf("in_kind with voice note: %q %d", p, tts.called)
	}
	if _, err := os.Stat(p); err != nil {
		t.Errorf("voice file missing: %v", err)
	}
	rt.settings = gwconfig.Settings{Channels: map[string]gwconfig.Channel{"telegram": {VoiceReplies: "always"}}}
	if p := rt.maybeSpeak(context.Background(), itText, "done"); p == "" {
		t.Error("always must speak")
	}
}

func TestSpokenSummary(t *testing.T) {
	in := "Fixed it.\n```go\nfunc x() {}\n```\nAll tests green."
	got := spokenSummary(in)
	if strings.Contains(got, "func x") || !strings.Contains(got, "All tests green") {
		t.Errorf("summary = %q", got)
	}
	long := strings.Repeat("word ", 300)
	if s := spokenSummary(long); len([]rune(s)) > 700 || !strings.Contains(s, "full details in the text reply") {
		t.Errorf("cap failed: %d runes", len([]rune(s)))
	}
}
