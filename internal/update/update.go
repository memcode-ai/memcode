// Package update implements self-update against the public GitHub Releases
// repo (memcode-ai/memcode — the same source the curl|sh installer uses). Three
// surfaces: `memcode upgrade` (explicit, synchronous, replaces the running
// binary after checksum verification), the silent startup self-update (Auto —
// same daily cadence, stages the new binary in the background; the running
// session is untouched and the next launch runs it; MEMCODE_AUTO_UPDATE=off
// keeps it manual), and a passive "update available" notice (cached in
// ~/.memcode/update-check.json, never blocks, never nags a dev build).
package update

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/memcode-ai/memcode/internal/buildinfo"
)

// ReleaseRepo is the public binary repo (goreleaser's release target).
const ReleaseRepo = "memcode-ai/memcode"

// apiBase is a var so tests can point it at an httptest server.
var apiBase = "https://api.github.com"

// downloadBase is the release-asset host (var for tests).
var downloadBase = "https://github.com"

var metaClient = &http.Client{Timeout: 15 * time.Second}
var downloadClient = &http.Client{Timeout: 5 * time.Minute}

// LatestVersion returns the newest release tag (e.g. "v0.3.1").
func LatestVersion(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		apiBase+"/repos/"+ReleaseRepo+"/releases/latest", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := metaClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("release lookup: http %d", resp.StatusCode)
	}
	var rel struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rel); err != nil {
		return "", err
	}
	if rel.TagName == "" {
		return "", errors.New("release lookup: empty tag_name")
	}
	return rel.TagName, nil
}

// IsNewer reports whether latest is a strictly newer semver than current.
// Pre-release/build suffixes ("-dev+abc") are stripped before comparing; a
// malformed version compares as not-newer (never upgrade on garbage input).
func IsNewer(current, latest string) bool {
	c, okC := parseSemver(current)
	l, okL := parseSemver(latest)
	if !okC || !okL {
		return false
	}
	for i := 0; i < 3; i++ {
		if l[i] != c[i] {
			return l[i] > c[i]
		}
	}
	return false
}

func parseSemver(v string) ([3]int, bool) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return [3]int{}, false
	}
	var out [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return [3]int{}, false
		}
		out[i] = n
	}
	return out, true
}

// AssetName is the goreleaser archive name for this platform, e.g.
// "memcode_Darwin_arm64.tar.gz" or "memcode_Windows_x86_64.zip".
func AssetName() string {
	osName := map[string]string{"darwin": "Darwin", "linux": "Linux", "windows": "Windows"}[runtime.GOOS]
	arch := runtime.GOARCH
	if arch == "amd64" {
		arch = "x86_64"
	}
	ext := ".tar.gz"
	if runtime.GOOS == "windows" {
		ext = ".zip"
	}
	return "memcode_" + osName + "_" + arch + ext
}

