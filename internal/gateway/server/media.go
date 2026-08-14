package server

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/memcode-ai/memcode/internal/channels"
	"github.com/memcode-ai/memcode/internal/providers/gemini"
	openaiprov "github.com/memcode-ai/memcode/internal/providers/openai"
)

// transcriber converts an audio file to text. Implemented by the provider
// homes (openai, gemini) so their SDKs stay contained there.
type transcriber interface {
	Transcribe(ctx context.Context, path, mime string) (string, error)
}

// newTranscriber picks a speech-to-text backend from the credentials present
// in the environment: OpenAI first (a dedicated, accurate STT endpoint), then
// Gemini (audio-in generate). nil when neither key is set — voice notes then
// get an honest "not configured" reply instead of silence.
func newTranscriber() transcriber {
	if k := strings.TrimSpace(os.Getenv(openaiprov.EnvOpenAIKey)); k != "" {
		return openaiprov.NewOpenAI(k)
	}
	if k := strings.TrimSpace(os.Getenv(gemini.EnvGeminiKey)); k != "" {
		return gemini.NewGemini(k)
	}
	return nil
}

// audioSpoolID reports whether a spool ID names an audio file (the spool is
// content-addressed with a MIME-derived extension, so the extension is ours).
func audioSpoolID(id string) bool {
	switch strings.ToLower(filepath.Ext(id)) {
	case ".ogg", ".oga", ".opus", ".mp3", ".m4a", ".wav", ".webm", ".amr", ".aac", ".flac":
		return true
	}
	return false
}

// transcribeAudio resolves and transcribes the audio attachments of a task,
// returning the composed task text and the remaining (non-audio) spool IDs.
// The transcript is labeled so the model knows it is machine-transcribed
// speech, not typed text. missing reports audio that could not be handled
// because no transcription provider is configured.
func (r *runtime) transcribeAudio(ctx context.Context, text string, ids []string) (task string, rest []string, missing bool) {
	var transcripts []string
	for _, id := range ids {
		if !audioSpoolID(id) {
			rest = append(rest, id)
			continue
		}
		path, err := channels.ResolveSpoolID(r.mediaDir, id)
		if err != nil {
			continue
		}
		if r.stt == nil {
			missing = true
			continue
		}
		t, err := r.stt.Transcribe(ctx, path, "")
		if err != nil {
			fmt.Fprintf(r.out, "gateway: transcribing %s failed: %v\n", id, err)
			missing = true
			continue
		}
		transcripts = append(transcripts, t)
	}
	task = text
	if len(transcripts) > 0 {
		joined := strings.Join(transcripts, "\n")
		if strings.TrimSpace(task) == "" {
			task = joined
		} else {
			task = task + "\n\n[transcribed voice note]\n" + joined
		}
	}
	return task, rest, missing
}

// pruneSpool deletes media spool files older than the cutoff — the same
// retention as the durable inbox, so an attachment outlives every task that
// could still reference it. Best-effort: a prune failure never blocks startup.
func pruneSpool(dir string, before time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.ModTime().Before(before) {
			continue
		}
		_ = os.Remove(filepath.Join(dir, e.Name()))
	}
}
