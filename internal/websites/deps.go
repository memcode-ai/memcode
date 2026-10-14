// deps.go — what a site may depend on, decided the same way everywhere.
//
// A site's packages come from two places:
//
//   - The kit: the template's own dependency set (React, Tailwind, motion,
//     GSAP, fonts, 3D…). The hosted builder has it preinstalled, so a site
//     that uses only the kit builds offline in seconds.
//   - Vetted additions: any other npm package that passes Vet — old enough
//     to have survived a compromised release being noticed, widely used, a
//     permissive license, no install scripts, not deprecated, a sane size.
//     Additions install with --ignore-scripts here and on memcode.ai.
//
// The local site worker adds packages through Add, a push runs Check before
// uploading, and the hosted builder runs the same Check (`memcode websites
// deps check`) before it installs anything. One policy, one implementation:
// a site that works locally builds when pushed, and nothing reaches the
// builder that this machine would have refused.
package websites

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Policy limits for a package outside the kit.
const (
	// MinPackageAge: a version must have been public this long. Compromised
	// releases are typically caught and pulled within days.
	MinPackageAge = 14 * 24 * time.Hour
	// MinWeeklyDownloads keeps additions to packages many people already run.
	MinWeeklyDownloads = 10000
	// MaxUnpackedBytes bounds one package's published size.
	MaxUnpackedBytes = 30 << 20
)

// permissiveLicenses are the SPDX ids an added package may carry.
var permissiveLicenses = map[string]bool{
	"MIT": true, "MIT-0": true, "ISC": true, "BSD-2-Clause": true, "BSD-3-Clause": true,
	"Apache-2.0": true, "0BSD": true, "Unlicense": true, "CC0-1.0": true, "OFL-1.1": true,
	"MPL-2.0": true, "BlueOak-1.0.0": true, "Zlib": true, "CC-BY-4.0": true,
}

// trustedFontScopes publish font files only (no code), so they skip the
// download floor: a less popular typeface is still just a typeface.
var trustedFontScopes = []string{"@fontsource/", "@fontsource-variable/"}

// Kit is the template's dependency set.
type Kit struct {
	Dependencies    map[string]string `json:"dependencies"`
	DevDependencies map[string]string `json:"devDependencies"`
}

// Has reports whether name@version is a kit runtime dependency.
func (k Kit) Has(name, version string) bool {
	v, ok := k.Dependencies[name]
	return ok && v == version
}

// ReadKit reads a kit from a package.json file.
func ReadKit(path string) (Kit, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Kit{}, err
	}
	return parseKit(raw)
}

func parseKit(raw []byte) (Kit, error) {
	var k Kit
	if err := json.Unmarshal(raw, &k); err != nil {
		return Kit{}, fmt.Errorf("reading package.json: %w", err)
	}
	if k.Dependencies == nil {
		k.Dependencies = map[string]string{}
	}
	if k.DevDependencies == nil {
		k.DevDependencies = map[string]string{}
	}
	return k, nil
}

// Kit returns the current template's dependency set, from the same template
// memcode.ai scaffolds new sites from (and bakes into the hosted builder).
func (c *Client) Kit(ctx context.Context) (Kit, error) {
	raw, err := c.do(ctx, http.MethodGet, "/api/website/template", nil, "")
	if err != nil {
		return Kit{}, fmt.Errorf("getting the site template: %w", err)
	}
	var tpl struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(raw, &tpl); err != nil || tpl.URL == "" {
		return Kit{}, errors.New("the site template is unavailable")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, tpl.URL, nil)
	if err != nil {
		return Kit{}, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return Kit{}, fmt.Errorf("downloading the site template: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return Kit{}, fmt.Errorf("downloading the site template: %s", resp.Status)
	}
	pkg, err := packageJSONFromTarGz(resp.Body)
	if err != nil {
		return Kit{}, err
	}
	return parseKit(pkg)
}

// packageJSONFromTarGz pulls the root package.json out of a template tarball.
func packageJSONFromTarGz(r io.Reader) ([]byte, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil, errors.New("the site template has no package.json")
		}
		if err != nil {
			return nil, err
		}
		if filepath.Clean(h.Name) == "package.json" {
			return io.ReadAll(io.LimitReader(tr, 1<<20))
		}
	}
}