// Upgrade downloads the latest release, verifies its sha256 against
// checksums.txt, and atomically replaces the running binary. Progress goes to
// out. Returns the version installed ("" when already up to date).
func Upgrade(ctx context.Context, out io.Writer) (string, error) {
	if buildinfo.IsDev() {
		return "", errors.New("this is a dev build — rebuild from source instead of self-updating")
	}
	current := buildinfo.Compact()
	latest, err := LatestVersion(ctx)
	if err != nil {
		return "", fmt.Errorf("checking latest release: %w", err)
	}
	if !IsNewer(current, latest) {
		fmt.Fprintf(out, "memcode %s is up to date (latest is %s)\n", current, latest)
		return "", nil
	}

	asset := AssetName()
	base := downloadBase + "/" + ReleaseRepo + "/releases/download/" + latest
	fmt.Fprintf(out, "upgrading %s → %s\n", current, latest)

	tmpDir, err := os.MkdirTemp("", "memcode-upgrade-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmpDir)

	archive := filepath.Join(tmpDir, asset)
	fmt.Fprintf(out, "downloading %s\n", asset)
	if err := downloadFile(ctx, base+"/"+asset, archive); err != nil {
		return "", fmt.Errorf("downloading %s: %w", asset, err)
	}

	// Checksum is MANDATORY for self-update (unlike the installer's best-effort):
	// we are overwriting the running binary, so a corrupt download must abort.
	sums := filepath.Join(tmpDir, "checksums.txt")
	if err := downloadFile(ctx, base+"/checksums.txt", sums); err != nil {
		return "", fmt.Errorf("downloading checksums.txt: %w", err)
	}
	if err := verifyChecksum(archive, asset, sums); err != nil {
		return "", err
	}
	fmt.Fprintln(out, "checksum verified")

	binName := "memcode"
	if runtime.GOOS == "windows" {
		binName = "memcode.exe"
	}
	newBin := filepath.Join(tmpDir, "new-"+binName)
	if err := extractBinary(archive, binName, newBin); err != nil {
		return "", fmt.Errorf("extracting %s: %w", binName, err)
	}

	if err := replaceExecutable(newBin); err != nil {
		return "", err
	}
	fmt.Fprintf(out, "✓ memcode %s installed\n", latest)
	return latest, nil
}

func downloadFile(ctx context.Context, url, dst string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := downloadClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("http %d", resp.StatusCode)
	}
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// verifyChecksum checks file against the goreleaser checksums.txt entry for
// name ("<sha256>  <name>" lines).
func verifyChecksum(file, name, sumsPath string) error {
	sums, err := os.ReadFile(sumsPath)
	if err != nil {
		return err
	}
	var want string
	for _, line := range strings.Split(string(sums), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == name {
			want = strings.ToLower(fields[0])
			break
		}
	}
	if want == "" {
		return fmt.Errorf("checksums.txt has no entry for %s", name)
	}
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != want {
		return fmt.Errorf("checksum mismatch for %s: got %s want %s", name, got, want)
	}
	return nil
}

// extractBinary pulls binName out of a .tar.gz or .zip archive into dst (0755).
func extractBinary(archive, binName, dst string) error {
	if strings.HasSuffix(archive, ".zip") {
		return extractZip(archive, binName, dst)
	}
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if filepath.Base(hdr.Name) == binName && hdr.Typeflag == tar.TypeReg {
			return writeFile(dst, tr)
		}
	}
	return fmt.Errorf("%s not found in archive", binName)
}

func extractZip(archive, binName, dst string) error {
	zr, err := zip.OpenReader(archive)
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, zf := range zr.File {
		if filepath.Base(zf.Name) == binName && !zf.FileInfo().IsDir() {
			rc, err := zf.Open()
			if err != nil {
				return err
			}
			defer rc.Close()
			return writeFile(dst, rc)
		}
	}
	return fmt.Errorf("%s not found in archive", binName)
}

