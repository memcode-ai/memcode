package stack

import (
	"fmt"
	"sort"
	"strings"
)

// RepoLine summarizes the repository shape in one line: "git · monorepo (4 modules)
// · pnpm workspaces, Turborepo". Empty if nothing detected.
func RepoLine(r RepoFacts) string {
	var parts []string
	if r.VCS != "" {
		parts = append(parts, r.VCS)
	}
	if r.Monorepo {
		m := "monorepo"
		if r.Modules > 0 {
			m = fmt.Sprintf("monorepo (%d modules)", r.Modules)
		}
		parts = append(parts, m)
	} else if r.Modules == 1 {
		parts = append(parts, "single module")
	}
	if len(r.Workspace) > 0 {
		parts = append(parts, strings.Join(r.Workspace, ", "))
	}
	return strings.Join(parts, " · ")
}

// Render formats StackFacts for `memcode stack` — repo shape + a language bar + a
// grouped detected-stack table. Plain text.
func Render(f StackFacts) string {
	var b strings.Builder
	if rl := RepoLine(f.Repo); rl != "" {
		b.WriteString("Repo\n  " + rl + "\n\n")
	}
	if len(f.Languages) > 0 {
		b.WriteString("Languages\n")
		for _, l := range topLangs(f.Languages, 8) {
			b.WriteString(fmt.Sprintf("  %-12s %4.1f%%\n", l.Name, l.Pct))
		}
	}
	rows := [][2]string{
		{"Runtime", names(f.Runtimes)},
		{"Framework", names(f.Frameworks)},
		{"CLI", names(f.CLIs)},
		{"Database", names(f.Databases)},
		{"Infra", names(f.Infra)},
		{"CI", names(f.CI)},
	}
	any := false
	for _, r := range rows {
		if r[1] != "" {
			any = true
		}
	}
	if any {
		if len(f.Languages) > 0 {
			b.WriteString("\n")
		}
		b.WriteString("Stack\n")
		for _, r := range rows {
			if r[1] != "" {
				b.WriteString(fmt.Sprintf("  %-10s %s\n", r[0], r[1]))
			}
		}
	}
	if b.Len() == 0 {
		return "no stack detected — no recognized manifests or source files."
	}
	return strings.TrimRight(b.String(), "\n")
}

// FactSheet is the compact, deterministic block injected into /overview so the
// model RENDERS the stack from facts instead of inferring it. One line per group.
func FactSheet(f StackFacts) string {
	var b strings.Builder
	if langs := topLangs(f.Languages, 6); len(langs) > 0 {
		parts := make([]string, len(langs))
		for i, l := range langs {
			parts[i] = fmt.Sprintf("%s %.0f%%", l.Name, l.Pct)
		}
		b.WriteString("Languages: " + strings.Join(parts, ", ") + "\n")
	}
	line := func(label string, ts []TechFact) {
		if s := names(ts); s != "" {
			b.WriteString(label + ": " + s + "\n")
		}
	}
	line("Runtimes", f.Runtimes)
	line("Frameworks", f.Frameworks)
	line("CLIs", f.CLIs)
	line("Databases", f.Databases)
	line("Infra", f.Infra)
	line("CI", f.CI)
	if b.Len() == 0 {
		return ""
	}
	return "DETECTED STACK (deterministic facts — render these verbatim; do NOT infer the\n" +
		"stack from commit text or subsystem names):\n" + b.String()
}

// Brief is the stack as an indented block for embedding under an /overview "Stack"
// header — the same canonical facts as `memcode stack`, no outer labels.
func Brief(f StackFacts) string {
	var b strings.Builder
	if langs := topLangs(f.Languages, 6); len(langs) > 0 {
		parts := make([]string, len(langs))
		for i, l := range langs {
			parts[i] = fmt.Sprintf("%s %.1f%%", l.Name, l.Pct)
		}
		b.WriteString("  " + strings.Join(parts, "  ") + "\n")
	}
	row := func(label string, ts []TechFact) {
		if s := names(ts); s != "" {
			b.WriteString(fmt.Sprintf("  %-10s %s\n", label, s))
		}
	}
	row("Runtime", f.Runtimes)
	row("Framework", f.Frameworks)
	row("CLI", f.CLIs)
	row("Database", f.Databases)
	row("Infra", f.Infra)
	row("CI", f.CI)
	return strings.TrimRight(b.String(), "\n")
}

func topLangs(ls []LanguageStat, n int) []LanguageStat {
	if len(ls) > n {
		ls = ls[:n]
	}
	return ls
}

func names(ts []TechFact) string {
	if len(ts) == 0 {
		return ""
	}
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.Name
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}
