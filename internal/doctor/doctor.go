// Package doctor runs deterministic health checks over a memcode project — the
// built-in answer to "is the runtime actually doing what we think?". It exists
// because this is the question that kept recurring while building: the .memcode
// self-ignore silently not firing, the key not resolving, the index not built.
// /doctor (TUI) and `memcode doctor` (CLI) both render these.
package doctor

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/memcode-ai/memcode/internal/config"
	"github.com/memcode-ai/memcode/internal/lsp"
	"github.com/memcode-ai/memcode/internal/provider"
	"github.com/memcode-ai/memcode/internal/store"
)

// Status is a check outcome. Warn = works but worth knowing; Fail = broken + actionable.
type Status int

const (
	OK Status = iota
	Warn
	Fail
)

// Result is one check's outcome with an actionable detail.
type Result struct {
	Name   string
	Status Status
	Detail string
}

// Check runs the health checks. st may be nil (not initialized); prov may be nil
// (web check skipped). root is the resolved project root.
func Check(ctx context.Context, st store.Store, root string, prov provider.ModelProvider) []Result {
	var out []Result
	add := func(name string, s Status, detail string) { out = append(out, Result{name, s, detail}) }

	// 1. Initialized — the state db exists.
	db := filepath.Join(root, config.DirName, "state.db")
	if _, err := os.Stat(db); err != nil {
		add("initialized", Fail, "no .memcode/state.db — run `memcode init`")
	} else {
		add("initialized", OK, root)
	}

	// 2. .memcode self-ignored — the one that bit us repeatedly. Trust git, not the
	// file's bytes: does git actually ignore .memcode?
	if inGitRepo(ctx, root) {
		// Probe a path INSIDE .memcode, not the bare dir. The self-ignore
		// (.memcode/.gitignore = `*`) ignores the dir's CONTENTS — which is what hides
		// it from `git status` — but `git check-ignore .memcode` (the dir name) does
		// NOT match it. Checking a content path is correct for both the self-ignore and
		// a root .gitignore entry.
		if gitIgnores(ctx, root, filepath.Join(config.DirName, "state.db")) {
			add(".memcode self-ignored", OK, "git ignores .memcode/")
		} else {
			add(".memcode self-ignored", Fail, "git shows .memcode/ — relaunch memcode (writes .memcode/.gitignore on startup)")
		}
	} else {
		add(".memcode self-ignored", Warn, "not a git repo — nothing to ignore")
	}

	// 3. Backend connection — hosted (cli → gateway → llms) or a custom
	// endpoint (any OpenAI-compat base, no memcode account). The endpoint
	// truth comes from the CONSTRUCTED provider's Endpointer seam, so a
	// config-listed endpoint (not just env) reports correctly — doctor must
	// describe the backend the app actually dials. The gateway URL defaults
	// to production: an unset MEMCODE_API_URL with a valid token is the
	// NORMAL hosted setup, never a failure.
	tokenSrc := provider.APITokenSource()
	provider.LoadDotEnv()
	var ep provider.Endpoint
	var onEndpoint bool
	if e, ok := prov.(provider.Endpointer); ok {
		ep, onEndpoint = e.Endpoint()
	}
	hosted := false
	switch {
	case onEndpoint:
		add("backend", OK, "custom endpoint "+ep.BaseURL+" (no memcode account — gateway features off)")
	case tokenSrc != "":
		hosted = true
		add("gateway", OK, provider.APIURL()+" (token via "+tokenSrc+")")
	default:
		add("gateway", Fail, "not connected — run `memcode login` (advanced: set "+provider.EnvAPIToken+" in "+provider.GlobalEnvPath()+", or a local endpoint via "+provider.EnvEndpointURL+")")
	}

	// 4. Control plane — GET /v1/models is the routing authority: model
	// selection, BYOK steering, and the /model picker all read it, and when
	// it's unreachable the CLI silently degrades to its embedded catalog
	// snapshot. Surface that degradation here instead of letting it hide.
	if hosted {
		cctx, cancel := context.WithTimeout(ctx, 6*time.Second)
		info, err := provider.FetchModels(cctx)
		cancel()
		switch {
		case err != nil:
			add("control plane", Warn, "GET /v1/models failed ("+clip(err.Error())+") — selection degrades to the embedded catalog")
		case len(info.Models) == 0:
			add("control plane", Warn, "GET /v1/models returned no servable models — selection degrades to the embedded catalog")
		default:
			detail := plural(len(info.Models), "servable model")
			if info.CreditsExhausted {
				detail += " · credits exhausted (BYOK lanes only)"
			}
			add("control plane", OK, detail)
		}
	}

	// 5. web_search — available when the provider can do server-side search.
	// A custom endpoint structurally satisfies the interface but has no search
	// backend behind it (onEndpoint, from the same Endpointer probe above).
	switch {
	case prov == nil:
		add("web_search", Warn, "provider not constructed (CLI) — unchecked")
	case onEndpoint:
		add("web_search", Warn, "server-side search is a memcode gateway service — unavailable on a custom endpoint")
	default:
		if _, ok := prov.(provider.WebSearcher); ok {
			add("web_search", OK, "available")
		} else {
			add("web_search", Warn, "current provider can't web-search")
		}
	}

	// 6. Sessionlog writable — episodic memory + $ capture depend on it.
	if dir := filepath.Join(root, config.DirName, "sessions"); writable(dir) {
		add("sessionlog writable", OK, dir)
	} else {
		add("sessionlog writable", Fail, "can't write "+dir+" — check permissions")
	}

	// 7. Index health — agent context comes from the model of the repo. The model
	// auto-refreshes on launch (memcode's own housekeeping), so doctor REPORTS its
	// freshness here rather than telling the user to run anything.
	if st != nil {
		subs, _ := st.ListEntities(ctx, "subsystem")
		evs, _ := st.ListEvents(ctx, store.EventFilter{})
		if len(subs) == 0 {
			add("index", Warn, "0 subsystems — the repo model is empty")
		} else {
			detail := plural(len(subs), "subsystem") + " · " + plural(len(evs), "event")
			if fresh, ok := indexFresh(ctx, st, root); ok && fresh {
				detail += " · fresh"
			} else if ok {
				detail += " · refreshes on next launch"
			}
			add("index", OK, detail)
		}
	}

	// 8. Language servers — LSP is detect-and-connect (nothing bundled), so a missing
	// binary silently degrades code intelligence (post-edit diagnostics, code_nav) to
	// the fallback tools. Surface that HERE, per repo language, with the install
	// one-liner. Warn, never Fail: everything still works via tsc/grep fallbacks.
	// (knowledge.Detect is the wrong detector — it covers web stacks, not languages.)
	bins := lsp.ServerBins()
	for _, lang := range detectLanguages(root) {
		bin, known := bins[lang]
		if !known {
			continue
		}
		if _, err := lookPath(bin); err == nil {
			add("lsp ("+lang+")", OK, bin)
		} else {
			add("lsp ("+lang+")", Warn, bin+" not on PATH — "+installHint[bin]+" (code intelligence degrades to fallback tools)")
		}
	}

	return out
}

