package conformance

// probes_test.go runs in NORMAL CI (no endpoint needed): it validates the probe
// fixtures themselves, so the capability probes can never report a false "no"
// because OUR pdf/png was malformed rather than the endpoint lacking the
// capability.

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image/png"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func TestMinimalPDFIsStructurallyValid(t *testing.T) {
	pdf := minimalPDF("Codeword: EMBER-NINE")
	s := string(pdf)
	if !strings.HasPrefix(s, "%PDF-1.4\n") || !strings.HasSuffix(s, "%%EOF\n") {
		t.Fatalf("bad envelope: %.40q … %.40q", s, s[len(s)-40:])
	}
	if !strings.Contains(s, "(Codeword: EMBER-NINE) Tj") {
		t.Fatal("text payload missing from the content stream")
	}
	// startxref must point at the xref table
	m := regexp.MustCompile(`startxref\n(\d+)\n`).FindStringSubmatch(s)
	if m == nil {
		t.Fatal("no startxref")
	}
	xrefAt, _ := strconv.Atoi(m[1])
	if !strings.HasPrefix(s[xrefAt:], "xref\n") {
		t.Fatalf("startxref %d does not point at the xref table (%.20q)", xrefAt, s[xrefAt:])
	}
	// every xref entry must point at its "N 0 obj" header
	entries := regexp.MustCompile(`(?m)^(\d{10}) 00000 n `).FindAllStringSubmatch(s, -1)
	if len(entries) != 5 {
		t.Fatalf("xref entries = %d, want 5", len(entries))
	}
	for i, e := range entries {
		off, _ := strconv.Atoi(e[1])
		want := fmt.Sprintf("%d 0 obj", i+1)
		if !strings.HasPrefix(s[off:], want) {
			t.Errorf("xref entry %d (offset %d) points at %.16q, want %q", i+1, off, s[off:], want)
		}
	}
	// the declared stream length must match the stream bytes
	lm := regexp.MustCompile(`/Length (\d+) >>\nstream\n`).FindStringSubmatchIndex(s)
	if lm == nil {
		t.Fatal("no content stream")
	}
	declared, _ := strconv.Atoi(s[lm[2]:lm[3]])
	body := s[lm[1]:]
	end := strings.Index(body, "endstream")
	if end != declared {
		t.Errorf("stream length declared %d, actual %d", declared, end)
	}
}

func TestRedPNGDataURL(t *testing.T) {
	url := redPNGDataURL()
	payload, ok := strings.CutPrefix(url, "data:image/png;base64,")
	if !ok {
		t.Fatalf("bad data URL prefix: %.40q", url)
	}
	raw, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		t.Fatalf("payload not base64: %v", err)
	}
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("not a decodable PNG: %v", err)
	}
	r, g, b, _ := img.At(12, 12).RGBA()
	if r>>8 != 255 || g>>8 != 0 || b>>8 != 0 {
		t.Fatalf("center pixel = (%d,%d,%d), want solid red", r>>8, g>>8, b>>8)
	}
}