// BaselineKit is the dependency set the site itself started from: package.json
// in its first commit. The fallback when the current template can't be read.
func BaselineKit(dir string) (Kit, error) {
	out, err := exec.Command("git", "-C", dir, "rev-list", "--max-parents=0", "HEAD").Output()
	if err != nil {
		return Kit{}, fmt.Errorf("finding the site's first commit: %w", err)
	}
	roots := strings.Fields(string(out))
	if len(roots) == 0 {
		return Kit{}, errors.New("the site has no commits")
	}
	raw, err := exec.Command("git", "-C", dir, "show", roots[len(roots)-1]+":package.json").Output()
	if err != nil {
		return Kit{}, fmt.Errorf("reading the site's original package.json: %w", err)
	}
	return parseKit(raw)
}

// SiteKit is the kit to judge a site against: the current template's when
// memcode.ai can be reached, else the one the site started from.
func SiteKit(ctx context.Context, c *Client, dir string) Kit {
	if c != nil {
		if k, err := c.Kit(ctx); err == nil {
			return k
		}
	}
	k, err := BaselineKit(dir)
	if err != nil {
		return Kit{Dependencies: map[string]string{}, DevDependencies: map[string]string{}}
	}
	return k
}

// Registry reads package facts from npm.
type Registry struct {
	BaseURL      string // registry, e.g. https://registry.npmjs.org
	DownloadsURL string // downloads API, e.g. https://api.npmjs.org/downloads/point/last-week
	HTTP         *http.Client
	Now          func() time.Time
}

// NewRegistry returns a registry client for the public npm registry.
func NewRegistry() *Registry {
	return &Registry{
		BaseURL:      "https://registry.npmjs.org",
		DownloadsURL: "https://api.npmjs.org/downloads/point/last-week",
		HTTP:         &http.Client{Timeout: 90 * time.Second},
		Now:          time.Now,
	}
}

// Verdict is the policy's answer for one package.
type Verdict struct {
	Name      string    `json:"name"`
	Version   string    `json:"version"`
	Kit       bool      `json:"kit,omitempty"`
	OK        bool      `json:"ok"`
	Reason    string    `json:"reason,omitempty"` // why not, when !OK
	License   string    `json:"license,omitempty"`
	Downloads int       `json:"weekly_downloads,omitempty"`
	Published time.Time `json:"published,omitzero"`
}

// Summary is the verdict in one line for the user and the model.
func (v Verdict) Summary() string {
	id := v.Name
	if v.Version != "" {
		id += "@" + v.Version
	}
	switch {
	case !v.OK:
		return id + " is not allowed: " + v.Reason
	case v.Kit:
		return id + " (from the memcode kit)"
	default:
		return fmt.Sprintf("%s (%s, %s weekly downloads, published %s)", id, v.License, humanCount(v.Downloads), v.Published.Format("2006-01-02"))
	}
}

func humanCount(n int) string {
	switch {
	case n >= 1_000_000:
		return strconv.FormatFloat(float64(n)/1_000_000, 'f', 1, 64) + "M"
	case n >= 1_000:
		return strconv.Itoa(n/1_000) + "k"
	}
	return strconv.Itoa(n)
}

var (
	validName    = regexp.MustCompile(`^(@[a-z0-9][a-z0-9._~-]*/)?[a-z0-9][a-z0-9._~-]*$`)
	exactVersion = regexp.MustCompile(`^\d+\.\d+\.\d+$`)
)

// npm registry document: just the fields the policy reads.
type npmDoc struct {
	Versions map[string]npmManifest `json:"versions"`
	Time     map[string]string      `json:"time"`
}

type npmManifest struct {
	License        json.RawMessage   `json:"license"`
	Deprecated     json.RawMessage   `json:"deprecated"`
	Scripts        map[string]string `json:"scripts"`
	HasInstallHook bool              `json:"hasInstallScript"`
	Dist           struct {
		UnpackedSize int64 `json:"unpackedSize"`
	} `json:"dist"`
}

