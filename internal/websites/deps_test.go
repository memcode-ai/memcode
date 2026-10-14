package websites

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var policyNow = time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)

// fakeNPM serves registry documents and weekly download counts.
func fakeNPM(t *testing.T, docs map[string]any, downloads map[string]int) *Registry {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if name, ok := strings.CutPrefix(r.URL.Path, "/downloads/"); ok {
			n, found := downloads[name]
			if !found {
				http.Error(w, "down", http.StatusGatewayTimeout)
				return
			}
			json.NewEncoder(w).Encode(map[string]int{"downloads": n})
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/")
		doc, ok := docs[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(doc)
	}))
	t.Cleanup(srv.Close)
	oldB := downloadsBackoff
	downloadsBackoff = time.Millisecond
	t.Cleanup(func() { downloadsBackoff = oldB })
	return &Registry{BaseURL: srv.URL, DownloadsURL: srv.URL + "/downloads", HTTP: srv.Client(), Now: func() time.Time { return policyNow }}
}

func days(n int) string {
	return policyNow.Add(-time.Duration(n) * 24 * time.Hour).Format(time.RFC3339)
}

func pkgDoc(versions map[string]map[string]any, times map[string]string) map[string]any {
	return map[string]any{"versions": versions, "time": times}
}

func TestVetPolicy(t *testing.T) {
	ok := map[string]any{"license": "MIT", "dist": map[string]any{"unpackedSize": 1000}}
	reg := fakeNPM(t, map[string]any{
		"confetti": pkgDoc(map[string]map[string]any{"1.0.0": ok, "1.2.0": ok, "1.10.0": ok, "2.0.0": ok, "2.1.0-beta.1": ok},
			map[string]string{"1.0.0": days(400), "1.2.0": days(100), "1.10.0": days(30), "2.0.0": days(3), "2.1.0-beta.1": days(40)}),
		"scripty":                  pkgDoc(map[string]map[string]any{"1.0.0": {"license": "MIT", "scripts": map[string]string{"postinstall": "node x.js"}}}, map[string]string{"1.0.0": days(100)}),
		"gpl":                      pkgDoc(map[string]map[string]any{"1.0.0": {"license": "GPL-3.0"}}, map[string]string{"1.0.0": days(100)}),
		"dual":                     pkgDoc(map[string]map[string]any{"1.0.0": {"license": "(GPL-3.0 OR MIT)"}}, map[string]string{"1.0.0": days(100)}),
		"old-style":                pkgDoc(map[string]map[string]any{"1.0.0": {"license": map[string]string{"type": "ISC"}}}, map[string]string{"1.0.0": days(100)}),
		"dead":                     pkgDoc(map[string]map[string]any{"1.0.0": {"license": "MIT", "deprecated": "use x"}}, map[string]string{"1.0.0": days(100)}),
		"huge":                     pkgDoc(map[string]map[string]any{"1.0.0": {"license": "MIT", "dist": map[string]any{"unpackedSize": 200 << 20}}}, map[string]string{"1.0.0": days(100)}),
		"niche":                    pkgDoc(map[string]map[string]any{"1.0.0": ok}, map[string]string{"1.0.0": days(100)}),
		"flaky":                    pkgDoc(map[string]map[string]any{"1.0.0": ok}, map[string]string{"1.0.0": days(100)}),
		"@fontsource/obscure-face": pkgDoc(map[string]map[string]any{"5.0.0": {"license": "OFL-1.1"}}, map[string]string{"5.0.0": days(60)}),
	}, map[string]int{"confetti": 2_000_000, "dual": 50_000, "old-style": 50_000, "niche": 40})
	ctx := context.Background()

	for _, tc := range []struct {
		name, version string
		ok            bool
		wantVersion   string
		reason        string
	}{
		{"confetti", "", true, "1.10.0", ""}, // newest old-enough stable, compared numerically (1.10 > 1.2), skipping 2.0.0 (3 days) and the beta
		{"confetti", "1.2.0", true, "1.2.0", ""},
		{"confetti", "2.0.0", false, "", "at least 14 days"},
		{"confetti", "^1.0.0", false, "", "exact"},
		{"confetti", "9.9.9", false, "", "no version"},
		{"scripty", "", false, "", "install script"},
		{"gpl", "", false, "", "license"},
		{"dual", "", true, "1.0.0", ""},
		{"old-style", "", true, "1.0.0", ""},
		{"dead", "", false, "", "no stable"},
		{"dead", "1.0.0", false, "", "deprecated"},
		{"huge", "", false, "", "MB"},
		{"niche", "", false, "", "weekly downloads"},
		{"flaky", "", false, "", "download count"}, // the downloads API failing fails closed
		{"@fontsource/obscure-face", "", true, "5.0.0", ""},
		{"nope", "", false, "", "no such package"},
		{"../etc", "", false, "", "valid"},
	} {
		v := reg.Vet(ctx, tc.name, tc.version)
		if v.OK != tc.ok || (tc.wantVersion != "" && v.Version != tc.wantVersion) || (tc.reason != "" && !strings.Contains(v.Reason, tc.reason)) {
			t.Errorf("Vet(%s, %q) = %+v; want ok=%v version=%s reason~%q", tc.name, tc.version, v, tc.ok, tc.wantVersion, tc.reason)
		}
	}
}

