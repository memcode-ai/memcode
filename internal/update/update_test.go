package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/memcode-ai/memcode/internal/buildinfo"
)

func TestIsNewer(t *testing.T) {
	cases := []struct {
		current, latest string
		want            bool
	}{
		{"0.1.0", "v0.1.1", true},
		{"v0.1.1", "v0.1.1", false},
		{"0.2.0", "v0.1.9", false},
		{"1.9.9", "2.0.0", true},
		{"0.1.0-dev+abc1234", "v0.1.0", false}, // suffix stripped, equal
		{"0.1.0-dev+abc1234", "v0.1.1", true},
		{"garbage", "v1.0.0", false}, // malformed never upgrades
		{"1.0.0", "not-a-version", false},
	}
	for _, c := range cases {
		if got := IsNewer(c.current, c.latest); got != c.want {
			t.Errorf("IsNewer(%q, %q) = %v, want %v", c.current, c.latest, got, c.want)
		}
	}
}

func TestAssetNameMatchesGoreleaser(t *testing.T) {
	name := AssetName()
	// e.g. memcode_Darwin_arm64.tar.gz / memcode_Linux_x86_64.tar.gz
	if !strings.HasPrefix(name, "memcode_") {
		t.Fatalf("asset name %q lacks project prefix", name)
	}
	if runtime.GOOS == "windows" && !strings.HasSuffix(name, ".zip") {
		t.Fatalf("windows asset %q must be a zip", name)
	}
	if runtime.GOOS != "windows" && !strings.HasSuffix(name, ".tar.gz") {
		t.Fatalf("unix asset %q must be a tar.gz", name)
	}
	if runtime.GOARCH == "amd64" && !strings.Contains(name, "x86_64") {
		t.Fatalf("amd64 must map to x86_64 in %q", name)
	}
}

