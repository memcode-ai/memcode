// kitsync.go — sites follow the kit.
//
// A site starts with the kit's exact versions pinned in its package.json. When
// memcode moves the kit (a security fix, a patch release), a site that still
// has the versions the kit gave it moves with the kit; a version the site's
// owner chose (`deps add pkg@x` for a kit package, an edit) stays where it is.
// The kit's history tells the two apart: every version the kit has ever
// pinned, with the epoch it shipped in.
//
// Epochs are the compatibility gate. A kit change that can break a site's code
// (a major release with API changes) starts a new epoch, and a site whose kit
// packages are from an older epoch is not moved automatically: it needs a
// migration. Within an epoch, following the kit is safe by construction.
//
// The local dev server syncs before it installs, and the hosted builder syncs
// before every build (`memcode websites deps sync`), so both land on the same
// versions.
package websites

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// KitHistoryFile sits next to the kit's package.json in the template. It is
// the template's, not the site's: scaffolding removes it from a new site.
const KitHistoryFile = "memcode-kit.json"

// KitHistory is every version the kit has pinned, by package, with the epoch
// each shipped in. Epoch is the kit's current epoch.
type KitHistory struct {
	Epoch    int                       `json:"epoch"`
	Versions map[string]map[string]int `json:"versions"`
}

func parseKitHistory(raw []byte) (KitHistory, error) {
	var h KitHistory
	if err := json.Unmarshal(raw, &h); err != nil {
		return KitHistory{}, fmt.Errorf("reading %s: %w", KitHistoryFile, err)
	}
	return h, nil
}

// KitMove is one package moving to the kit's version.
type KitMove struct {
	Name string `json:"name"`
	From string `json:"from"`
	To   string `json:"to"`
	Dev  bool   `json:"dev,omitempty"`
}

func (m KitMove) String() string { return m.Name + " " + m.From + " → " + m.To }

// KitPlan is what syncing a site with the kit would do.
type KitPlan struct {
	Moves []KitMove `json:"moves"`
	// Kept lists kit packages at a version the site's owner chose.
	Kept []string `json:"kept,omitempty"`
	// Migration lists kit packages from an older epoch. When it is non-empty,
	// nothing moves.
	Migration []string `json:"migration,omitempty"`
}

// PlanKitSync decides which of the site's packages follow the kit.
func PlanKitSync(site, kit Kit) KitPlan {
	var p KitPlan
	if kit.History.Epoch == 0 {
		return p // a kit without history can't tell kit versions from chosen ones
	}
	plan := func(siteDeps, kitDeps map[string]string, dev bool) {
		for _, name := range sortedKeys(siteDeps) {
			have := siteDeps[name]
			want, ok := kitDeps[name]
			if !ok || have == want {
				continue
			}
			epoch, fromKit := kit.History.Versions[name][have]
			switch {
			case !fromKit:
				p.Kept = append(p.Kept, name+"@"+have)
			case epoch != kit.History.Epoch:
				p.Migration = append(p.Migration, name+"@"+have)
			default:
				p.Moves = append(p.Moves, KitMove{Name: name, From: have, To: want, Dev: dev})
			}
		}
	}
	plan(site.Dependencies, kit.Dependencies, false)
	plan(site.DevDependencies, kit.DevDependencies, true)
	if len(p.Migration) > 0 {
		p.Moves = nil
	}
	return p
}

// SyncKit moves the site's kit packages to the kit's versions, updating
// package.json and package-lock.json (not node_modules). It returns the plan
// it carried out.
func SyncKit(ctx context.Context, dir string, kit Kit) (KitPlan, error) {
	site, err := SiteDeps(dir)
	if err != nil {
		return KitPlan{}, err
	}
	p := PlanKitSync(site, kit)
	var deps, dev []string
	for _, m := range p.Moves {
		if m.Dev {
			dev = append(dev, m.Name+"@"+m.To)
		} else {
			deps = append(deps, m.Name+"@"+m.To)
		}
	}
	for _, set := range []struct {
		specs []string
		flag  string
	}{{deps, "--save-prod"}, {dev, "--save-dev"}} {
		if len(set.specs) == 0 {
			continue
		}
		if err := npmLockOnly(ctx, dir, set.flag, set.specs); err != nil {
			return p, err
		}
	}
	return p, nil
}

func npmLockOnly(ctx context.Context, dir, flag string, specs []string) error {
	cctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()
	args := append([]string{"install", "--package-lock-only", "--save-exact", flag,
		"--ignore-scripts", "--no-audit", "--no-fund"}, specs...)
	cmd := exec.CommandContext(cctx, "npm", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("npm install %s failed: %s", strings.Join(specs, " "), lastLines(string(out), 15))
	}
	return nil
}

// Summary describes a plan in one line for the terminal.
func (p KitPlan) Summary() string {
	switch {
	case len(p.Migration) > 0:
		return "This site's kit packages are from an older memcode kit and need a migration: " + strings.Join(p.Migration, ", ")
	case len(p.Moves) == 0:
		return "Kit packages are current."
	}
	var moves []string
	for _, m := range p.Moves {
		moves = append(moves, m.String())
	}
	return "Updated memcode kit packages: " + strings.Join(moves, ", ")
}

// removeKitHistory drops the template's kit history from a freshly scaffolded
// site.
func removeKitHistory(dir string) error {
	err := os.Remove(filepath.Join(dir, KitHistoryFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
