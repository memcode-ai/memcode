package server

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/memcode-ai/memcode/internal/channels"
	"github.com/memcode-ai/memcode/internal/gateway/state"
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

// speaker synthesizes speech (OGG/Opus bytes) for a reply. Implemented by the
// openai provider home; nil when no key is configured.
type speaker interface {
	Speak(ctx context.Context, text string) ([]byte, error)
}

// newSpeaker picks a text-to-speech backend from present credentials. OpenAI
// only for now — its speech endpoint emits Opus directly, so no transcoding
// (and no ffmpeg) exists anywhere in the pipeline.
func newSpeaker() speaker {
	if k := strings.TrimSpace(os.Getenv(openaiprov.EnvOpenAIKey)); k != "" {
		return openaiprov.NewOpenAI(k)
	}
	return nil
}

// maybeSpeak synthesizes a voice rendition of a reply when the channel's
// voice_replies policy asks for one, returning the spool path ("" = text
// only). Policy: "always", or "in_kind" when the task arrived with a voice
// note. Failures degrade silently to text — a reply is never lost to TTS.
func (r *runtime) maybeSpeak(ctx context.Context, it state.Item, reply string) string {
	if r.tts == nil {
		return ""
	}
	switch r.cfg().Get(it.Channel).VoiceReplies {
	case "always":
	case "in_kind":
		hadVoice := false
		for _, id := range it.Attachments {
			if audioSpoolID(id) {
				hadVoice = true
			}
		}
		if !hadVoice {
			return ""
		}
	default: // "" / "off" — deliberate opt-in
		return ""
	}
	spoken := spokenSummary(reply)
	if spoken == "" {
		return ""
	}
	data, err := r.tts.Speak(ctx, spoken)
	if err != nil {
		fmt.Fprintf(r.out, "gateway: voice reply synthesis failed: %v\n", err)
		return ""
	}
	att, err := channels.SaveToSpool(r.mediaDir, bytes.NewReader(data), "audio/ogg", "reply.ogg")
	if err != nil {
		return ""
	}
	return att.Path
}

// spokenSummary renders a reply as speakable text: code blocks dropped (nobody
// wants a diff read aloud), whitespace collapsed, capped at ~600 runes with the
// full text always arriving alongside as a message.
func spokenSummary(reply string) string {
	var kept []string
	inFence := false
	for _, line := range strings.Split(reply, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
			continue
		}
		if !inFence {
			kept = append(kept, line)
		}
	}
	s := strings.Join(strings.Fields(strings.Join(kept, " ")), " ")
	runes := []rune(s)
	if len(runes) > 600 {
		s = string(runes[:600]) + "… full details in the text reply."
	}
	return strings.TrimSpace(s)
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
