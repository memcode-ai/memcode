// Package input routes a raw interactive line — coalesce / steer / queue /
// interrupt — so the agent collaborates the way people actually type instead of
// treating every Enter as a new task. Composer text is TEXT: Parse never scans
// prose for arbitrary file paths (that "helpfully" turned words into attachments
// because they matched repo paths). The ONE Parse-level extraction is a narrow
// pasted/dragged ABSOLUTE path to an EXISTING supported image file (the macOS
// "paste a screenshot" case); every OTHER attachment comes only from an explicit
// drag/drop or attach event via Resolve. Routing is deterministic and
// unit-testable; the timing side (coalescing window) lives in the interactive runner.
package input

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/memcode-ai/memcode/internal/agent/secrets"
)

// Route is how an input is folded into the session.
type Route string

const (
	Coalesce  Route = "coalesce"  // merge into the current turn (timing-decided by the runner)
	Steer     Route = "steer"     // refine the active objective (default)
	Queue     Route = "queue"     // a separate/future task
	Interrupt Route = "interrupt" // stop and re-plan
	Shell     Route = "shell"     // `$` direct-shell lane: run verbatim, no agent/model
)

// Kind classifies an attachment.
type Kind string

const (
	KindImage     Kind = "image"
	KindPDF       Kind = "pdf" // sent as a native document block — the model reads it on the LLM call
	KindText      Kind = "text"
	KindBinary    Kind = "binary"
	KindDirectory Kind = "directory"
	KindSecret    Kind = "secret" // detected as credential-bearing — never sent raw
)

// Attachment is one file referenced in a turn. Raw bytes are NOT stored here;
// only metadata (and the path, for the runner to read under policy).
type Attachment struct {
	Path      string `json:"path"`
	Kind      Kind   `json:"kind"`
	Mime      string `json:"mime,omitempty"`
	SHA256    string `json:"sha256,omitempty"`
	SizeBytes int64  `json:"size_bytes,omitempty"`
	Source    string `json:"source"` // drag_drop | paste
}

// Bundle is one user turn: canonical pasted text plus additive attachments.
// AttachmentOnly is true only when the routed source consists solely of recognized
// attachment paths, so callers can distinguish missing chat text from mixed input.
type Bundle struct {
	Text           string       `json:"text"`
	Attachments    []Attachment `json:"attachments"`
	AttachmentOnly bool         `json:"attachment_only,omitempty"`
}

// Decision is the full routing result for an input line (inspectable).
type Decision struct {
	Bundle Bundle
	Route  Route
	Reason string
}

var (
	// no(?:pe)? matches "no"/"nope" (the old nope? matched "nop"/"nope" but NOT plain "no").
	interruptWord = regexp.MustCompile(`^(stop|wait|wrong|don'?t|do not|undo|no(?:pe)?|nah|cancel|abort)\b`)
	// \b after each keyword so "next" doesn't swallow "nextjs"/"nextauth", "then" not "thenable".
	queueMarker = regexp.MustCompile(`^(after this\b|next\b[,:]?|then\b|later\b[,:]?|once that\b)`)
)

// Parse routes a raw input line. It does NOT scan prose for file paths (that turned
// words into attachments). The ONE narrow exception is a pasted ABSOLUTE path to an
// EXISTING image file — the macOS "paste a screenshot" case — which becomes an image
// attachment so the model can actually see it. Everything else is text.
func Parse(line, cwd string) Decision {
	raw := strings.TrimSpace(line)

	// Explicit override prefixes win.
	switch {
	case strings.HasPrefix(raw, "!"):
		return mkDecision(raw[1:], cwd, Interrupt, "explicit `!` interrupt")
	case strings.HasPrefix(raw, ">"):
		return mkDecision(raw[1:], cwd, Queue, "explicit `>` queue")
	case strings.HasPrefix(raw, "+"):
		return mkDecision(raw[1:], cwd, Steer, "explicit `+` steer")
	case strings.HasPrefix(raw, "$"):
		// `$` is a verbatim shell command — never image-extract.
		return Decision{Bundle{Text: strings.TrimSpace(raw[1:])}, Shell, "explicit `$` shell"}
	}

	lower := strings.ToLower(raw)
	switch {
	case interruptWord.MatchString(lower):
		return mkDecision(raw, cwd, Interrupt, "interrupt/negation language")
	case queueMarker.MatchString(lower):
		return mkDecision(raw, cwd, Queue, "future-task marker")
	default:
		// Default is steer; the runner upgrades to coalesce on rapid succession,
		// and may re-classify as queue using objective/journey state.
		return mkDecision(raw, cwd, Steer, "default: refine active objective")
	}
}

// mkDecision builds a Decision with canonical pasted text and additive attachments.
func mkDecision(text, cwd string, route Route, reason string) Decision {
	text = strings.TrimSpace(text)
	atts, attachmentOnly := discoverImagePaths(text, cwd)
	return Decision{Bundle: Bundle{Text: text, Attachments: atts, AttachmentOnly: attachmentOnly}, Route: route, Reason: reason}
}

