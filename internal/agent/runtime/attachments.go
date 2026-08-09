package runtime

import (
	"context"
	"os"
	"path/filepath"

	"github.com/memcode-ai/memcode/internal/agent/input"
	"github.com/memcode-ai/memcode/internal/events"
	"github.com/memcode-ai/memcode/internal/wire"
)

// userBlocks converts an input bundle into provider blocks, honoring the secrets
// policy and the permission gate (ask before sending an attachment to the model).
func (s *Session) userBlocks(ctx context.Context, b input.Bundle) []wire.Block {
	var blocks []wire.Block
	if b.Text != "" {
		// Pasted text is canonical, including attachment paths. Native attachment
		// blocks are additive, so a failed read or rejected file never erases the
		// user's transcript from the model request.
		blocks = append(blocks, wire.TextBlock(b.Text))
	}
	// Bound the bundle (count + aggregate bytes) before sending — many small files still add up.
	atts, dropped := input.CapAttachments(b.Attachments)
	if dropped > 0 {
		s.printf("  ⚠ %d attachment(s) dropped — over the limit (%d files / %dMB request payload).\n",
			dropped, input.MaxAttachments, input.MaxRequestB64Bytes/(1024*1024))
	}
	for _, a := range atts {
		s.emit(ctx, events.KindAttachmentDetected, map[string]any{
			"path": a.Path, "kind": string(a.Kind), "mime": a.Mime, "sha256": a.SHA256, "size": a.SizeBytes})
		name := filepath.Base(a.Path)
		// The user explicitly attached it, so send it — no approval friction. The
		// one exception is a credential-bearing file, which is never sent raw.
		switch a.Kind {
		case input.KindSecret:
			s.printf("  ⚠ %s looks like a credential file — not sending it.\n", name)
			s.emit(ctx, events.KindAttachmentRejected, map[string]any{"path": a.Path, "reason": "secret"})
		case input.KindImage:
			data, err := os.ReadFile(a.Path)
			if err != nil {
				s.printf("  ⚠ could not read attached image %s — not sending.\n", name)
				s.emit(ctx, events.KindAttachmentRejected, map[string]any{"path": a.Path, "reason": "read_error"})
				continue
			}
			// Downscale before sending: Anthropic downscales anything over its long-edge cap
			// server-side anyway, so shipping full-res just burns upload bandwidth + latency on
			// EVERY turn the image rides in history. Cap to the high-res tier (no fidelity loss).
			mime := a.Mime
			data, mime = input.Downscale(data, mime)
			// Check the BASE64 size (how the image travels) against Anthropic's per-image ceiling —
			// AFTER downscaling, so a large source that shrinks under the limit is salvaged, not
			// rejected. Images always serve on Anthropic (the gateway escalates vision; glm has none).
			if input.Base64Len(int64(len(data))) > input.MaxImageB64Bytes {
				s.printf("  ⚠ %s is too large to send even downscaled (%dMB base64; limit %dMB).\n",
					name, input.Base64Len(int64(len(data)))/(1024*1024), input.MaxImageB64Bytes/(1024*1024))
				s.emit(ctx, events.KindAttachmentRejected, map[string]any{"path": a.Path, "reason": "too_large", "size": int64(len(data))})
				continue
			}
			blocks = append(blocks, wire.ImageBlock(mime, data))
			s.printf("● attached image %s (%dKB)\n", name, int64(len(data))/1024)
			s.emit(ctx, events.KindAttachmentSent, map[string]any{"path": a.Path, "kind": "image"})
		case input.KindPDF:
			data, err := os.ReadFile(a.Path)
			if err != nil {
				s.printf("  ⚠ could not read attached PDF %s — not sending.\n", name)
				s.emit(ctx, events.KindAttachmentRejected, map[string]any{"path": a.Path, "reason": "read_error"})
				continue
			}
			// PDFs ride the LLM call as native document blocks — the model reads the
			// file directly; models without document input absorb the turn gateway-side.
			if input.Base64Len(int64(len(data))) > input.MaxPDFB64Bytes {
				s.printf("  ⚠ %s is too large to attach (%dMB base64; limit %dMB).\n",
					name, input.Base64Len(int64(len(data)))/(1024*1024), input.MaxPDFB64Bytes/(1024*1024))
				s.emit(ctx, events.KindAttachmentRejected, map[string]any{"path": a.Path, "reason": "too_large", "size": int64(len(data))})
				continue
			}
			blocks = append(blocks, wire.DocumentBlock("application/pdf", data))
			s.printf("● attached PDF %s (%dKB)\n", name, int64(len(data))/1024)
			s.emit(ctx, events.KindAttachmentSent, map[string]any{"path": a.Path, "kind": "pdf"})
		case input.KindText:
			data, _ := os.ReadFile(a.Path)
			content := s.redactor.Redact(truncate(string(data), maxAttachInline))
			blocks = append(blocks, wire.TextBlock("Attached file "+a.Path+":\n"+content))
			s.printf("● attached file %s\n", name)
			s.emit(ctx, events.KindAttachmentSent, map[string]any{"path": a.Path, "kind": "text"})
		default:
			// Unsupported type (binary/exe/app/unknown): REJECT it — don't send the bytes, and
			// don't quietly inject the path either. If the user meant to reference the path, they
			// can paste it as text.
			s.printf("  ⚠ %s (%s) is an unsupported file type — not sending. Paste its path as text to reference it.\n", name, a.Mime)
			s.emit(ctx, events.KindAttachmentRejected, map[string]any{"path": a.Path, "reason": "unsupported_type", "mime": a.Mime})
		}
	}
	if len(blocks) == 0 {
		blocks = append(blocks, wire.TextBlock(b.Text))
	}
	return blocks
}