func writeFile(dst string, r io.Reader) error {
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	// Cap at 512MB — a release binary is ~tens of MB; anything bigger is wrong.
	if _, err := io.Copy(f, io.LimitReader(r, 512<<20)); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// replaceExecutable atomically swaps the running binary for newBin: write a
// temp file NEXT TO the target (same filesystem → rename is atomic), then
// rename over it. Windows can't rename over a running exe, so there the old
// binary is moved aside first.
func replaceExecutable(newBin string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	dir := filepath.Dir(exe)

	src, err := os.Open(newBin)
	if err != nil {
		return err
	}
	defer src.Close()
	tmp, err := os.CreateTemp(dir, ".memcode-upgrade-*")
	if err != nil {
		return permissionHint(err, dir)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := io.Copy(tmp, src); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		cleanup()
		return err
	}

	if runtime.GOOS == "windows" {
		old := exe + ".old"
		_ = os.Remove(old)
		if err := os.Rename(exe, old); err != nil {
			cleanup()
			return permissionHint(err, dir)
		}
		if err := os.Rename(tmpName, exe); err != nil {
			_ = os.Rename(old, exe) // roll back
			cleanup()
			return err
		}
		return nil
	}
	if err := os.Rename(tmpName, exe); err != nil {
		cleanup()
		return permissionHint(err, dir)
	}
	return nil
}

func permissionHint(err error, dir string) error {
	if errors.Is(err, os.ErrPermission) {
		return fmt.Errorf("%w — %s is not writable; re-run with sudo, or reinstall: curl -fsSL https://memcode.ai/install.sh | sh", err, dir)
	}
	return err
}

// --- passive daily notice ----------------------------------------------------

// ReexecStaged replaces the current process with the already-staged newer
// binary, so "the next launch runs it" is THIS launch. Cache-only and
// instant: no network, no version probe — the staged marker is written only
// after a checksum-verified install. No-op on Windows (no exec) and for dev
// builds. Loop-proof twice over: the re-exec'd binary IS the staged version
// (IsNewer fails), and MEMCODE_REEXEC guards the pathological case.
func ReexecStaged() {
	if buildinfo.IsDev() || runtime.GOOS == "windows" || os.Getenv("MEMCODE_REEXEC") != "" {
		return
	}
	target, ok := reexecTarget()
	if !ok {
		return
	}
	env := append(os.Environ(), "MEMCODE_REEXEC=1")
	_ = syscallExec(target, os.Args, env) // on failure, just run the current build
}

// reexecTarget reports the executable path to re-exec when the staged install
// is newer than the running build. Split from ReexecStaged for testability.
func reexecTarget() (string, bool) {
	c, ok := readCache()
	if !ok || c.Installed == "" || !IsNewer(buildinfo.Compact(), c.Installed) {
		return "", false
	}
	exe, err := os.Executable()
	if err != nil {
		return "", false
	}
	return exe, true
}

// checkCache is ~/.memcode/update-check.json.
type checkCache struct {
	CheckedAt time.Time `json:"checked_at"`
	Latest    string    `json:"latest"`
	// Installed dedups the silent self-update within one check window: the
	// release Auto already staged on disk, so concurrently-running OLD
	// binaries don't each re-download it. Deliberately NOT carried across
	// cache refreshes — the daily re-check re-stages, which also means a
	// manual downgrade is re-upgraded within a day (opt out via
	// MEMCODE_AUTO_UPDATE=off, not by downgrading).
	Installed string `json:"installed,omitempty"`
}

// 6h, not 24: a check that lands minutes before a release used to poison the
// cache for a whole day ("auto update doesn't work"). Four unauthenticated
// GitHub API calls a day is nothing.
const checkTTL = 6 * time.Hour

func cachePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".memcode", "update-check.json")
}

func readCache() (checkCache, bool) {
	p := cachePath()
	if p == "" {
		return checkCache{}, false
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return checkCache{}, false
	}
	var c checkCache
	if json.Unmarshal(b, &c) != nil || c.Latest == "" {
		return checkCache{}, false
	}
	return c, true
}

func writeCache(c checkCache) {
	p := cachePath()
	if p == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return
	}
	b, err := json.Marshal(c)
	if err != nil {
		return
	}
	_ = os.WriteFile(p, b, 0o644)
}

// notice formats the user-facing update line ("" when current is latest).
func notice(latest string) string {
	if latest == "" || !IsNewer(buildinfo.Compact(), latest) {
		return ""
	}
	return fmt.Sprintf("memcode %s is available (you have %s) — run `memcode upgrade`", latest, buildinfo.Compact())
}

// NoticeFromCache is the zero-network variant: reads only the cached check.
// Safe to call from anywhere (version output, doctor).
func NoticeFromCache() string {
	if buildinfo.IsDev() {
		return ""
	}
	c, ok := readCache()
	if !ok {
		return ""
	}
	return notice(c.Latest)
}

// --- silent startup self-update ---------------------------------------------

