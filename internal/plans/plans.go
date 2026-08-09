// Package plans is the user-level store of saved plans — one markdown file per plan under
// ~/.memcode/plans, mirroring Claude Code's ~/.claude/plans. A plan is written the moment it's
// presented for approval and updated in place across revisions, so it outlives the session and
// can be re-opened later by asking the agent to recall it (the recall_plan tool). Plain markdown,
// no frontmatter: the file IS
// the plan, renderable as-is. User-level (not per-repo) so plans accumulate in one place.
package plans

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/memcode-ai/memcode/internal/atomicfile"
)

// Dir is the user-level plans directory (~/.memcode/plans). Created on demand by Save.
func Dir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".memcode", "plans"), nil
}

// Ref is one saved plan's metadata, for listing.
type Ref struct {
	Slug  string
	Title string
	Saved time.Time
	Path  string
}

// Save writes the plan markdown to ~/.memcode/plans/<slug>.md, creating the dir if needed. An
// empty slug mints a fresh memorable one (adjective-gerund-noun, like Claude Code's plan names);
// pass a returned slug back to UPDATE the same file across revisions of one plan. Returns the slug
// used so the caller can reuse it.
func Save(slug, plan string) (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if slug == "" {
		slug = freshSlug(dir)
	}
	body := strings.TrimRight(plan, "\n") + "\n"
	if err := atomicfile.WriteFile(filepath.Join(dir, slug+".md"), []byte(body), 0o644); err != nil {
		return "", err
	}
	return slug, nil
}

// List returns saved plans, newest first (by file modtime).
func List() ([]Ref, error) {
	dir, err := Dir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var refs []Ref
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		path := filepath.Join(dir, e.Name())
		refs = append(refs, Ref{
			Slug:  strings.TrimSuffix(e.Name(), ".md"),
			Title: titleOf(path),
			Saved: info.ModTime(),
			Path:  path,
		})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Saved.After(refs[j].Saved) })
	return refs, nil
}

// Read returns the markdown of one saved plan.
func Read(slug string) (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(filepath.Join(dir, slug+".md"))
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// titleOf reads the first non-empty line of a plan file as its title — stripping a leading
// markdown "#"s and a "Plan:"/"PLAN:" label — clipped to one line. "" on any error.
func titleOf(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, ln := range strings.Split(string(data), "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		ln = strings.TrimSpace(strings.TrimLeft(ln, "#"))
		for _, p := range []string{"PLAN:", "Plan:", "plan:"} {
			ln = strings.TrimSpace(strings.TrimPrefix(ln, p))
		}
		if len([]rune(ln)) > 80 {
			ln = string([]rune(ln)[:79]) + "…"
		}
		return ln
	}
	return ""
}

var (
	planAdjs  = []string{"calm", "mighty", "hazy", "dapper", "graceful", "whimsical", "cryptic", "mellow", "dazzling", "flickering", "brave", "quiet", "lucky", "nimble", "gentle", "bold", "clever", "cosmic", "fuzzy", "amber"}
	planVerbs = []string{"cooking", "growing", "baking", "leaping", "percolating", "jumping", "painting", "forging", "seeking", "tinkering", "gathering", "drifting", "humming", "wandering", "building", "scheming", "sketching", "roaming", "weaving", "charting"}
	planNouns = []string{"seahorse", "deer", "pillow", "boot", "hearth", "balloon", "brooks", "toucan", "otter", "comet", "lantern", "harbor", "willow", "cobble", "marble", "thistle", "falcon", "meadow", "cinder", "quartz"}
)

// freshSlug mints an "adjective-gerund-noun" slug (e.g. "calm-cooking-otter"), like Claude Code's
// plan names, retrying on collision with an existing file in dir; falls back to a numeric suffix.
func freshSlug(dir string) string {
	for try := 0; try < 12; try++ {
		s := pick(planAdjs) + "-" + pick(planVerbs) + "-" + pick(planNouns)
		if _, err := os.Stat(filepath.Join(dir, s+".md")); os.IsNotExist(err) {
			return s
		}
	}
	return fmt.Sprintf("%s-%s-%s-%d", pick(planAdjs), pick(planVerbs), pick(planNouns), time.Now().UnixNano()%100000)
}

func pick(xs []string) string {
	n, err := rand.Int(rand.Reader, big.NewInt(int64(len(xs))))
	if err != nil {
		return xs[0]
	}
	return xs[n.Int64()]
}
