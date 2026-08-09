package skills

import (
	"bufio"
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

// This file integrates the skills.sh ecosystem through its `skills` CLI — the UN-gated path.
// `find` SEARCHES the public catalog with no auth (the skills.sh HTTP API requires a Vercel
// OIDC token, unsuitable for a distributed CLI); `add` INSTALLS a skill from GitHub into
// .agents/skills, where Discover then picks it up. memcode shells out rather than reimplement
// the resolver, so it tracks the ecosystem for free. The find output has no --json, so we read
// its stable line format. (Stripping ANSI with a regex is not parsing shell — the lines are
// split by fields, never by regex.)

// RemoteSkill is one search hit from the skills.sh catalog.
type RemoteSkill struct {
	Package  string // "owner/repo@skill" — the argument to `skills add`
	Installs string // reported install count, e.g. "509.5K" (informational)
	URL      string // https://skills.sh/owner/repo/skill
}

// ansiCodes matches terminal color escapes in the CLI's pretty output.
var ansiCodes = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// skillsCmd resolves how to invoke the `skills` CLI: a `skills` on PATH if installed, else
// `npx --yes skills@latest`. ok=false only when neither skills nor npx is available.
func skillsCmd() (bin string, prefix []string, ok bool) {
	if p, err := exec.LookPath("skills"); err == nil {
		return p, nil, true
	}
	if p, err := exec.LookPath("npx"); err == nil {
		return p, []string{"--yes", "skills@latest"}, true
	}
	return "", nil, false
}

// RemoteFind searches the skills.sh catalog for a query (optionally scoped to a GitHub owner).
// Read-only; needs network + node/npx. Returns the parsed matches.
func RemoteFind(ctx context.Context, query, owner string) ([]RemoteSkill, error) {
	bin, prefix, ok := skillsCmd()
	if !ok {
		return nil, fmt.Errorf("the `skills` CLI isn't available — install Node.js (npx) or `npm i -g skills`")
	}
	args := append(append([]string{}, prefix...), "find", query)
	if owner != "" {
		args = append(args, "--owner", owner)
	}
	out, err := exec.CommandContext(ctx, bin, args...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("`skills find` failed: %v: %s", err, clipOutput(out))
	}
	return parseFind(string(out)), nil
}

// parseFind extracts skills from `skills find` output. Each hit is a name line
// "owner/repo@skill   <N> installs" optionally followed by a "└ <url>" line.
func parseFind(out string) []RemoteSkill {
	var res []RemoteSkill
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := strings.TrimSpace(ansiCodes.ReplaceAllString(sc.Text(), ""))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "└") { // url line for the most recent hit
			if u := strings.TrimSpace(strings.TrimPrefix(line, "└")); strings.HasPrefix(u, "http") && len(res) > 0 {
				res[len(res)-1].URL = u
			}
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 || !strings.Contains(fields[0], "/") || !strings.Contains(fields[0], "@") {
			continue // not a "owner/repo@skill" hit line
		}
		hit := RemoteSkill{Package: fields[0]}
		for k := 1; k < len(fields); k++ { // installs = token just before the literal "installs"
			if fields[k] == "installs" {
				hit.Installs = fields[k-1]
			}
		}
		res = append(res, hit)
	}
	return res
}

// RemoteAdd installs a skill package ("owner/repo" or "owner/repo@skill") into dir's
// .agents/skills via the `skills` CLI. This EXECUTES external code and writes files — callers
// MUST gate it. Returns the (ANSI-stripped) CLI output on success.
func RemoteAdd(ctx context.Context, dir, pkg string) (string, error) {
	bin, prefix, ok := skillsCmd()
	if !ok {
		return "", fmt.Errorf("the `skills` CLI isn't available — install Node.js (npx) or `npm i -g skills`")
	}
	args := append(append([]string{}, prefix...), "add", pkg, "--yes")
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("`skills add %s` failed: %v: %s", pkg, err, clipOutput(out))
	}
	return strings.TrimSpace(ansiCodes.ReplaceAllString(string(out), "")), nil
}

func clipOutput(b []byte) string {
	s := strings.TrimSpace(ansiCodes.ReplaceAllString(string(b), ""))
	if len(s) > 500 {
		s = s[:500] + "…"
	}
	return s
}
