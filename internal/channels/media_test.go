package channels

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSaveToSpoolAndResolve(t *testing.T) {
	dir := t.TempDir()
	att, err := SaveToSpool(dir, strings.NewReader("hello"), "image/png", "shot.png")
	if err != nil {
		t.Fatal(err)
	}
	if att.Kind != KindImage || !strings.HasSuffix(att.Path, ".png") {
		t.Errorf("attachment = %+v", att)
	}
	// Content-addressed: the same bytes land in the same file.
	again, err := SaveToSpool(dir, strings.NewReader("hello"), "image/png", "other.png")
	if err != nil || again.Path != att.Path {
		t.Errorf("dedup: %v, %q vs %q", err, again.Path, att.Path)
	}
	// The ID round-trips through the resolver.
	p, err := ResolveSpoolID(dir, att.ID())
	if err != nil || p != att.Path {
		t.Errorf("resolve = %q, %v", p, err)
	}
	b, _ := os.ReadFile(p)
	if string(b) != "hello" {
		t.Errorf("content = %q", b)
	}
}

// The spool is the trust boundary: IDs that aren't bare spool filenames never
// resolve, so a corrupted context file can't point a job at arbitrary files.
func TestResolveSpoolIDRejectsEscapes(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"", "../secret.txt", outside, "a/b.png", ".hidden", `..\evil`} {
		if _, err := ResolveSpoolID(dir, id); err == nil {
			t.Errorf("id %q must not resolve", id)
		}
	}
	// A valid-shaped but absent ID errors rather than inventing a path.
	if _, err := ResolveSpoolID(dir, "deadbeef.png"); err == nil {
		t.Error("missing id must not resolve")
	}
}

func TestKindForMime(t *testing.T) {
	cases := map[[2]string]string{
		{"image/jpeg", "x"}:              KindImage,
		{"audio/ogg; codecs=opus", "v"}:  KindAudio,
		{"application/pdf", "doc"}:       KindPDF,
		{"", "voice.opus"}:               KindAudio,
		{"", "report.pdf"}:               KindPDF,
		{"application/octet-stream", ""}: KindFile,
	}
	for in, want := range cases {
		if got := KindForMime(in[0], in[1]); got != want {
			t.Errorf("KindForMime(%q,%q) = %q, want %q", in[0], in[1], got, want)
		}
	}
}

func TestSaveToSpoolCap(t *testing.T) {
	dir := t.TempDir()
	huge := strings.NewReader(strings.Repeat("x", MaxAttachmentBytes+1))
	if _, err := SaveToSpool(dir, huge, "application/octet-stream", "big"); err == nil {
		t.Fatal("over-cap attachment must be refused")
	}
	// No partial file left behind.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "spool-") {
			t.Errorf("temp file leaked: %s", e.Name())
		}
	}
}