// Vet judges a package outside the kit. version "" picks the newest version
// that passes; an explicit version is judged as given.
func (r *Registry) Vet(ctx context.Context, name, version string) Verdict {
	v := Verdict{Name: name, Version: version}
	if !validName.MatchString(name) {
		v.Reason = "not a valid npm package name"
		return v
	}
	if version != "" && !exactVersion.MatchString(version) {
		v.Reason = "versions must be exact (like 1.2.3), not ranges, tags or URLs"
		return v
	}
	doc, err := r.doc(ctx, name)
	if err != nil {
		v.Reason = err.Error()
		return v
	}
	now := r.Now()
	if version == "" {
		version = newestEligible(doc, now)
		if version == "" {
			v.Reason = fmt.Sprintf("no stable, non-deprecated release is at least %d days old", int(MinPackageAge.Hours()/24))
			return v
		}
		v.Version = version
	}
	man, ok := doc.Versions[version]
	if !ok {
		v.Reason = "npm has no version " + version
		return v
	}
	if published, err := time.Parse(time.RFC3339, doc.Time[version]); err == nil {
		v.Published = published
		if now.Sub(published) < MinPackageAge {
			v.Reason = fmt.Sprintf("version %s was published %s; versions must be at least %d days old (an older release works)",
				version, published.Format("2006-01-02"), int(MinPackageAge.Hours()/24))
			return v
		}
	} else {
		v.Reason = "npm doesn't say when " + version + " was published"
		return v
	}
	if deprecated(man) {
		v.Reason = "version " + version + " is deprecated"
		return v
	}
	if hook := installHook(man); hook != "" {
		v.Reason = "it runs an install script (" + hook + "); sites install packages with scripts off, so it would not work"
		return v
	}
	if man.Dist.UnpackedSize > MaxUnpackedBytes {
		v.Reason = fmt.Sprintf("it is %d MB unpacked; the limit is %d MB", man.Dist.UnpackedSize>>20, MaxUnpackedBytes>>20)
		return v
	}
	v.License = licenseOf(man.License)
	if !licenseAllowed(v.License) {
		lic := v.License
		if lic == "" {
			lic = "none declared"
		}
		v.Reason = "its license (" + lic + ") isn't a permissive one"
		return v
	}
	if !trustedFont(name) {
		n, err := r.weeklyDownloads(ctx, name)
		if err != nil {
			v.Reason = "couldn't check its download count: " + err.Error()
			return v
		}
		v.Downloads = n
		if n < MinWeeklyDownloads {
			v.Reason = fmt.Sprintf("only %s weekly downloads; packages need at least %s", humanCount(n), humanCount(MinWeeklyDownloads))
			return v
		}
	}
	v.OK = true
	return v
}

func (r *Registry) doc(ctx context.Context, name string) (npmDoc, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.BaseURL+"/"+url.PathEscape(name), nil)
	if err != nil {
		return npmDoc{}, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := r.HTTP.Do(req)
	if err != nil {
		return npmDoc{}, fmt.Errorf("npm is unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return npmDoc{}, errors.New("there is no such package on npm")
	}
	if resp.StatusCode != http.StatusOK {
		return npmDoc{}, fmt.Errorf("npm returned %s", resp.Status)
	}
	var doc npmDoc
	if err := json.NewDecoder(io.LimitReader(resp.Body, 256<<20)).Decode(&doc); err != nil {
		return npmDoc{}, fmt.Errorf("reading npm's answer: %w", err)
	}
	return doc, nil
}

// downloadsAttempts and downloadsTimeout: npm's downloads API is slow and
// flaky (504s, stalls) far more often than the registry, so each try is
// short and a few are made before the check fails closed.
var (
	downloadsAttempts = 3
	downloadsTimeout  = 20 * time.Second
	downloadsBackoff  = 2 * time.Second
)

func (r *Registry) weeklyDownloads(ctx context.Context, name string) (int, error) {
	var lastErr error
	for i := 0; i < downloadsAttempts; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return 0, ctx.Err()
			case <-time.After(downloadsBackoff * time.Duration(i)):
			}
		}
		n, err := r.weeklyDownloadsOnce(ctx, name)
		if err == nil {
			return n, nil
		}
		lastErr = err
	}
	return 0, lastErr
}

