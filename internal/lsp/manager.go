package lsp

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// serverSpec describes how to launch a language server and the languageId to tag its
// documents with. args is the invocation after the binary.
type serverSpec struct {
	bin        string
	args       []string
	languageID string
	env        []string // extra env (nil in production; tests use it for the in-binary stub)
}

// defaultServers is the registry, following Claude Code's detect-and-connect model: a
// server is used only if its binary is on PATH (nothing bundled or auto-installed).
// Keyed by our internal language name (see langForExt).
func defaultServers() map[string]serverSpec {
	return map[string]serverSpec{
		"go":         {bin: "gopls", languageID: "go"},
		"typescript": {bin: "typescript-language-server", args: []string{"--stdio"}, languageID: "typescript"},
		"javascript": {bin: "typescript-language-server", args: []string{"--stdio"}, languageID: "javascript"},
		"python":     {bin: "pyright-langserver", args: []string{"--stdio"}, languageID: "python"},
	}
}

// ServerBins reports the registry's language → server binary (for doctor's
// missing-server check). javascript shares typescript's binary; callers that
// want one row per binary can dedupe on the value.
func ServerBins() map[string]string {
	out := map[string]string{}
	for lang, spec := range defaultServers() {
		out[lang] = spec.bin
	}
	return out
}

// Manager owns one resident language server per language for a workspace, started lazily
// and reused for the session. It is the session-facing surface (Diagnostics/Definition/
// References/Hover by file path). Nil-safe: a zero Manager degrades to "no LSP".
type Manager struct {
	root    string
	servers map[string]serverSpec

	mu      sync.Mutex
	clients map[string]*Client // language → resident client (nil = tried, unavailable)
	opened  map[string]int     // uri → last document version sent (0 = never opened)
}

// NewManager returns a Manager rooted at the workspace. Servers are started on first use.
func NewManager(root string) *Manager {
	return &Manager{
		root:    root,
		servers: defaultServers(),
		clients: map[string]*Client{},
		opened:  map[string]int{},
	}
}

// Supported reports whether a resident server COULD serve this path (its language is
// known and its binary is on PATH). Used to decide whether to prefer LSP over a one-shot
// checker, and to give an actionable "install X" message otherwise.
func (m *Manager) Supported(path string) (lang string, ok bool) {
	if m == nil {
		return "", false
	}
	lang = langForExt(path)
	spec, known := m.servers[lang]
	if !known {
		return lang, false
	}
	_, err := exec.LookPath(spec.bin)
	return lang, err == nil
}

// InstallHint returns the binary a user must install to enable LSP for path's language.
func (m *Manager) InstallHint(path string) string {
	if m == nil {
		return ""
	}
	if spec, ok := m.servers[langForExt(path)]; ok {
		return spec.bin
	}
	return ""
}

// clientFor lazily starts and initializes the server for a language, caching the result
// (including a nil for "unavailable" so we don't re-probe every call). Returns nil when
// the language is unknown or the binary isn't on PATH.
func (m *Manager) clientFor(ctx context.Context, lang string) *Client {
	m.mu.Lock()
	if c, tried := m.clients[lang]; tried {
		m.mu.Unlock()
		return c
	}
	spec, ok := m.servers[lang]
	m.mu.Unlock()
	if !ok {
		return nil
	}
	if _, err := exec.LookPath(spec.bin); err != nil {
		m.setClient(lang, nil)
		return nil
	}
	// Start with a fresh (background) context so the server outlives one turn's ctx.
	c, err := newClient(context.Background(), m.root, spec.env, spec.bin, spec.args...)
	if err != nil {
		m.setClient(lang, nil)
		return nil
	}
	ictx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := c.Initialize(ictx, m.root); err != nil {
		c.Close()
		m.setClient(lang, nil)
		return nil
	}
	m.setClient(lang, c)
	return c
}

func (m *Manager) setClient(lang string, c *Client) {
	m.mu.Lock()
	m.clients[lang] = c
	m.mu.Unlock()
}

// open reads the file and sends didOpen once per uri, so the server has the content for
// diagnostics/position queries. Re-opening is harmless but we track to avoid re-reads.
func (m *Manager) open(c *Client, lang, path string) (string, error) {
	abs := path
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(m.root, path)
	}
	uri := PathToURI(abs)
	m.mu.Lock()
	_, already := m.opened[uri]
	m.mu.Unlock()
	if already {
		return uri, nil
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return "", err
	}
	if err := c.DidOpen(uri, m.servers[lang].languageID, string(data)); err != nil {
		return "", err
	}
	m.mu.Lock()
	m.opened[uri] = 1
	m.mu.Unlock()
	return uri, nil
}

