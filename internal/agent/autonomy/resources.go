package autonomy

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

type ResourceType string

const (
	ResourceFilesystem       ResourceType = "filesystem"
	ResourceBrowser          ResourceType = "browser"
	ResourceMCP              ResourceType = "mcp"
	ResourceCommand          ResourceType = "command"
	ResourceRepository       ResourceType = "repository"
	ResourceCloud            ResourceType = "cloud"
	ResourceDocument         ResourceType = "document"
	ResourceChannel          ResourceType = "channel"
	ResourceGeneratedProcess ResourceType = "generated_process"
)

type ResourceGrantModel struct {
	ID                                      string
	Type                                    ResourceType
	Locator, AccessMode                     string
	Constraints                             map[string]any
	AuthorizationSource, PolicyHash, Status string
	ExpiresAt                               *time.Time
}

func (g ResourceGrantModel) Active(now time.Time) bool {
	return g.Status == "active" && (g.ExpiresAt == nil || now.Before(*g.ExpiresAt))
}
func CanonicalFilesystemGrant(path string) (string, error) {
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		path = filepath.Join(home, strings.TrimPrefix(path, "~/"))
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	// A grant may be a single file (e.g. a resume) or a directory root — both
	// work with PathWithinGrant unchanged (a file grant's only "contained"
	// path is itself: rel == "."). Requiring a directory here would force
	// granting a whole folder just to share one file, which is both more
	// ceremony and a broader grant than the task needs.
	if _, err := os.Stat(resolved); err != nil {
		return "", err
	}
	return resolved, nil
}

// PathWithinGrant reports whether path resolves (symlinks evaluated) to a
// location inside the canonical grant root. The requested path's symlinks are
// resolved before the containment check so a symlink inside a granted dir that
// points outside cannot escape the boundary.
func PathWithinGrant(path, root string) bool {
	// Resolve the path fully. For a write to a not-yet-existing file, EvalSymlinks
	// fails on the leaf; resolve the deepest existing ancestor and re-join the rest.
	resolvedPath := resolveDeep(path)
	resolvedRoot := resolveDeep(root)
	rel, err := filepath.Rel(resolvedRoot, resolvedPath)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// resolveDeep resolves symlinks on the longest existing prefix of path, then
// re-attaches the non-existent tail. This lets us contain a write to a new file
// while still catching a symlinked parent that escapes the grant.
func resolveDeep(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		return r
	}
	// Walk up until an existing ancestor resolves.
	dir := abs
	var tail []string
	for {
		parent := filepath.Dir(dir)
		if parent == dir {
			return abs
		}
		tail = append([]string{filepath.Base(dir)}, tail...)
		dir = parent
		if r, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(append([]string{r}, tail...)...)
		}
	}
}
