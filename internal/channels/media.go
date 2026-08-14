package channels

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"
	"strings"
)

// MaxAttachmentBytes caps a single downloaded attachment. Keeps a hostile or
// oversized upload from filling the disk; also comfortably above every
// transcription provider's audio cap.
const MaxAttachmentBytes = 25 << 20 // 25 MiB

func filepathBase(p string) string { return filepath.Base(p) }

// KindForMime maps a MIME type (and filename, as a fallback) to a coarse
// attachment kind.
func KindForMime(mimeType, name string) string {
	mt := strings.ToLower(strings.TrimSpace(mimeType))
	if i := strings.IndexByte(mt, ';'); i >= 0 {
		mt = strings.TrimSpace(mt[:i])
	}
	switch {
	case strings.HasPrefix(mt, "image/"):
		return KindImage
	case strings.HasPrefix(mt, "audio/"):
		return KindAudio
	case mt == "application/pdf":
		return KindPDF
	}
	switch strings.ToLower(filepath.Ext(name)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp":
		return KindImage
	case ".ogg", ".oga", ".opus", ".mp3", ".m4a", ".wav", ".aac", ".flac", ".amr":
		return KindAudio
	case ".pdf":
		return KindPDF
	}
	return KindFile
}

// SaveToSpool streams r into the media spool as <sha256>.<ext> (content-addressed:
// the same bytes land once) and returns the Attachment. The write is capped at
// MaxAttachmentBytes; an over-cap stream is an error, never a truncated file.
func SaveToSpool(dir string, r io.Reader, mimeType, name string) (Attachment, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Attachment{}, err
	}
	tmp, err := os.CreateTemp(dir, "spool-*")
	if err != nil {
		return Attachment{}, err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(r, MaxAttachmentBytes+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return Attachment{}, err
	}
	if n > MaxAttachmentBytes {
		return Attachment{}, fmt.Errorf("attachment %q exceeds %d bytes", name, int64(MaxAttachmentBytes))
	}
	id := hex.EncodeToString(h.Sum(nil)) + spoolExt(mimeType, name)
	final := filepath.Join(dir, id)
	if _, statErr := os.Stat(final); statErr == nil {
		return Attachment{Path: final, Kind: KindForMime(mimeType, name), Mime: mimeType, Name: name}, nil
	}
	if err := os.Rename(tmp.Name(), final); err != nil {
		return Attachment{}, err
	}
	if err := os.Chmod(final, 0o600); err != nil {
		return Attachment{}, err
	}
	return Attachment{Path: final, Kind: KindForMime(mimeType, name), Mime: mimeType, Name: name}, nil
}

// ResolveSpoolID resolves a spool ID back to a path STRICTLY inside dir — the
// spool is the trust boundary, so an ID carrying separators, "..", or anything
// but a bare filename is rejected rather than resolved.
func ResolveSpoolID(dir, id string) (string, error) {
	if id == "" || id != filepath.Base(id) || strings.HasPrefix(id, ".") || strings.ContainsAny(id, `/\`) {
		return "", fmt.Errorf("invalid media id %q", id)
	}
	p := filepath.Join(dir, id)
	fi, err := os.Stat(p)
	if err != nil {
		return "", err
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("media id %q is not a regular file", id)
	}
	return p, nil
}

// spoolExt picks a filename extension: the platform-reported MIME type first,
// the original name's extension second, ".bin" last.
func spoolExt(mimeType, name string) string {
	mt := strings.ToLower(strings.TrimSpace(mimeType))
	if i := strings.IndexByte(mt, ';'); i >= 0 {
		mt = strings.TrimSpace(mt[:i])
	}
	// Common types get stable, unsurprising extensions (mime.ExtensionsByType
	// ordering is platform-dependent).
	known := map[string]string{
		"image/jpeg": ".jpg", "image/png": ".png", "image/gif": ".gif", "image/webp": ".webp",
		"audio/ogg": ".ogg", "audio/opus": ".ogg", "audio/mpeg": ".mp3", "audio/mp4": ".m4a",
		"audio/x-m4a": ".m4a", "audio/wav": ".wav", "audio/webm": ".webm", "audio/amr": ".amr",
		"application/pdf": ".pdf", "text/plain": ".txt",
	}
	if ext, ok := known[mt]; ok {
		return ext
	}
	if exts, _ := mime.ExtensionsByType(mt); len(exts) > 0 {
		return exts[0]
	}
	if ext := strings.ToLower(filepath.Ext(name)); ext != "" && len(ext) <= 8 {
		return ext
	}
	return ".bin"
}