// lookPath is exec.LookPath, swappable so tests control binary presence.
var lookPath = exec.LookPath

// installHint is the one-liner that puts each language server on PATH.
var installHint = map[string]string{
	"gopls":                      "go install golang.org/x/tools/gopls@latest",
	"typescript-language-server": "npm install -g typescript-language-server typescript",
	"pyright-langserver":         "npm install -g pyright",
}

// detectLanguages reports the repo's languages by marker files, scanning the root and
// its first-level subdirectories (a monorepo's go.mod often lives one level down —
// this very repo keeps it in cli/). Depth-2 only, skipping dependency/output dirs, so
// the scan stays cheap and read-only. Sorted for stable doctor output.
func detectLanguages(root string) []string {
	found := map[string]bool{}
	scan := func(dir string) {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			found["go"] = true
		}
		for _, m := range []string{"tsconfig.json", "package.json"} {
			if _, err := os.Stat(filepath.Join(dir, m)); err == nil {
				found["typescript"] = true // javascript shares the same server binary
				break
			}
		}
		for _, m := range []string{"pyproject.toml", "requirements.txt", "setup.py"} {
			if _, err := os.Stat(filepath.Join(dir, m)); err == nil {
				found["python"] = true
				break
			}
		}
	}
	scan(root)
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if !e.IsDir() || skipScanDirs[e.Name()] || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		scan(filepath.Join(root, e.Name()))
	}
	langs := make([]string, 0, len(found))
	for l := range found {
		langs = append(langs, l)
	}
	sort.Strings(langs)
	return langs
}