// imagePathRe matches an absolute path to an image file, allowing shell-escaped
// spaces (`\ `) — exactly how macOS pastes a screenshot's path.
// Only the extensions the model actually accepts inline (supportedImageMimes). heic/bmp/tiff
// were matched before, then sniffed + rejected downstream — confusing; don't consider them.
var imagePathRe = regexp.MustCompile(`(?i)/(?:\\ |[^\s])+\.(?:png|jpe?g|gif|webp|pdf)`)

// discoverImagePaths finds absolute paths to existing supported image/PDF files
// without changing the submitted text. Pasted text is canonical; attachments are an
// additive representation for native model blocks. attachmentOnly is true only when
// the entire routed source is composed of recognized attachment path substrings and
// whitespace.
func discoverImagePaths(text, cwd string) (atts []Attachment, attachmentOnly bool) {
	matches := imagePathRe.FindAllStringIndex(text, -1)
	if len(matches) == 0 {
		return nil, false
	}

	var remainder strings.Builder
	last := 0
	for _, match := range matches {
		remainder.WriteString(text[last:match[0]])
		matched := text[match[0]:match[1]]
		path := strings.ReplaceAll(matched, `\ `, " ") // unescape shell-escaped spaces
		if att, ok := Resolve(path, cwd, "paste"); ok && (att.Kind == KindImage || att.Kind == KindPDF) {
			atts = append(atts, att)
		} else {
			remainder.WriteString(matched)
		}
		last = match[1]
	}
	remainder.WriteString(text[last:])
	return atts, len(atts) > 0 && strings.TrimSpace(remainder.String()) == ""
}

// ImageMatches returns the substrings of text that are absolute paths to existing
// supported image/PDF files — the same eligibility criteria used by discoverImagePaths.
// The TUI uses this to collapse a dragged-in path to a compact "[Image #N]" chip while
// keeping the exact substring recoverable on submit.
func ImageMatches(text, cwd string) []string {
	var out []string
	for _, m := range imagePathRe.FindAllString(text, -1) {
		path := strings.ReplaceAll(m, `\ `, " ")
		if att, ok := Resolve(path, cwd, "paste"); ok && (att.Kind == KindImage || att.Kind == KindPDF) {
			out = append(out, m)
		}
	}
	return out
}