// tarGzWith returns a .tar.gz archive containing name→content.
func tarGzWith(t *testing.T, name string, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestVerifyChecksumAndExtract(t *testing.T) {
	dir := t.TempDir()
	content := []byte("#!/bin/sh\necho fake memcode\n")
	archiveBytes := tarGzWith(t, "memcode", content)

	archive := filepath.Join(dir, "memcode_Test_arm64.tar.gz")
	if err := os.WriteFile(archive, archiveBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(archiveBytes)
	sums := filepath.Join(dir, "checksums.txt")
	line := hex.EncodeToString(sum[:]) + "  memcode_Test_arm64.tar.gz\n"
	if err := os.WriteFile(sums, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := verifyChecksum(archive, "memcode_Test_arm64.tar.gz", sums); err != nil {
		t.Fatalf("checksum should verify: %v", err)
	}
	if err := verifyChecksum(archive, "missing.tar.gz", sums); err == nil {
		t.Fatal("missing checksum entry must fail")
	}
	// Corrupt the archive → mismatch.
	if err := os.WriteFile(archive, append(archiveBytes, 'x'), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := verifyChecksum(archive, "memcode_Test_arm64.tar.gz", sums); err == nil {
		t.Fatal("corrupted archive must fail checksum")
	}
	if err := os.WriteFile(archive, archiveBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(dir, "extracted")
	if err := extractBinary(archive, "memcode", dst); err != nil {
		t.Fatalf("extract: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Fatal("extracted content differs")
	}
	if err := extractBinary(archive, "nope", filepath.Join(dir, "x")); err == nil {
		t.Fatal("extracting a missing member must fail")
	}
}

func TestLatestVersionParsesTag(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/"+ReleaseRepo+"/releases/latest" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"tag_name":"v9.9.9"}`)
	}))
	defer srv.Close()
	old := apiBase
	apiBase = srv.URL
	defer func() { apiBase = old }()

	got, err := LatestVersion(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != "v9.9.9" {
		t.Fatalf("got %q", got)
	}
}

func TestNoticeCacheRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	// No cache → no notice, no network.
	if n := NoticeFromCache(); n != "" {
		t.Fatalf("empty cache must be silent, got %q", n)
	}
	writeCache(checkCache{Latest: "v999.0.0"})
	c, ok := readCache()
	if !ok || c.Latest != "v999.0.0" {
		t.Fatalf("cache round-trip failed: %+v ok=%v", c, ok)
	}
	// notice() compares against the running build; a dev build suppresses via
	// the public wrappers, so test the formatter directly.
	if n := notice("v999.0.0"); n == "" {
		t.Fatal("v999.0.0 should beat any current build")
	}
	if n := notice(""); n != "" {
		t.Fatal("empty latest must be silent")
	}
}

// stubRelease makes the test binary look like a real vX release so Auto's
// dev-build guard doesn't suppress the paths under test.
func stubRelease(t *testing.T, version string) {
	t.Helper()
	oldV, oldDev := buildinfo.Version, buildinfo.DevBuild
	buildinfo.Version, buildinfo.DevBuild = version, ""
	t.Cleanup(func() { buildinfo.Version, buildinfo.DevBuild = oldV, oldDev })
}

// stubInstall swaps the download→replace core; returns the call counter.
func stubInstall(t *testing.T, err error) *int {
	t.Helper()
	calls := 0
	old := installLatest
	installLatest = func(context.Context) error { calls++; return err }
	t.Cleanup(func() { installLatest = old })
	return &calls
}

func TestAutoStagesWhenNewer(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stubRelease(t, "1.0.0")
	calls := stubInstall(t, nil)
	writeCache(checkCache{CheckedAt: time.Now(), Latest: "v1.1.0"}) // fresh check, newer

	n := Auto(context.Background())
	if *calls != 1 {
		t.Fatalf("install calls = %d, want 1", *calls)
	}
	if !strings.Contains(n, "v1.1.0") || !strings.Contains(n, "next launch") {
		t.Fatalf("staged line = %q", n)
	}
	c, _ := readCache()
	if c.Installed != "v1.1.0" {
		t.Fatalf("cache.Installed = %q, want v1.1.0", c.Installed)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(cachePath()), "upgrade.lock")); err == nil {
		t.Fatal("lock file must be released after install")
	}

	// A second session on the SAME old binary must not re-download: the cache
	// remembers the staged release and only the restart line returns.
	n2 := Auto(context.Background())
	if *calls != 1 {
		t.Fatalf("second Auto re-installed (calls=%d)", *calls)
	}
	if !strings.Contains(n2, "v1.1.0") {
		t.Fatalf("second Auto line = %q", n2)
	}
}

func TestAutoUpToDateIsSilent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stubRelease(t, "2.0.0")
	calls := stubInstall(t, nil)
	writeCache(checkCache{CheckedAt: time.Now(), Latest: "v2.0.0"})
	if n := Auto(context.Background()); n != "" || *calls != 0 {
		t.Fatalf("up-to-date must be silent: n=%q calls=%d", n, *calls)
	}
}

func TestAutoKillSwitchKeepsNoticeOnly(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stubRelease(t, "1.0.0")
	calls := stubInstall(t, nil)
	t.Setenv(EnvAutoUpdate, "off")
	writeCache(checkCache{CheckedAt: time.Now(), Latest: "v1.1.0"})

	n := Auto(context.Background())
	if *calls != 0 {
		t.Fatalf("kill switch must prevent install (calls=%d)", *calls)
	}
	if !strings.Contains(n, "memcode upgrade") {
		t.Fatalf("want the manual nudge, got %q", n)
	}
}

func TestAutoInstallFailureFallsBackToNotice(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stubRelease(t, "1.0.0")
	stubInstall(t, errors.New("network died"))
	writeCache(checkCache{CheckedAt: time.Now(), Latest: "v1.1.0"})

	n := Auto(context.Background())
	if !strings.Contains(n, "memcode upgrade") {
		t.Fatalf("failed install must fall back to the nudge, got %q", n)
	}
	if c, _ := readCache(); c.Installed != "" {
		t.Fatalf("failed install must not stamp Installed, got %q", c.Installed)
	}
}

func TestAutoConcurrentSessionYieldsToLock(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stubRelease(t, "1.0.0")
	calls := stubInstall(t, nil)
	writeCache(checkCache{CheckedAt: time.Now(), Latest: "v1.1.0"})
	unlock, ok := lockUpdate() // another session holds the lock
	if !ok {
		t.Fatal("test could not take the lock")
	}
	defer unlock()

	n := Auto(context.Background())
	if *calls != 0 {
		t.Fatalf("locked Auto must not install (calls=%d)", *calls)
	}
	if !strings.Contains(n, "memcode upgrade") {
		t.Fatalf("locked Auto returns the nudge, got %q", n)
	}
}

func TestAutoDevBuildIsInert(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	calls := stubInstall(t, nil)
	writeCache(checkCache{CheckedAt: time.Now(), Latest: "v99.0.0"})
	// The test binary IS a dev build (no ldflags) — no stubRelease here.
	if n := Auto(context.Background()); n != "" || *calls != 0 {
		t.Fatalf("dev build must never self-update: n=%q calls=%d", n, *calls)
	}
}

func TestAutoRefreshesStaleCache(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stubRelease(t, "1.0.0")
	calls := stubInstall(t, nil)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"tag_name":"v1.2.0"}`)
	}))
	defer srv.Close()
	old := apiBase
	apiBase = srv.URL
	defer func() { apiBase = old }()
	// Stale cache with a stamped Installed: the refresh must re-check AND drop
	// Installed (that is what re-stages after a manual downgrade).
	writeCache(checkCache{CheckedAt: time.Now().Add(-25 * time.Hour), Latest: "v1.1.0", Installed: "v1.1.0"})

	n := Auto(context.Background())
	if *calls != 1 {
		t.Fatalf("stale cache must re-check and install (calls=%d)", *calls)
	}
	if !strings.Contains(n, "v1.2.0") {
		t.Fatalf("staged line = %q", n)
	}
	if c, _ := readCache(); c.Latest != "v1.2.0" || c.Installed != "v1.2.0" {
		t.Fatalf("refreshed cache = %+v", c)
	}
}
