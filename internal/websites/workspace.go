// workspace.go — the on-disk layout `memcode websites dev` works from.
//
// Two modes, mirroring the revised architecture's LOCAL layer:
//
//	managed:  ~/.memcode/websites/<site_id>/{source,assets,runtime}/ — used
//	          when `dev` is given a slug with no local checkout. Mirrors
//	          internal/plans's ~/.memcode/plans convention (user-level, not
//	          per-repo, so it survives outside any git working tree).
//	explicit: <dir>/.memcode/{assets,runtime}/ — used when `dev` is pointed
//	          at an existing pulled checkout. The caller MUST run
//	          config.EnsureGitignore(dir) before writing here (that's what
//	          keeps .memcode/assets and .memcode/runtime untracked and out of
//	          `git bundle create --all`) — this package deliberately does not
//	          import internal/config to avoid widening its dependency surface.
//
// Neither mode ever touches draft/ or repo.bundle — those stay exactly as
// the Pull/Push flow already manages them.
package websites

import (
	"os"
	"path/filepath"
)

// ManagedDir returns ~/.memcode/websites/<site_id>, creating no directories.
func ManagedDir(siteID string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".memcode", "websites", siteID), nil
}

// Workspace is the resolved {source, assets, runtime} layout `dev` works
// from, whichever mode produced it.
type Workspace struct {
	Root       string // managed: ManagedDir(siteID); explicit: the checkout dir itself
	Managed    bool
	SourceDir  string // the site's repo working tree (== Root for explicit mode)
	AssetsDir  string // asset cache, fully refreshed on every `dev` run
	RuntimeDir string // generated, untracked dev-server config override
}

// ManagedWorkspace resolves (and creates assets/ + runtime/ for) the managed
// workspace for siteID. source/ is created separately, only when the caller
// actually clones into it (see Workspace.SourceDir, populated on demand).
func ManagedWorkspace(siteID string) (Workspace, error) {
	root, err := ManagedDir(siteID)
	if err != nil {
		return Workspace{}, err
	}
	ws := Workspace{
		Root:       root,
		Managed:    true,
		SourceDir:  filepath.Join(root, "source"),
		AssetsDir:  filepath.Join(root, "assets"),
		RuntimeDir: filepath.Join(root, "runtime"),
	}
	if err := ws.ensureDirs(); err != nil {
		return Workspace{}, err
	}
	return ws, nil
}

// ExplicitWorkspace resolves (and creates .memcode/assets + .memcode/runtime
// for) the workspace rooted at an existing checkout dir. Callers MUST call
// config.EnsureGitignore(dir) first — see the package doc comment.
func ExplicitWorkspace(dir string) (Workspace, error) {
	base := filepath.Join(dir, ".memcode")
	ws := Workspace{
		Root:       dir,
		Managed:    false,
		SourceDir:  dir,
		AssetsDir:  filepath.Join(base, "assets"),
		RuntimeDir: filepath.Join(base, "runtime"),
	}
	if err := ws.ensureDirs(); err != nil {
		return Workspace{}, err
	}
	return ws, nil
}

func (ws Workspace) ensureDirs() error {
	for _, d := range []string{ws.AssetsDir, ws.RuntimeDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	return nil
}

// SourceExists reports whether SourceDir already holds a cloned repo (a
// non-empty directory) — the signal `dev` uses to decide whether to Pull.
func (ws Workspace) SourceExists() bool {
	entries, err := os.ReadDir(ws.SourceDir)
	return err == nil && len(entries) > 0
}