func (r *Registry) weeklyDownloadsOnce(ctx context.Context, name string) (int, error) {
	cctx, cancel := context.WithTimeout(ctx, downloadsTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, r.DownloadsURL+"/"+name, nil)
	if err != nil {
		return 0, err
	}
	resp, err := r.HTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("npm returned %s", resp.Status)
	}
	var out struct {
		Downloads int `json:"downloads"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return 0, err
	}
	return out.Downloads, nil
}

// newestEligible is the highest stable, non-deprecated version old enough.
func newestEligible(doc npmDoc, now time.Time) string {
	best := ""
	for ver, man := range doc.Versions {
		if !exactVersion.MatchString(ver) || deprecated(man) {
			continue
		}
		published, err := time.Parse(time.RFC3339, doc.Time[ver])
		if err != nil || now.Sub(published) < MinPackageAge {
			continue
		}
		if best == "" || versionLess(best, ver) {
			best = ver
		}
	}
	return best
}

// versionLess compares two exact x.y.z versions numerically.
func versionLess(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < 3; i++ {
		x, _ := strconv.Atoi(pa[i])
		y, _ := strconv.Atoi(pb[i])
		if x != y {
			return x < y
		}
	}
	return false
}

func deprecated(m npmManifest) bool {
	d := strings.TrimSpace(string(m.Deprecated))
	return d != "" && d != "null" && d != "false" && d != `""`
}

func installHook(m npmManifest) string {
	for _, k := range []string{"preinstall", "install", "postinstall"} {
		if strings.TrimSpace(m.Scripts[k]) != "" {
			return k
		}
	}
	if m.HasInstallHook {
		return "install"
	}
	return ""
}

// licenseOf reads the license field: an SPDX string, or the legacy
// {"type": "MIT"} object.
func licenseOf(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	var o struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &o) == nil {
		return strings.TrimSpace(o.Type)
	}
	return ""
}

// licenseAllowed accepts an SPDX id or a simple expression: "A OR B" needs one
// permissive choice, "A AND B" needs all of them.
func licenseAllowed(expr string) bool {
	e := strings.TrimSpace(strings.NewReplacer("(", " ", ")", " ").Replace(expr))
	if e == "" {
		return false
	}
	if strings.Contains(e, " OR ") {
		for _, part := range strings.Split(e, " OR ") {
			if licenseAllowed(part) {
				return true
			}
		}
		return false
	}
	if strings.Contains(e, " AND ") {
		for _, part := range strings.Split(e, " AND ") {
			if !licenseAllowed(part) {
				return false
			}
		}
		return true
	}
	return permissiveLicenses[strings.TrimSpace(e)]
}

func trustedFont(name string) bool {
	for _, p := range trustedFontScopes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// SiteDeps reads a site's package.json.
func SiteDeps(dir string) (Kit, error) {
	return ReadKit(filepath.Join(dir, "package.json"))
}

// Check judges every dependency of the site at dir against kit and the
// policy. It returns one verdict per dependency outside the kit (the kit is
// fine by definition) plus any devDependency change, which is never allowed:
// the build toolchain is the template's.
func Check(ctx context.Context, dir string, kit Kit, reg *Registry) ([]Verdict, error) {
	site, err := SiteDeps(dir)
	if err != nil {
		return nil, err
	}
	var out []Verdict
	for _, name := range sortedKeys(site.DevDependencies) {
		if want, ok := kit.DevDependencies[name]; !ok || want != site.DevDependencies[name] {
			out = append(out, Verdict{Name: name, Version: site.DevDependencies[name],
				Reason: "build tooling (devDependencies) is fixed by the template; use dependencies for packages the site imports"})
		}
	}
	for _, name := range sortedKeys(site.Dependencies) {
		version := site.Dependencies[name]
		if kit.Has(name, version) {
			continue
		}
		if !exactVersion.MatchString(version) {
			out = append(out, Verdict{Name: name, Version: version,
				Reason: "versions must be exact (like 1.2.3), not ranges, tags or URLs"})
			continue
		}
		out = append(out, reg.Vet(ctx, name, version))
	}
	return out, nil
}

// Problems keeps the verdicts that fail.
func Problems(vs []Verdict) []Verdict {
	var bad []Verdict
	for _, v := range vs {
		if !v.OK {
			bad = append(bad, v)
		}
	}
	return bad
}

// ErrDependencies is a site whose package.json the policy refuses.
type ErrDependencies struct{ Problems []Verdict }

func (e *ErrDependencies) Error() string {
	lines := make([]string, len(e.Problems))
	for i, p := range e.Problems {
		lines[i] = p.Summary()
	}
	return "the site's dependencies won't build on memcode.ai: " + strings.Join(lines, "; ")
}

// Add installs one package into the site at dir: a kit package at the kit's
// version, anything else only when Vet passes. Installs never run package
// scripts, exactly as on memcode.ai.
func Add(ctx context.Context, dir, name, version string, kit Kit, reg *Registry) (Verdict, error) {
	name = strings.TrimSpace(name)
	version = strings.TrimSpace(strings.TrimPrefix(version, "v"))
	if i := strings.LastIndex(name, "@"); i > 0 { // "gsap@3.13.0" (not the scope's leading @)
		name, version = name[:i], strings.TrimPrefix(name[i+1:], "v")
	}
	var v Verdict
	if kv, ok := kit.Dependencies[name]; ok && (version == "" || version == kv) {
		v = Verdict{Name: name, Version: kv, Kit: true, OK: true}
	} else {
		v = reg.Vet(ctx, name, version)
	}
	if !v.OK {
		return v, nil
	}
	cctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(cctx, "npm", "install", "--save-exact", "--ignore-scripts", "--no-audit", "--no-fund", v.Name+"@"+v.Version)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		return v, fmt.Errorf("npm install %s@%s failed: %s", v.Name, v.Version, lastLines(string(out), 15))
	}
	return v, nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// lastLines keeps the tail of command output, where the error usually is.
func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}