// FileSymbol is one declaration from a file's LSP symbol tree, flattened for
// consumers that want a per-file symbol list (the repo map) rather than the
// enclosing-position query EnclosingSymbol answers.
type FileSymbol struct {
	Name    string
	Kind    int // LSP SymbolKind
	Line    int // 1-based full-range start
	EndLine int // 1-based full-range end
	SelLine int // 1-based name-token line
	Depth   int // 0 = top level, 1 = member (a class's method), …
}

// FileSymbols returns the file's declarations via textDocument/documentSymbol,
// flattened two levels deep (top-level + members — grandchildren are locals and
// fields, noise for a repo overview). ok=false when no resident server serves
// the file's language.
func (m *Manager) FileSymbols(ctx context.Context, path string) ([]FileSymbol, bool, error) {
	if m == nil {
		return nil, false, nil
	}
	lang := langForExt(path)
	c := m.clientFor(ctx, lang)
	if c == nil {
		return nil, false, nil
	}
	uri, err := m.open(c, lang, path)
	if err != nil {
		return nil, true, err
	}
	syms, err := c.DocumentSymbol(ctx, uri)
	if err != nil {
		return nil, true, err
	}
	var out []FileSymbol
	var walk func(s DocumentSymbol, depth int)
	walk = func(s DocumentSymbol, depth int) {
		out = append(out, FileSymbol{
			Name:    s.Name,
			Kind:    s.Kind,
			Line:    s.Range.Start.Line + 1,
			EndLine: s.Range.End.Line + 1,
			SelLine: s.SelectionRange.Start.Line + 1,
			Depth:   depth,
		})
		if depth >= 1 {
			return
		}
		for _, ch := range s.Children {
			walk(ch, depth+1)
		}
	}
	for _, s := range syms {
		walk(s, 0)
	}
	return out, true, nil
}

// DiagnoseAfterEdit re-syncs a file that was just edited on disk (didChange with the new
// content, or didOpen if the server hasn't seen it yet), then waits for the server's fresh
// diagnostics. This is the "sees the error immediately, fixes it the same turn" loop.
// ok=false when no resident server serves the file.
func (m *Manager) DiagnoseAfterEdit(ctx context.Context, path string) (diags []Diagnostic, ok bool, err error) {
	lang := langForExt(path)
	c := m.clientFor(ctx, lang)
	if c == nil {
		return nil, false, nil
	}
	abs := path
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(m.root, path)
	}
	uri := PathToURI(abs)
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, true, err
	}
	m.mu.Lock()
	ver := m.opened[uri]
	if ver == 0 {
		m.opened[uri] = 1
		m.mu.Unlock()
		if err := c.DidOpen(uri, m.servers[lang].languageID, string(data)); err != nil {
			return nil, true, err
		}
	} else {
		ver++
		m.opened[uri] = ver
		m.mu.Unlock()
		c.invalidate(uri) // block WaitDiagnostics for the NEXT publish, not the stale one
		if err := c.DidChange(uri, ver, string(data)); err != nil {
			return nil, true, err
		}
	}
	return c.WaitDiagnostics(ctx, uri, 3*time.Second), true, nil
}

// Diagnostics returns the resident server's diagnostics for a file (opening it first and
// waiting briefly for the async push). ok=false means no server is available for it.
func (m *Manager) Diagnostics(ctx context.Context, path string) (diags []Diagnostic, ok bool, err error) {
	c := m.clientFor(ctx, langForExt(path))
	if c == nil {
		return nil, false, nil
	}
	uri, err := m.open(c, langForExt(path), path)
	if err != nil {
		return nil, true, err
	}
	return c.WaitDiagnostics(ctx, uri, 3*time.Second), true, nil
}

// Definition/References/Hover resolve the symbol at a 1-based line/col (the human/editor
// convention) — converted to LSP's 0-based position here.
func (m *Manager) Definition(ctx context.Context, path string, line, col int) ([]Location, bool, error) {
	return m.locQuery(ctx, path, line, col, (*Client).Definition)
}

func (m *Manager) References(ctx context.Context, path string, line, col int) ([]Location, bool, error) {
	return m.locQuery(ctx, path, line, col, (*Client).References)
}

func (m *Manager) locQuery(ctx context.Context, path string, line, col int, fn func(*Client, context.Context, string, Position) ([]Location, error)) ([]Location, bool, error) {
	c := m.clientFor(ctx, langForExt(path))
	if c == nil {
		return nil, false, nil
	}
	uri, err := m.open(c, langForExt(path), path)
	if err != nil {
		return nil, true, err
	}
	locs, err := fn(c, ctx, uri, Position{Line: line - 1, Character: col - 1})
	return locs, true, err
}