// EnvAutoUpdate opts OUT of the silent startup self-update ("0" | "false" |
// "off" | "no"): the daily check then only surfaces the passive notice, like
// before. Machine-scoped — set it in the global env file, like every knob.
const EnvAutoUpdate = "MEMCODE_AUTO_UPDATE"

func autoUpdateOff() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(EnvAutoUpdate))) {
	case "0", "false", "off", "no":
		return true
	}
	return false
}

// installLatest is the download→verify→replace core, delegated to Upgrade
// (which re-resolves latest itself — one redundant metadata call per day, and
// race-safe: whatever is newest AT INSTALL TIME wins). A var so tests stub the
// install without overwriting the test binary.
var installLatest = func(ctx context.Context) error {
	_, err := Upgrade(ctx, io.Discard)
	return err
}

// Auto is the startup self-update: BackgroundNotice's activist sibling, on the
// same daily cadence and the same silence contract (run it on a goroutine;
// boot never waits on it; a failed check or install must never surface as an
// error). When a newer release exists it downloads, checksum-verifies, and
// atomically swaps the binary ON DISK — the running session keeps its inode
// and is untouched; the next launch runs the new version. The returned line
// (printed after the TUI releases the terminal) reports what happened: the
// staged version, or the manual nudge when auto-update is off, mid-install
// elsewhere, or the install failed. Concurrent sessions single-flight through
// a lock file, and the release just staged is remembered in the check cache
// so still-running old binaries don't each re-download it. The kill-switch
// story for shipped policy bugs: cut a release, the fleet converges on next
// launch — no user action.
func Auto(ctx context.Context) string {
	if buildinfo.IsDev() {
		return ""
	}
	if autoUpdateOff() {
		return BackgroundNotice(ctx)
	}
	// Resolve latest: the fresh cache first (zero network), else one check.
	c, cached := readCache()
	if !cached || time.Since(c.CheckedAt) >= checkTTL {
		lctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		latest, err := LatestVersion(lctx)
		if err != nil {
			return ""
		}
		c = checkCache{CheckedAt: time.Now(), Latest: latest}
		writeCache(c)
	}
	if !IsNewer(buildinfo.Compact(), c.Latest) {
		return ""
	}
	if c.Installed == c.Latest {
		return staged(c.Latest) // already on disk — this session just predates it
	}
	unlock, ok := lockUpdate()
	if !ok {
		return notice(c.Latest) // another session is mid-install
	}
	defer unlock()
	if err := installLatest(ctx); err != nil {
		return notice(c.Latest) // silent failure → the manual nudge
	}
	c.Installed = c.Latest
	writeCache(c)
	return staged(c.Latest)
}

func staged(latest string) string {
	return fmt.Sprintf("✓ memcode %s installed — the next launch runs it", latest)
}

// lockUpdate single-flights concurrent sessions' installs via an exclusive
// lock file next to the check cache. A stale lock (a killed process) expires
// after an hour.
func lockUpdate() (func(), bool) {
	p := cachePath()
	if p == "" {
		return nil, false
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return nil, false
	}
	lock := filepath.Join(filepath.Dir(p), "upgrade.lock")
	if fi, err := os.Stat(lock); err == nil && time.Since(fi.ModTime()) > time.Hour {
		_ = os.Remove(lock)
	}
	f, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, false
	}
	f.Close()
	return func() { _ = os.Remove(lock) }, true
}

// BackgroundNotice refreshes the cache when stale (one network call per 24h)
// and returns the update line, or "". Errors are silent — a failed check must
// never surface in the product. Intended to run on a goroutine.
func BackgroundNotice(ctx context.Context) string {
	if buildinfo.IsDev() {
		return ""
	}
	if c, ok := readCache(); ok && time.Since(c.CheckedAt) < checkTTL {
		return notice(c.Latest)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	latest, err := LatestVersion(ctx)
	if err != nil {
		return ""
	}
	writeCache(checkCache{CheckedAt: time.Now(), Latest: latest})
	return notice(latest)
}
