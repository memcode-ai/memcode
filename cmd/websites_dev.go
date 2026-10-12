// websites_dev.go — `memcode websites dev` runs a site's REAL
// vite-react-ssg dev server locally, not a preview iframe: full HMR, real
// browser devtools, the site's own code exactly as authored.
//
// The only wrinkle is that the site's build output (draft/) and its durable
// uploaded assets (assets/) are served from two different places in
// production (the builder's dist/, and the sibling assets/ prefix) but need
// to appear at ONE url convention locally too: everything under
// /assets/<name>. Vite only has a single `publicDir`, so this generates an
// untracked MERGED public dir every run — symlinks to the project's own
// public/* entries, plus a symlink named "assets" pointing at a freshly
// downloaded cache of the site's uploaded assets — and points an untracked
// vite.config.js override at it via --config. Nothing here ever touches the
// tracked source tree.
package cmd

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"github.com/memcode-ai/memcode/internal/authflow"
	"github.com/memcode-ai/memcode/internal/config"
	"github.com/memcode-ai/memcode/internal/websites"
)

var websitesDevCmd = &cobra.Command{
	Use:   "dev [slug-or-dir]",
	Short: "Run a website's real Vite dev server locally",
	Long: `Run the site's actual vite-react-ssg dev server on your machine — full
HMR and real devtools, not the preview iframe.

With a slug (` + "`memcode websites dev my-site`" + `): clones into a MANAGED
workspace under ~/.memcode/websites/<site-id> the first time, then reuses it
on later runs. That reuse is NOT automatic refresh — if the remote site has
moved on, pull again yourself (or pass an explicit directory instead).

With a directory (or no argument, inside one you already ` + "`pull`" + `ed): runs
there directly; its .memcode/ stays untracked (gitignored) the same way a
regular pulled checkout's is.

Either way, uploaded assets are synced into an untracked local cache and
served at the same /assets/<name> urls as the live site and preview,
alongside the site's own public/ files.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runWebsitesDev(cmd, args)
	},
}

func init() {
	websitesDevCmd.Flags().Bool("no-open", false, "don't open a browser when the dev server is ready")
	websitesCmd.AddCommand(websitesDevCmd)
}

func runWebsitesDev(cmd *cobra.Command, args []string) error {
	c, err := websites.New()
	if err != nil {
		return err
	}
	ctx := cmd.Context()

	ws, meta, err := resolveDevWorkspace(ctx, c, args)
	if err != nil {
		return err
	}

	if err := ensureNodeModules(ws.SourceDir); err != nil {
		return fmt.Errorf("installing dependencies: %w", err)
	}

	mergedPublicDir, err := syncDevAssets(ctx, c, meta.SiteID, ws)
	if err != nil {
		fmt.Printf("warning: could not sync uploaded assets: %v\n", err)
		mergedPublicDir = "" // fall back to the project's own public/ as-is
	}

	overridePath, err := writeViteDevOverride(ws, mergedPublicDir)
	if err != nil {
		return err
	}

	fmt.Printf("Starting the dev server for %s in %s …\n", meta.Slug, ws.SourceDir)
	noOpen, _ := cmd.Flags().GetBool("no-open")
	return runViteDevServer(ctx, ws.SourceDir, overridePath, !noOpen)
}

// resolveDevWorkspace picks explicit-dir mode (an existing pulled checkout —
// the given arg, or cwd when none) over managed mode (the arg is a slug,
// cloned into ~/.memcode/websites/<site-id> on first use).
func resolveDevWorkspace(ctx context.Context, c *websites.Client, args []string) (websites.Workspace, websites.Meta, error) {
	arg := ""
	if len(args) > 0 {
		arg = args[0]
	}

	dir := arg
	if dir == "" {
		if wd, err := os.Getwd(); err == nil {
			dir = wd
		}
	}
	if info, err := os.Stat(dir); err == nil && info.IsDir() {
		if meta, metaErr := websites.ReadMeta(dir); metaErr == nil {
			abs, err := filepath.Abs(dir)
			if err != nil {
				return websites.Workspace{}, websites.Meta{}, err
			}
			config.EnsureGitignore(abs) // .memcode/{assets,runtime} must stay untracked
			ws, err := websites.ExplicitWorkspace(abs)
			return ws, meta, err
		}
	}

	if arg == "" {
		return websites.Workspace{}, websites.Meta{}, fmt.Errorf(
			"not a pulled website here — pass a slug or a directory: `memcode websites dev <slug>`")
	}

	// A site with a managed working copy (made here with `init`, or pulled
	// earlier) runs from that copy: no network, and one folder per site.
	if local, ok := websites.FindLocal(arg); ok {
		ws, err := local.Workspace()
		return ws, local.Meta, err
	}
	site, err := c.Resolve(ctx, arg)
	if err != nil {
		return websites.Workspace{}, websites.Meta{}, err
	}
	if local, ok := websites.FindLocal(site.ID); ok {
		ws, err := local.Workspace()
		return ws, local.Meta, err
	}
	ws, err := websites.ManagedWorkspace(site.ID)
	if err != nil {
		return websites.Workspace{}, websites.Meta{}, err
	}
	if !ws.SourceExists() {
		fmt.Printf("Cloning %s into %s …\n", site.Slug, ws.SourceDir)
		if _, err := c.Pull(ctx, site.Slug, ws.SourceDir); err != nil {
			return websites.Workspace{}, websites.Meta{}, err
		}
	} else {
		fmt.Printf("Reusing the existing local checkout at %s (not auto-refreshed — pull again manually if the remote site has moved on)\n", ws.SourceDir)
	}
	meta, err := websites.ReadMeta(ws.SourceDir)
	return ws, meta, err
}

// ensureNodeModules installs dependencies on first use — the repo bundle
// never carries node_modules/ (gitignored, like any real checkout).
func ensureNodeModules(sourceDir string) error {
	if info, err := os.Stat(filepath.Join(sourceDir, "node_modules")); err == nil && info.IsDir() {
		return nil
	}
	fmt.Println("Installing dependencies (first run)…")
	args := []string{"install"}
	if _, err := os.Stat(filepath.Join(sourceDir, "package-lock.json")); err == nil {
		args = []string{"ci"}
	}
	npm := exec.Command("npm", args...)
	npm.Dir = sourceDir
	npm.Stdout = os.Stdout
	npm.Stderr = os.Stderr
	npm.Stdin = os.Stdin
	return npm.Run()
}

// syncDevAssets fully refreshes the local asset cache (the set is capped at
// 20MB total, small enough to always re-pull rather than diff) and returns
// the merged public dir `dev` should point Vite at.
func syncDevAssets(ctx context.Context, c *websites.Client, siteID string, ws websites.Workspace) (string, error) {
	assets, err := c.ListAssets(ctx, siteID)
	if err != nil {
		return "", err
	}
	if err := os.RemoveAll(ws.AssetsDir); err != nil {
		return "", err
	}
	if err := os.MkdirAll(ws.AssetsDir, 0o755); err != nil {
		return "", err
	}
	for _, a := range assets {
		if a.URL == "" {
			continue
		}
		// Best-effort: an unpublished site's asset URL isn't live yet, so a
		// fetch here can 404 — the asset still exists, it just won't render
		// locally until the site is published.
		if err := downloadToFile(ctx, a.URL, filepath.Join(ws.AssetsDir, a.Name)); err != nil {
			fmt.Printf("  (skipping asset %s: %v)\n", a.Name, err)
		}
	}
	return mergePublicDir(ws)
}

func downloadToFile(ctx context.Context, url, dest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("%s", resp.Status)
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, resp.Body)
	return err
}

// mergePublicDir rebuilds runtime/public/ from scratch: a symlink per entry
// of the project's own public/ (so edits there show up live, same as any
// other HMR'd file), plus one symlink named "assets" over the just-synced
// asset cache — giving Vite ONE publicDir that serves both.
func mergePublicDir(ws websites.Workspace) (string, error) {
	merged := filepath.Join(ws.RuntimeDir, "public")
	if err := os.RemoveAll(merged); err != nil {
		return "", err
	}
	if err := os.MkdirAll(merged, 0o755); err != nil {
		return "", err
	}

	realPublic := filepath.Join(ws.SourceDir, "public")
	if entries, err := os.ReadDir(realPublic); err == nil {
		for _, e := range entries {
			if e.Name() == "assets" {
				continue // reserved below for uploaded assets
			}
			src, err := filepath.Abs(filepath.Join(realPublic, e.Name()))
			if err != nil {
				return "", err
			}
			if err := os.Symlink(src, filepath.Join(merged, e.Name())); err != nil {
				return "", err
			}
		}
	}

	assetsAbs, err := filepath.Abs(ws.AssetsDir)
	if err != nil {
		return "", err
	}
	if err := os.Symlink(assetsAbs, filepath.Join(merged, "assets")); err != nil {
		return "", err
	}
	return merged, nil
}

// writeViteDevOverride generates runtime/vite.dev.config.mjs: the project's
// own vite.config.js (imported by absolute file:// URL, so ITS bare imports
// like @vitejs/plugin-react still resolve against the project's own
// node_modules) with publicDir swapped for the merged dir above. Deliberately
// a plain object spread, not vite's mergeConfig — that would need to import
// the 'vite' package from runtime/, which (in managed mode) is a SIBLING of
// source/, not an ancestor, so Node's bare-specifier resolution would never
// find source/node_modules/vite.
func writeViteDevOverride(ws websites.Workspace, mergedPublicDir string) (string, error) {
	baseConfig, err := filepath.Abs(filepath.Join(ws.SourceDir, "vite.config.js"))
	if err != nil {
		return "", err
	}
	baseURL, err := toFileURL(baseConfig)
	if err != nil {
		return "", err
	}

	publicDirLine := ""
	if mergedPublicDir != "" {
		publicDirLine = fmt.Sprintf("  publicDir: %s,\n", jsString(mergedPublicDir))
	}

	content := fmt.Sprintf(`// Generated by `+"`memcode websites dev`"+` — untracked, safe to delete.
import base from %s;

export default {
  ...base,
  build: { ...(base.build || {}), copyPublicDir: false },
%s};
`, jsString(baseURL), publicDirLine)

	path := filepath.Join(ws.RuntimeDir, "vite.dev.config.mjs")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

func jsString(s string) string { return fmt.Sprintf("%q", s) }

// toFileURL turns an absolute filesystem path into a `file://` URL — the one
// import specifier shape Node's ESM loader accepts for an arbitrary absolute
// path (a bare "/abs/path" is not a valid module specifier).
func toFileURL(absPath string) (string, error) {
	abs, err := filepath.Abs(absPath)
	if err != nil {
		return "", err
	}
	abs = filepath.ToSlash(abs)
	if !strings.HasPrefix(abs, "/") {
		abs = "/" + abs
	}
	return "file://" + strings.ReplaceAll(abs, " ", "%20"), nil
}

var devServerURLRE = regexp.MustCompile(`https?://[^\s]+`)

// runViteDevServer execs the project's own vite-react-ssg binary directly
// (node_modules/.bin — no npx/network touch) with the generated override,
// streaming its output live and opening a browser the moment its "Local:"
// line reports the dev server is ready.
func runViteDevServer(ctx context.Context, sourceDir, overridePath string, open bool) error {
	bin := filepath.Join(sourceDir, "node_modules", ".bin", "vite-react-ssg")
	if _, err := os.Stat(bin); err != nil {
		return fmt.Errorf("vite-react-ssg not found in node_modules/.bin — try deleting node_modules and re-running")
	}

	run := exec.CommandContext(ctx, bin, "dev", "--config", overridePath)
	run.Dir = sourceDir
	run.Stdin = os.Stdin
	run.Stderr = os.Stderr

	opened := false
	run.Stdout = &urlWatcher{out: os.Stdout, onFound: func(url string) {
		if opened || !open {
			return
		}
		opened = true
		fmt.Printf("\nOpening %s …\n", url)
		if err := authflow.OpenBrowser(url); err != nil {
			fmt.Println("(could not open a browser — use the URL above)")
		}
	}}

	return run.Run()
}

// urlWatcher passes everything through to out unmodified while scanning the
// accumulated tail for Vite's "Local:  http://…" line, firing onFound once.
type urlWatcher struct {
	out     io.Writer
	buf     []byte
	onFound func(string)
}

func (w *urlWatcher) Write(p []byte) (int, error) {
	n, err := w.out.Write(p)
	w.buf = append(w.buf, p...)
	if idx := bytes.Index(w.buf, []byte("Local:")); idx >= 0 {
		if m := devServerURLRE.FindString(string(w.buf[idx:])); m != "" {
			w.onFound(m)
		}
	}
	if len(w.buf) > 4096 {
		w.buf = w.buf[len(w.buf)-4096:]
	}
	return n, err
}
