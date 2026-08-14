// Package buildinfo reports build-time metadata. Release builds inject exact
// values via -ldflags (GoReleaser); local/dev builds (the dev wrapper, `go build`)
// have none, so we recover what we can from the embedded VCS stamp (commit +
// dirty flag) and the binary's own mtime as the build time. Because the dev
// wrapper rebuilds on every launch, that mtime makes each dev build uniquely
// identifiable — so a boot banner can tell you exactly which build is running.
package buildinfo

import (
	"fmt"
	"os"
	"runtime/debug"
	"time"
)

// baseVersion is the current development semver — the version the NEXT release will carry.
// Bump it when cutting a release/tag. It anchors dev builds to a real version number so the
// footer never shows a bare "dev"; the VCS commit is appended as the per-build identifier.
const baseVersion = "0.12.0"

// These are overridden at release time, e.g.:
//
//	-X github.com/memcode-ai/memcode/internal/buildinfo.Version=1.2.3
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

// resolve fills dev placeholders from the embedded VCS info and the binary mtime. When the
// version wasn't injected (plain `go build`), it synthesizes a REAL version from baseVersion
// plus the embedded commit — e.g. "0.1.0-dev+a1b2c3d" — never a bare "dev".
func resolve() (version, commit, date string) {
	version, commit, date = Version, Commit, Date

	if commit == "none" {
		if bi, ok := debug.ReadBuildInfo(); ok {
			var rev, mod string
			for _, s := range bi.Settings {
				switch s.Key {
				case "vcs.revision":
					rev = s.Value
				case "vcs.modified":
					mod = s.Value
				}
			}
			if rev != "" {
				if len(rev) > 7 {
					rev = rev[:7]
				}
				commit = rev
				if mod == "true" {
					commit += "-dirty" // uncommitted working tree
				}
			}
		}
	}

	// Not injected → a real dev version anchored to baseVersion + the commit.
	if version == "dev" {
		version = baseVersion + "-dev"
		if commit != "none" {
			version += "+" + commit
		}
	}

	if date == "unknown" {
		if exe, err := os.Executable(); err == nil {
			if fi, err := os.Stat(exe); err == nil {
				date = fi.ModTime().Format("2006-01-02 15:04:05")
			}
		}
	}
	return
}

// String is the full one-line version summary (for `memcode version`).
func String() string {
	v, c, d := resolve()
	return fmt.Sprintf("%s (commit %s, built %s)", v, c, d)
}

// Short is a compact identifier — "dev · a1b2c3d-dirty · built 11:02:05" — enough
// to tell two builds apart at a glance.
func Short() string {
	v, c, d := resolve()
	if c == "none" {
		return v
	}
	t := d
	if parsed, err := time.Parse("2006-01-02 15:04:05", d); err == nil {
		t = parsed.Format("15:04:05") // just the time
	}
	return fmt.Sprintf("%s · %s · built %s", v, c, t)
}

// Compact is the build identifier for the always-on footer — always a REAL version. A clean
// release shows just its semver (e.g. "1.2.3"); a dev build shows the synthesized
// baseVersion-dev+commit (e.g. "0.1.0-dev+a1b2c3d-dirty"), where the commit is the per-build
// identifier. No build timestamps — the footer carries a version, not a clock.
func Compact() string {
	v, _, _ := resolve()
	return v
}

// DevBuild is stamped "1" by the dev wrapper's ldflags. Default is OFF, so a
// release build (or any build that forgets the flag) can never accidentally
// show dev diagnostics.
var DevBuild = ""

// IsDev reports whether this is a local/dev build: the dev wrapper stamps
// DevBuild=1 (it also stamps a real semver, so Version alone can't tell), and
// a bare `go build` has no ldflags at all (Version stays "dev").
func IsDev() bool { return DevBuild == "1" || Version == "dev" }
