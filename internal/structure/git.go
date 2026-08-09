package structure

import (
	"bufio"
	"bytes"
	"context"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

// gitRepo gathers ownership and change-frequency signals by shelling out to git.
// All methods degrade to zero values when git is unavailable or the directory
// isn't a repository, so callers never need to special-case its absence.
type gitRepo struct {
	root      string
	available bool
}

func newGit(root string) *gitRepo {
	g := &gitRepo{root: root}
	if _, err := exec.LookPath("git"); err != nil {
		return g
	}
	// `git rev-parse` succeeds only inside a work tree.
	cmd := exec.Command("git", "-C", root, "rev-parse", "--is-inside-work-tree")
	if out, err := cmd.Output(); err == nil && strings.TrimSpace(string(out)) == "true" {
		g.available = true
	}
	return g
}

// recentWindowDays bounds "recent" activity. All-time totals are dominated by a
// repo's oldest, biggest area; recent activity is where work actually IS now —
// the more useful hotspot signal.
const recentWindowDays = 30

// maxFileChurn caps any single file's contribution to a subsystem's churn. A
// 40k-line committed data dump or fixture shouldn't outweigh hundreds of real
// source edits; this dampens single-file outliers (generated content we didn't
// pattern-match, JSON datasets, large fixtures) without enumerating every case.
const maxFileChurn = 1500

// stats returns total commits touching relpath, commits within the recent window
// (the better hotspot proxy), and the top authors. One `git log` carries the
// author and commit time per commit.
func (g *gitRepo) stats(ctx context.Context, relpath string) (commits, recent int, owners []string) {
	if g == nil || !g.available {
		return 0, 0, nil
	}
	cutoff := time.Now().AddDate(0, 0, -recentWindowDays).Unix()

	authors := map[string]int{}
	cmd := exec.CommandContext(ctx, "git", "-C", g.root, "log", "--format=%an|%ct", "--", relpath)
	out, err := cmd.Output()
	if err != nil {
		return 0, 0, nil
	}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		name, ts, ok := strings.Cut(strings.TrimSpace(sc.Text()), "|")
		if name = strings.TrimSpace(name); name == "" {
			continue
		}
		commits++
		authors[name]++
		if ok {
			if t, e := strconv.ParseInt(strings.TrimSpace(ts), 10, 64); e == nil && t >= cutoff {
				recent++
			}
		}
	}
	return commits, recent, topAuthors(authors, 3)
}

// generatedExcludes are git pathspecs that keep generated/vendored files from
// inflating churn — committed lockfiles, build output, minified bundles, vendored
// deps, and generated code. These dwarf hand-written changes (a single lockfile
// bump can be thousands of lines), so counting them makes churn a worse signal
// than commit count, not a better one.
var generatedExcludes = []string{
	":(glob,exclude)**/package-lock.json",
	":(glob,exclude)**/pnpm-lock.yaml",
	":(glob,exclude)**/yarn.lock",
	":(glob,exclude)**/bun.lock",
	":(glob,exclude)**/bun.lockb",
	":(glob,exclude)**/go.sum",
	":(glob,exclude)**/Cargo.lock",
	":(glob,exclude)**/poetry.lock",
	":(glob,exclude)**/Pipfile.lock",
	":(glob,exclude)**/composer.lock",
	":(glob,exclude)**/Gemfile.lock",
	":(glob,exclude)**/*.min.js",
	":(glob,exclude)**/*.min.css",
	":(glob,exclude)**/*.map",
	":(glob,exclude)**/dist/**",
	":(glob,exclude)**/build/**",
	":(glob,exclude)**/.next/**",
	":(glob,exclude)**/out/**",
	":(glob,exclude)**/vendor/**",
	":(glob,exclude)**/node_modules/**",
	":(glob,exclude)**/__generated__/**",
	":(glob,exclude)**/__snapshots__/**",
	":(glob,exclude)**/*.snap",
	":(glob,exclude)**/*.pb.go",
	":(glob,exclude)**/*_generated.go",
	":(glob,exclude)**/*.gen.go",
	":(glob,exclude)**/*_pb2.py",
}

// recentActivity returns the code churn (added+deleted lines) and the number of
// distinct active days for relpath within the recent window — the depth/sustain
// signals that commit COUNT misses. One `git log --numstat` over the window
// carries both: a "D<date>" line per commit, then numstat rows per file.
func (g *gitRepo) recentActivity(ctx context.Context, relpath string) (churn, activeDays int) {
	if g == nil || !g.available {
		return 0, 0
	}
	args := []string{"-C", g.root, "log",
		"--since", strconv.Itoa(recentWindowDays) + " days ago",
		"--no-merges", "--date=format:%Y-%m-%d", "--format=D%cd", "--numstat", "--", relpath}
	args = append(args, generatedExcludes...) // don't let lockfiles/build output inflate churn
	cmd := exec.CommandContext(ctx, "git", args...)
	out, err := cmd.Output()
	if err != nil {
		return 0, 0
	}
	days := map[string]bool{}
	perFile := map[string]int{} // churn per file, summed across commits in the window
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "D") { // commit marker: "D2026-06-02"
			days[line[1:]] = true
			continue
		}
		// numstat row: "<added>\t<deleted>\t<path>"; binary files show "-".
		fields := strings.Fields(line)
		if len(fields) >= 3 {
			n := 0
			if a, e := strconv.Atoi(fields[0]); e == nil {
				n += a
			}
			if d, e := strconv.Atoi(fields[1]); e == nil {
				n += d
			}
			perFile[fields[2]] += n
		}
	}
	// Cap each file's total contribution so one mega-file can't dominate.
	for _, n := range perFile {
		if n > maxFileChurn {
			n = maxFileChurn
		}
		churn += n
	}
	return churn, len(days)
}

func topAuthors(counts map[string]int, n int) []string {
	type kv struct {
		name string
		n    int
	}
	ranked := make([]kv, 0, len(counts))
	for name, c := range counts {
		ranked = append(ranked, kv{name, c})
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].n != ranked[j].n {
			return ranked[i].n > ranked[j].n
		}
		return ranked[i].name < ranked[j].name
	})
	var out []string
	for i, kv := range ranked {
		if i >= n {
			break
		}
		out = append(out, kv.name)
	}
	return out
}