// looksLikePath reports whether a candidate is shaped like a real path rather
// than an incidental prose word — it must be absolute, contain a separator, or
// carry a file extension. A bare word (or a lone "/"/"."/"..") never qualifies,
// so "datasets" or "scripts" can't be mistaken for an attachment.
func looksLikePath(s string) bool {
	s = strings.TrimSpace(s)
	switch s {
	case "", "/", ".", "..", "~":
		return false
	}
	if filepath.IsAbs(s) || strings.ContainsAny(s, `/\`) {
		return true
	}
	ext := filepath.Ext(s)
	return ext != "" && ext != "."
}

// Attachment ceilings — the DIRECT Anthropic API limits, verified against the vision + PDF docs
// at platform.claude.com. These are the binding limits because memcode's gateway talks to the
// Anthropic API directly AND images always serve on Anthropic (the cheap lane has no
// vision). On Bedrock/Vertex the per-image cap would be 5MB instead of 10MB — not a path memcode
// uses, so we don't carry a provider-profile abstraction for it (it would be dead code).
//
// Sizes are checked BASE64-ENCODED, because that's how images travel in the request and base64
// inflates raw bytes by ~33% — a raw 8MB image is ~10.7MB on the wire. Checking raw bytes would
// accept bundles the API then rejects.
const (
	MaxImageB64Bytes   = 10 << 20 // per-image ceiling, base64-encoded (Anthropic direct; 5MB on Bedrock/Vertex)
	MaxPDFB64Bytes     = 24 << 20 // per-PDF ceiling, base64-encoded — fits the 32MB request envelope with headroom
	MaxRequestB64Bytes = 32 << 20 // whole-request payload ceiling
	MaxAttachments     = 20       // claude.ai's per-message limit; ≤20 also avoids the stricter >20-image 2000px dimension rule
	attachB64Headroom  = 4 << 20  // reserve under the request ceiling for the prompt + JSON envelope + history
)

// Base64Len returns the base64-encoded byte length of n raw bytes (4 chars per 3 bytes, padded).
func Base64Len(n int64) int64 { return ((n + 2) / 3) * 4 }

// CapAttachments enforces the count + aggregate-payload ceilings, measuring each attachment at
// its BASE64-encoded size (how it travels to the model) so a bundle accepted locally can't blow
// the request envelope after encoding. Returns the kept attachments (order preserved) and how
// many were dropped, so the caller can tell the user.
func CapAttachments(atts []Attachment) (kept []Attachment, dropped int) {
	budget := int64(MaxRequestB64Bytes - attachB64Headroom)
	var used int64
	for _, a := range atts {
		enc := Base64Len(a.SizeBytes)
		if len(kept) >= MaxAttachments || used+enc > budget {
			dropped++
			continue
		}
		used += enc
		kept = append(kept, a)
	}
	return kept, dropped
}

// Resolve turns an explicit path (from a drag/drop payload or an attach command,
// NOT from scanning prose) into an Attachment if it exists. source is recorded
// for provenance (e.g. "drag_drop", "attach").
//
// PATH POLICY (current behavior, deliberately documented — not yet a hardened gate):
// a pasted/dragged ABSOLUTE path is honored ANYWHERE on disk (e.g. ~/Desktop, /tmp,
// /Volumes), because the user explicitly produced the event — but it's narrowed to
// supported images (imagePathRe) and size-capped. Symlinks are NOT resolved/confined,
// and an explicit attach of an arbitrary absolute path is not yet confirmation-gated.
// TODO(attachments): a real policy — workspace-local by default for `attach`, explicit
// allow for out-of-cwd absolute paths, and no silent symlink traversal outside cwd.
// TestResolveOutsideCwdAllowed pins TODAY's behavior so any tightening is intentional.
func Resolve(candidate, cwd, source string) (Attachment, bool) {
	candidate = strings.TrimSpace(candidate)
	if !looksLikePath(candidate) {
		return Attachment{}, false
	}
	path := candidate
	if !filepath.IsAbs(path) {
		path = filepath.Join(cwd, candidate)
	}
	info, err := os.Stat(path)
	if err != nil {
		return Attachment{}, false
	}
	att := Attachment{Path: path, Source: source}
	if info.IsDir() {
		att.Kind = KindDirectory
		return att, true
	}
	att.SizeBytes = info.Size()
	// Judge the REAL target too: a symlink named innocently but pointing at
	// ~/.ssh/id_rsa (or any credential file) must classify as a secret, not
	// sniff as text. EvalSymlinks failure (racing deletion, permission) falls
	// back to the unresolved path — the raw-candidate check still applies.
	resolved := path
	if r, err := filepath.EvalSymlinks(path); err == nil {
		resolved = r
	}
	if secrets.IsSecretPath(candidate) || secrets.IsSecretPath(resolved) {
		att.Kind = KindSecret
		return att, true
	}
	att.Kind, att.Mime = detectKind(path)
	// Hash for identity/dedup, but STREAM it (bounded memory) and skip entirely above the cap —
	// never read a 4GB attachment into memory just to fingerprint it (the runner enforces the
	// real send-size limits later; this is only the parser touching the file).
	if info.Size() <= maxHashBytes {
		if sum, ok := hashFile(path); ok {
			att.SHA256 = sum
		}
	}
	return att, true
}

// supportedImageMimes is the EXACT set the model accepts as an inline image. An image is
// classified by its real content (http.DetectContentType), never its extension — a .png that
// is actually text or a renamed binary must not be sent as image/png (the API would reject it).
var supportedImageMimes = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/gif":  true,
	"image/webp": true,
}

// detectKind classifies a file for the model by SNIFFING its first bytes: a supported image
// mime → KindImage (with the true mime); text → KindText; code/config that sniffs as octet is
// rescued by the extension whitelist; everything else → KindBinary (which the runtime rejects
// rather than sending). This is the supported-types gate — not "it's not an app, send it."
// maxHashBytes caps how large a file we'll read to fingerprint it — above this the SHA is
// skipped (empty) rather than streaming gigabytes off disk during input parsing.
const maxHashBytes = 100 * 1024 * 1024

// hashFile streams a file's SHA-256 so a huge attachment can't be slurped into memory.
func hashFile(path string) (string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", false
	}
	return hex.EncodeToString(h.Sum(nil)), true
}

func detectKind(path string) (Kind, string) {
	f, err := os.Open(path)
	if err != nil {
		return kindByExt(path)
	}
	head := make([]byte, 512)
	n, _ := f.Read(head)
	f.Close()
	sniff := strings.SplitN(http.DetectContentType(head[:n]), ";", 2)[0]
	switch {
	case supportedImageMimes[sniff]:
		return KindImage, sniff
	case sniff == "application/pdf":
		return KindPDF, "application/pdf"
	case strings.HasPrefix(sniff, "text/"):
		return KindText, "text/plain"
	}
	// Code/config (.go/.json/…) often sniffs as text/plain already, but some sniff as octet;
	// honor the text extension whitelist so a real source file is still sendable as text.
	if k, m := kindByExt(path); k == KindText {
		return KindText, m
	}
	return KindBinary, "application/octet-stream"
}

func kindByExt(path string) (Kind, string) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png":
		return KindImage, "image/png"
	case ".jpg", ".jpeg":
		return KindImage, "image/jpeg"
	case ".webp":
		return KindImage, "image/webp"
	case ".gif":
		return KindImage, "image/gif"
	case ".pdf":
		return KindPDF, "application/pdf"
	case ".txt", ".md", ".json", ".yaml", ".yml", ".log", ".csv",
		".go", ".js", ".jsx", ".ts", ".tsx", ".py", ".rs", ".sql", ".sh":
		return KindText, "text/plain"
	default:
		return KindBinary, "application/octet-stream"
	}
}
