package gemini

import (
	"context"
	"fmt"
	"os"
	"strings"

	"google.golang.org/genai"
)

// transcribeAudioModel is the cheap multimodal tier — audio-in text-out is a
// plain generate call on Gemini, no dedicated speech endpoint needed.
const transcribeAudioModel = "gemini-2.5-flash"

// Transcribe converts an audio file to text by sending it inline to a
// multimodal generate call. Exported from the provider home so the genai SDK
// stays contained here; the gateway calls it for inbound voice notes when a
// Gemini key is the available credential.
func (g *Gemini) Transcribe(ctx context.Context, path, mime string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if mime == "" {
		mime = "audio/ogg"
	}
	client, err := g.client(ctx)
	if err != nil {
		return "", err
	}
	contents := []*genai.Content{{
		Role: "user",
		Parts: []*genai.Part{
			{Text: "Transcribe this audio verbatim. Output ONLY the transcript text, nothing else."},
			{InlineData: &genai.Blob{MIMEType: mime, Data: data}},
		},
	}}
	resp, err := client.Models.GenerateContent(ctx, transcribeAudioModel, contents, nil)
	if err != nil {
		return "", fmt.Errorf("gemini transcription: %w", err)
	}
	text := strings.TrimSpace(resp.Text())
	if text == "" {
		return "", fmt.Errorf("gemini transcription: empty text")
	}
	return text, nil
}