// Hover returns the type/signature/doc at a 1-based line/col.
func (m *Manager) Hover(ctx context.Context, path string, line, col int) (string, bool, error) {
	c := m.clientFor(ctx, langForExt(path))
	if c == nil {
		return "", false, nil
	}
	uri, err := m.open(c, langForExt(path), path)
	if err != nil {
		return "", true, err
	}
	h, err := c.Hover(ctx, uri, Position{Line: line - 1, Character: col - 1})
	return h, true, err
}

// EnclosingSymbol is the named symbol whose body contains a position — used to attribute a
// reference to the function/method that contains it, for call-graph / impact analysis.
type EnclosingSymbol struct {
	Name    string
	Kind    int
	Path    string // absolute file path (as queried)
	DefLine int    // 1-based line of the symbol name (its selectionRange start)
	DefCol  int    // 1-based column
}

// EnclosingSymbol resolves the most-specific symbol containing a 1-based line/col. ok=false
// when no resident server serves the file or nothing encloses the position.
func (m *Manager) EnclosingSymbol(ctx context.Context, path string, line, col int) (EnclosingSymbol, bool, error) {
	c := m.clientFor(ctx, langForExt(path))
	if c == nil {
		return EnclosingSymbol{}, false, nil
	}
	uri, err := m.open(c, langForExt(path), path)
	if err != nil {
		return EnclosingSymbol{}, true, err
	}
	syms, err := c.DocumentSymbol(ctx, uri)
	if err != nil {
		return EnclosingSymbol{}, true, err
	}
	sym, ok := deepestEnclosing(syms, Position{Line: line - 1, Character: col - 1})
	if !ok {
		return EnclosingSymbol{}, true, nil
	}
	return EnclosingSymbol{
		Name:    sym.Name,
		Kind:    sym.Kind,
		Path:    path,
		DefLine: sym.SelectionRange.Start.Line + 1,
		DefCol:  sym.SelectionRange.Start.Character + 1,
	}, true, nil
}

// deepestEnclosing returns the tightest symbol whose Range contains pos (a reference inside a
// method body attributes to the method, not the enclosing type). Pure — unit-tested without a
// live server.
func deepestEnclosing(syms []DocumentSymbol, pos Position) (DocumentSymbol, bool) {
	var best DocumentSymbol
	found := false
	var walk func(s DocumentSymbol)
	walk = func(s DocumentSymbol) {
		if !rangeContains(s.Range, pos) {
			return
		}
		if !found || rangeTighter(s.Range, best.Range) {
			best, found = s, true
		}
		for _, ch := range s.Children {
			walk(ch)
		}
	}
	for _, s := range syms {
		walk(s)
	}
	return best, found
}

// rangeContains reports whether pos falls within r (inclusive), comparing line then column.
func rangeContains(r Range, pos Position) bool {
	if pos.Line < r.Start.Line || pos.Line > r.End.Line {
		return false
	}
	if pos.Line == r.Start.Line && pos.Character < r.Start.Character {
		return false
	}
	if pos.Line == r.End.Line && pos.Character > r.End.Character {
		return false
	}
	return true
}

// rangeTighter reports whether a spans fewer lines (then fewer columns) than b.
func rangeTighter(a, b Range) bool {
	if al, bl := a.End.Line-a.Start.Line, b.End.Line-b.Start.Line; al != bl {
		return al < bl
	}
	return (a.End.Character - a.Start.Character) < (b.End.Character - b.Start.Character)
}

// Close shuts every resident server down (call on session end).
func (m *Manager) Close() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.clients {
		if c != nil {
			c.Close()
		}
	}
	m.clients = map[string]*Client{}
}

// langForExt maps a file extension to our internal language key.
func langForExt(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".go":
		return "go"
	case ".ts", ".tsx", ".mts", ".cts":
		return "typescript"
	case ".js", ".jsx", ".mjs", ".cjs":
		return "javascript"
	case ".py", ".pyi":
		return "python"
	}
	return ""
}

// FormatLocations renders locations as repo-relative file:line references for the model.
func (m *Manager) FormatLocations(locs []Location) string {
	var b strings.Builder
	for _, l := range locs {
		p := URIToPath(l.URI)
		if rel, err := filepath.Rel(m.root, p); err == nil && !strings.HasPrefix(rel, "..") {
			p = rel
		}
		fmt.Fprintf(&b, "%s:%d:%d\n", p, l.Range.Start.Line+1, l.Range.Start.Character+1)
	}
	return strings.TrimRight(b.String(), "\n")
}
