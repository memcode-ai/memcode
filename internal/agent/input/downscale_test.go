package input

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"
)

// makePNG builds a PNG of the given size filled with incompressible xorshift noise — so byte
// size tracks pixel count (a smooth gradient can compress BETTER at full res, which would mask
// the downscale win). Deterministic (fixed seed), no math/rand.
func makePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	seed := uint32(2463534242)
	next := func() uint8 { seed ^= seed << 13; seed ^= seed >> 17; seed ^= seed << 5; return uint8(seed) }
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{next(), next(), next(), 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func longEdge(t *testing.T, data []byte) int {
	t.Helper()
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	b := img.Bounds()
	if b.Dx() > b.Dy() {
		return b.Dx()
	}
	return b.Dy()
}

// A large image is capped to the long-edge limit and comes back smaller on the wire.
func TestDownscaleCapsLargeImage(t *testing.T) {
	orig := makePNG(t, 5000, 200) // long edge 5000 > cap; small area keeps the test fast
	out, mime := Downscale(orig, "image/png")
	if mime != "image/png" {
		t.Errorf("mime should be preserved, got %q", mime)
	}
	if le := longEdge(t, out); le > maxImageEdge {
		t.Errorf("long edge %d not capped to %d", le, maxImageEdge)
	}
	if len(out) >= len(orig) {
		t.Errorf("downscaled image should be smaller on the wire: %d >= %d", len(out), len(orig))
	}
}

// An image already within the cap is returned byte-identical (no needless re-encode).
func TestDownscaleSmallImageUntouched(t *testing.T) {
	orig := makePNG(t, 800, 600)
	out, _ := Downscale(orig, "image/png")
	if !bytes.Equal(out, orig) {
		t.Error("an image within the cap must pass through unchanged")
	}
}

// Formats we don't resize (webp/gif) pass through untouched even when oversized.
func TestDownscaleSkipsOtherFormats(t *testing.T) {
	orig := makePNG(t, 3000, 200) // PNG bytes, but mime says webp → hits the passthrough branch
	out, mime := Downscale(orig, "image/webp")
	if !bytes.Equal(out, orig) || mime != "image/webp" {
		t.Error("non-PNG/JPEG mimes must pass through untouched")
	}
}
