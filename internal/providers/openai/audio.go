package openai

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	oai "github.com/openai/openai-go/v3"
)

// Audio models: cheap-and-fast defaults with the open-source Whisper as the
// fallback for accounts that haven't enabled the 4o audio models.
const (
	transcribeModel         = "gpt-4o-mini-transcribe"
	transcribeFallbackModel = "whisper-1"
	speechModel             = "gpt-4o-mini-tts"
	speechVoice             = "alloy"
)

// Transcribe converts an audio file (ogg/mp3/m4a/wav/…) to text via the audio
// transcriptions endpoint. Exported from the provider home so the OpenAI SDK
// stays contained here (TestVendorSDKsOnlyInTheirAdapters); the gateway calls
// it for inbound voice notes. Tries the 4o mini transcribe model first and
// falls back to whisper-1 once on any API error.
func (o *OpenAI) Transcribe(ctx context.Context, path, mime string) (string, error) {
	text, err := o.transcribeWith(ctx, path, transcribeModel)
	if err == nil {
		return text, nil
	}
	if text, ferr := o.transcribeWith(ctx, path, transcribeFallbackModel); ferr == nil {
		return text, nil
	}
	return "", err
}

func (o *OpenAI) transcribeWith(ctx context.Context, path, model string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	client := o.client()
	resp, err := client.Audio.Transcriptions.New(ctx, oai.AudioTranscriptionNewParams{
		File:  f,
		Model: oai.AudioModel(model),
	})
	if err != nil {
		return "", fmt.Errorf("openai transcription (%s): %w", model, err)
	}
	text := strings.TrimSpace(resp.Text)
	if text == "" {
		return "", fmt.Errorf("openai transcription (%s): empty text", model)
	}
	return text, nil
}

// Speak synthesizes speech for text and returns OGG/Opus bytes — the container
// chat platforms accept as a voice note directly, so no transcoding step (and
// no ffmpeg) is needed anywhere downstream.
func (o *OpenAI) Speak(ctx context.Context, text string) ([]byte, error) {
	client := o.client()
	resp, err := client.Audio.Speech.New(ctx, oai.AudioSpeechNewParams{
		Input:          text,
		Model:          speechModel,
		Voice:          oai.AudioSpeechNewParamsVoiceUnion{OfString: oai.String(speechVoice)},
		ResponseFormat: oai.AudioSpeechNewParamsResponseFormatOpus,
	})
	if err != nil {
		return nil, fmt.Errorf("openai speech: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 25<<20))
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("openai speech: empty audio")
	}
	return data, nil
}