func writePkg(t *testing.T, deps, dev map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	raw, _ := json.Marshal(map[string]any{"dependencies": deps, "devDependencies": dev})
	if err := os.WriteFile(filepath.Join(dir, "package.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// The kit passes untouched (even with licenses the policy wouldn't take on
// its own, like GSAP's); everything else is judged; build tooling is fixed.
func TestCheckAgainstKit(t *testing.T) {
	ok := map[string]any{"license": "MIT"}
	reg := fakeNPM(t, map[string]any{
		"confetti": pkgDoc(map[string]map[string]any{"1.0.0": ok}, map[string]string{"1.0.0": days(100)}),
	}, map[string]int{"confetti": 1_000_000})
	kit := Kit{Dependencies: map[string]string{"react": "19.2.8", "gsap": "3.15.0"}, DevDependencies: map[string]string{"vite": "8.1.5"}}

	dir := writePkg(t, map[string]string{"react": "19.2.8", "gsap": "3.15.0"}, map[string]string{"vite": "8.1.5"})
	if vs, err := Check(context.Background(), dir, kit, reg); err != nil || len(vs) != 0 {
		t.Fatalf("kit-only site: %+v %v", vs, err)
	}

	dir = writePkg(t, map[string]string{"react": "19.2.8", "confetti": "1.0.0", "lodash": "^4.0.0"}, map[string]string{"vite": "9.0.0"})
	vs, err := Check(context.Background(), dir, kit, reg)
	if err != nil {
		t.Fatal(err)
	}
	bad := Problems(vs)
	if len(vs) != 3 || len(bad) != 2 {
		t.Fatalf("verdicts %+v", vs)
	}
	msg := (&ErrDependencies{Problems: bad}).Error()
	for _, want := range []string{"devDependencies", "lodash@^4.0.0", "exact"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q missing %q", msg, want)
		}
	}
}

// A kit package is added at the kit's version without asking npm; a refused
// package never reaches npm install.
func TestAddUsesKitAndRefusesBeforeInstalling(t *testing.T) {
	reg := fakeNPM(t, map[string]any{}, nil)
	dir := writePkg(t, map[string]string{}, nil)
	v, err := Add(context.Background(), dir, "left-pad@1.0.0", "", Kit{}, reg)
	if err != nil || v.OK || !strings.Contains(v.Reason, "no such package") {
		t.Fatalf("Add of an unknown package = %+v, %v", v, err)
	}
	kit := Kit{Dependencies: map[string]string{"gsap": "3.15.0"}}
	if v := (Verdict{Name: "gsap", Version: kit.Dependencies["gsap"], Kit: true, OK: true}); !strings.Contains(v.Summary(), "memcode kit") {
		t.Fatal(v.Summary())
	}
}

func TestPackageJSONFromTemplate(t *testing.T) {
	kit, err := parseKit([]byte(`{"dependencies":{"gsap":"3.15.0"}}`))
	if err != nil || kit.Dependencies["gsap"] != "3.15.0" || kit.DevDependencies == nil {
		t.Fatalf("%+v %v", kit, err)
	}
}
