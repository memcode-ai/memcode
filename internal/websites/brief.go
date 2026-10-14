package websites

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/memcode-ai/memcode/internal/atomicfile"
)

// Brief is what a site has been asked to be: the references it should look
// like and the requests made of it, kept with the site (in its gitignored
// .memcode/) so a later session that says "pick up where we left off" or
// "make sure it's like the reference" still knows what that was. The
// worker's design plan sits beside it in .memcode/design.md.
type Brief struct {
	References []string  `json:"references,omitempty"`
	Requests   []Request `json:"requests,omitempty"`
}

// Request is one change asked of the site.
type Request struct {
	At   time.Time `json:"at"`
	Text string    `json:"text"`
}

const (
	briefRelPath  = ".memcode/brief.json"
	designRelPath = ".memcode/design.md"
	maxRequests   = 12
)

// ReadBrief loads a site's brief (empty when it has none).
func ReadBrief(dir string) Brief {
	var b Brief
	if raw, err := os.ReadFile(filepath.Join(dir, briefRelPath)); err == nil {
		_ = json.Unmarshal(raw, &b)
	}
	return b
}

// Remember adds a request and any new references to the site's brief and
// returns the updated brief. References accumulate: one named once still
// applies to every later change.
func Remember(dir, request string, refs []string) (Brief, error) {
	b := ReadBrief(dir)
	seen := map[string]bool{}
	for _, r := range b.References {
		seen[normRef(r)] = true
	}
	for _, r := range refs {
		r = strings.TrimSpace(r)
		if r != "" && !seen[normRef(r)] {
			seen[normRef(r)] = true
			b.References = append(b.References, r)
		}
	}
	if t := strings.TrimSpace(request); t != "" {
		b.Requests = append(b.Requests, Request{At: time.Now().UTC(), Text: t})
		if len(b.Requests) > maxRequests {
			b.Requests = b.Requests[len(b.Requests)-maxRequests:]
		}
	}
	raw, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return b, err
	}
	if err := os.MkdirAll(filepath.Join(dir, ".memcode"), 0o755); err != nil {
		return b, err
	}
	return b, atomicfile.WriteFile(filepath.Join(dir, briefRelPath), append(raw, '\n'), 0o644)
}

// DesignPlan is the worker's saved design plan for the site ("" if none).
func DesignPlan(dir string) string {
	raw, err := os.ReadFile(filepath.Join(dir, designRelPath))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

func normRef(r string) string {
	r = strings.ToLower(strings.TrimSpace(r))
	for _, p := range []string{"https://", "http://", "www."} {
		r = strings.TrimPrefix(r, p)
	}
	return strings.TrimRight(r, "/")
}