// skipScanDirs are dependency/output trees that must not count as repo languages
// (a node_modules package's go.mod is not YOUR language).
var skipScanDirs = map[string]bool{
	"node_modules": true, "vendor": true, "dist": true, "build": true, "target": true,
}

// Render formats results as a checklist with a one-line summary.
func Render(rs []Result) string {
	fails, warns := 0, 0
	for _, r := range rs {
		switch r.Status {
		case Fail:
			fails++
		case Warn:
			warns++
		}
	}
	// All green: confirm it in one line and nothing more. Paths, endpoints and counts
	// are internals — only worth surfacing when a check actually needs attention.
	if fails == 0 && warns == 0 {
		return "all good"
	}

	// Something's off: show ONLY the checks that need looking at, with their detail
	// (now it's actionable), aligned, plus a one-line summary of the rest.
	glyph := map[Status]string{OK: "✓", Warn: "⚠", Fail: "✗"}
	maxName := 0
	for _, r := range rs {
		if r.Status != OK && len(r.Name) > maxName {
			maxName = len(r.Name)
		}
	}
	var b strings.Builder
	for _, r := range rs {
		if r.Status == OK {
			continue
		}
		b.WriteString(glyph[r.Status] + "  " + r.Name)
		if r.Detail != "" {
			b.WriteString(strings.Repeat(" ", maxName-len(r.Name)) + "   " + r.Detail)
		}
		b.WriteString("\n")
	}
	passed := len(rs) - fails - warns
	if fails > 0 {
		b.WriteString("\n" + plural(fails, "problem") + " to fix")
		if warns > 0 {
			b.WriteString(" · " + plural(warns, "warning"))
		}
		if passed > 0 {
			b.WriteString(" · " + strconv.Itoa(passed) + " ok")
		}
	} else {
		b.WriteString("\nhealthy · " + plural(warns, "warning"))
	}
	return strings.TrimRight(b.String(), "\n")
}

func inGitRepo(ctx context.Context, root string) bool {
	return exec.CommandContext(ctx, "git", "-C", root, "rev-parse", "--git-dir").Run() == nil
}

// indexFresh compares the HEAD the model was last scanned at (the repo/index_head
// State marker that openProject's freshen writes) to the current HEAD. ok=false when
// there's nothing to compare (non-git, or never scanned).
func indexFresh(ctx context.Context, st store.Store, root string) (fresh, ok bool) {
	out, err := exec.CommandContext(ctx, "git", "-C", root, "rev-parse", "HEAD").Output()
	if err != nil {
		return false, false
	}
	cur := strings.TrimSpace(string(out))
	state, found, _ := st.GetState(ctx, "repo", "index_head")
	if !found {
		return false, false
	}
	var last string
	_ = json.Unmarshal(state.Body, &last)
	return last == cur, true
}

// gitIgnores reports whether git actually ignores the path (check-ignore exits 0).
func gitIgnores(ctx context.Context, root, path string) bool {
	return exec.CommandContext(ctx, "git", "-C", root, "check-ignore", "-q", path).Run() == nil
}

// writable reports whether dir is (or could be) written — probing the nearest
// EXISTING ancestor so a diagnostic never creates .memcode on an uninitialized repo.
func writable(dir string) bool {
	probe := dir
	for {
		if _, err := os.Stat(probe); err == nil {
			break
		}
		if parent := filepath.Dir(probe); parent != probe {
			probe = parent
		} else {
			break
		}
	}
	f, err := os.CreateTemp(probe, ".doctor-*")
	if err != nil {
		return false
	}
	name := f.Name()
	f.Close()
	_ = os.Remove(name)
	return true
}

// clip bounds an upstream error for the one-line doctor detail.
func clip(s string) string {
	if len(s) > 90 {
		return s[:90] + "…"
	}
	return s
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return strconv.Itoa(n) + " " + unit + "s"
}
