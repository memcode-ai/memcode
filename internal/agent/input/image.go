package input

import (
	"bytes"
	"image"
	"image/jpeg"
	"image/png"

	xdraw "golang.org/x/image/draw"
)

// maxImageEdge is the long-edge pixel cap we downscale to before sending. It's Anthropic's
// HIGH-RESOLUTION tier limit (Opus 5, Fable/Mythos 5 — 2576px); standard-tier models
// (Sonnet et al.) downscale further to 1568px server-side. We can't know at attach time which
// model a turn will route to, so we cap at the larger limit: that NEVER discards fidelity a
// model could use, and it still strips the genuinely oversized originals (4K screenshots, phone
// photos at 3000–4000px) that the API would only throw away. The win is pure: anything over the
// cap is downscaled by Anthropic regardless, so shipping it full-res just wastes upload
// bandwidth + latency — on EVERY turn the image rides in the conversation history.
const maxImageEdge = 2576

// Downscale shrinks an image's long edge to maxImageEdge (re-encoding in the same format) when
// it's larger, returning the smaller bytes. It's a best-effort optimization: any decode/encode
// failure, an already-small image, or a result that isn't actually smaller returns the original
// bytes + mime unchanged. Only PNG and JPEG are resized — the common screenshot/photo formats;
// GIF/WebP pass through untouched (no extra decoder dependency, and GIF animation is preserved).
func Downscale(data []byte, mime string) ([]byte, string) {
	switch mime {
	case "image/png", "image/jpeg": // resize only the formats stdlib decodes losslessly here
	default:
		return data, mime
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return data, mime
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	long := w
	if h > long {
		long = h
	}
	if long <= maxImageEdge {
		return data, mime // already within the cap — sending it as-is loses nothing
	}
	scale := float64(maxImageEdge) / float64(long)
	nw, nh := int(float64(w)*scale+0.5), int(float64(h)*scale+0.5)
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), img, b, xdraw.Over, nil) // high-quality resample (keeps text legible)

	var buf bytes.Buffer
	switch mime {
	case "image/png":
		if png.Encode(&buf, dst) != nil {
			return data, mime
		}
	case "image/jpeg":
		if jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 90}) != nil {
			return data, mime
		}
	}
	if buf.Len() == 0 || buf.Len() >= len(data) {
		return data, mime // didn't actually shrink (rare) — keep the original
	}
	return buf.Bytes(), mime
}
