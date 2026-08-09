package lsp

import (
	"context"
	"encoding/json"
	"net/url"
	"path/filepath"
	"strings"
	"time"
)

// Position is a zero-based line/character in a document (the LSP convention).
type Position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

// Range is a span in a document.
type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

// Location is a range within a document (uri) — the shape definition/references return.
type Location struct {
	URI   string `json:"uri"`
	Range Range  `json:"range"`
}

// Diagnostic is one problem the server reports for a document.
type Diagnostic struct {
	Range    Range  `json:"range"`
	Severity int    `json:"severity"` // 1 error, 2 warning, 3 info, 4 hint
	Code     any    `json:"code"`
	Source   string `json:"source"`
	Message  string `json:"message"`
}

// SeverityLabel renders the numeric severity as text.
func (d Diagnostic) SeverityLabel() string {
	switch d.Severity {
	case 1:
		return "error"
	case 2:
		return "warning"
	case 3:
		return "info"
	case 4:
		return "hint"
	default:
		return "error" // servers sometimes omit severity on errors
	}
}

// Initialize runs the LSP handshake with the workspace root, then sends `initialized`.
// The declared client capabilities are the minimal set our operations use.
func (c *Client) Initialize(ctx context.Context, root string) error {
	params := map[string]any{
		"processId": nil,
		"rootUri":   PathToURI(root),
		"capabilities": map[string]any{
			"textDocument": map[string]any{
				"publishDiagnostics": map[string]any{},
				"definition":         map[string]any{},
				"references":         map[string]any{},
				"hover":              map[string]any{"contentFormat": []string{"plaintext", "markdown"}},
				"documentSymbol":     map[string]any{"hierarchicalDocumentSymbolSupport": true},
				"synchronization":    map[string]any{"didSave": true},
			},
		},
	}
	if _, err := c.call(ctx, "initialize", params); err != nil {
		return err
	}
	return c.notify("initialized", map[string]any{})
}

// DidOpen tells the server about a document's content — required before it will produce
// diagnostics or answer position queries for that file.
func (c *Client) DidOpen(uri, languageID, text string) error {
	return c.notify("textDocument/didOpen", map[string]any{
		"textDocument": map[string]any{
			"uri": uri, "languageId": languageID, "version": 1, "text": text,
		},
	})
}

// DidChange tells the server a document's full content changed (full-sync) — sent after
// an edit so its diagnostics reflect the new text. version must increase per document.
func (c *Client) DidChange(uri string, version int, text string) error {
	return c.notify("textDocument/didChange", map[string]any{
		"textDocument":   map[string]any{"uri": uri, "version": version},
		"contentChanges": []map[string]any{{"text": text}},
	})
}

// invalidate drops the cached diagnostics for uri so a following WaitDiagnostics blocks
// for the server's NEXT publish (after a didChange) rather than returning stale results.
func (c *Client) invalidate(uri string) {
	c.diagMu.Lock()
	delete(c.diags, uri)
	c.diagMu.Unlock()
}

// Definition returns where the symbol at pos is defined. Handles the server returning a
// single Location or an array (both are valid per the spec).
func (c *Client) Definition(ctx context.Context, uri string, pos Position) ([]Location, error) {
	raw, err := c.call(ctx, "textDocument/definition", posParams(uri, pos))
	if err != nil {
		return nil, err
	}
	return decodeLocations(raw), nil
}

// References returns every use of the symbol at pos (including its declaration).
func (c *Client) References(ctx context.Context, uri string, pos Position) ([]Location, error) {
	params := posParams(uri, pos)
	params["context"] = map[string]any{"includeDeclaration": true}
	raw, err := c.call(ctx, "textDocument/references", params)
	if err != nil {
		return nil, err
	}
	return decodeLocations(raw), nil
}

// Hover returns the type/signature/doc for the symbol at pos, flattened to text.
func (c *Client) Hover(ctx context.Context, uri string, pos Position) (string, error) {
	raw, err := c.call(ctx, "textDocument/hover", posParams(uri, pos))
	if err != nil {
		return "", err
	}
	var h struct {
		Contents json.RawMessage `json:"contents"`
	}
	if json.Unmarshal(raw, &h) != nil || len(h.Contents) == 0 {
		return "", nil
	}
	return flattenHover(h.Contents), nil
}

// DocumentSymbol is a named symbol in a file (LSP documentSymbol). Range is the full span
// (declaration + body); SelectionRange is the name token. Children nest (a method inside a
// type). Used to attribute a reference to the function/method that contains it.
type DocumentSymbol struct {
	Name           string           `json:"name"`
	Kind           int              `json:"kind"`
	Range          Range            `json:"range"`
	SelectionRange Range            `json:"selectionRange"`
	Children       []DocumentSymbol `json:"children,omitempty"`
}

// DocumentSymbol returns the file's symbol tree. Handles both the hierarchical
// DocumentSymbol[] result (gopls/tsserver/pyright) and the flat SymbolInformation[] fallback.
func (c *Client) DocumentSymbol(ctx context.Context, uri string) ([]DocumentSymbol, error) {
	raw, err := c.call(ctx, "textDocument/documentSymbol", map[string]any{
		"textDocument": map[string]any{"uri": uri},
	})
	if err != nil {
		return nil, err
	}
	return decodeSymbols(raw), nil
}

// decodeSymbols accepts either DocumentSymbol[] (hierarchical, has selectionRange) or the
// legacy SymbolInformation[] (flat, has a location) — distinguished by the "location" key on
// the first element — and normalizes both to a DocumentSymbol slice.
func decodeSymbols(raw json.RawMessage) []DocumentSymbol {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var probe []map[string]json.RawMessage
	if json.Unmarshal(raw, &probe) != nil || len(probe) == 0 {
		return nil
	}
	if _, flat := probe[0]["location"]; flat {
		var infos []struct {
			Name     string   `json:"name"`
			Kind     int      `json:"kind"`
			Location Location `json:"location"`
		}
		if json.Unmarshal(raw, &infos) != nil {
			return nil
		}
		out := make([]DocumentSymbol, 0, len(infos))
		for _, s := range infos {
			out = append(out, DocumentSymbol{Name: s.Name, Kind: s.Kind, Range: s.Location.Range, SelectionRange: s.Location.Range})
		}
		return out
	}
	var hier []DocumentSymbol
	if json.Unmarshal(raw, &hier) != nil {
		return nil
	}
	return hier
}

// Diagnostics returns the latest diagnostics the server pushed for uri. Callers that just
// opened/changed a file should give the server a moment to publish (see WaitDiagnostics).
func (c *Client) Diagnostics(uri string) []Diagnostic {
	c.diagMu.Lock()
	defer c.diagMu.Unlock()
	return append([]Diagnostic(nil), c.diags[uri]...)
}

// WaitDiagnostics polls for diagnostics on uri up to timeout — diagnostics arrive as an
// async push after didOpen, so a caller that needs them right after opening waits briefly.
// Returns whatever is present at the deadline (empty means "clean" once the server settled).
func (c *Client) WaitDiagnostics(ctx context.Context, uri string, timeout time.Duration) []Diagnostic {
	deadline := time.Now().Add(timeout)
	for {
		c.diagMu.Lock()
		d, seen := c.diags[uri]
		c.diagMu.Unlock()
		if seen {
			return append([]Diagnostic(nil), d...)
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return nil
		}
		select {
		case <-time.After(40 * time.Millisecond):
		case <-ctx.Done():
			return nil
		}
	}
}

func posParams(uri string, pos Position) map[string]any {
	return map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     map[string]any{"line": pos.Line, "character": pos.Character},
	}
}

// decodeLocations accepts the definition/references result as either a single Location or
// an array (both valid), returning a slice.
func decodeLocations(raw json.RawMessage) []Location {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var many []Location
	if json.Unmarshal(raw, &many) == nil {
		return many
	}
	var one Location
	if json.Unmarshal(raw, &one) == nil {
		return []Location{one}
	}
	return nil
}

// flattenHover renders LSP Hover contents (a MarkupContent, a MarkedString, or an array
// of them) to plain text.
func flattenHover(raw json.RawMessage) string {
	// MarkupContent: {kind, value}
	var mc struct {
		Value string `json:"value"`
	}
	if json.Unmarshal(raw, &mc) == nil && mc.Value != "" {
		return strings.TrimSpace(mc.Value)
	}
	// A bare string.
	var s string
	if json.Unmarshal(raw, &s) == nil && s != "" {
		return strings.TrimSpace(s)
	}
	// An array of MarkedString ({language,value}) or strings.
	var arr []json.RawMessage
	if json.Unmarshal(raw, &arr) == nil {
		var parts []string
		for _, el := range arr {
			var ms struct {
				Value string `json:"value"`
			}
			if json.Unmarshal(el, &ms) == nil && ms.Value != "" {
				parts = append(parts, ms.Value)
				continue
			}
			var es string
			if json.Unmarshal(el, &es) == nil && es != "" {
				parts = append(parts, es)
			}
		}
		return strings.TrimSpace(strings.Join(parts, "\n"))
	}
	return ""
}

// PathToURI converts an absolute filesystem path to a file:// URI.
func PathToURI(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	u := &url.URL{Scheme: "file", Path: filepath.ToSlash(abs)}
	return u.String()
}

// URIToPath converts a file:// URI back to a filesystem path (best-effort).
func URIToPath(uri string) string {
	if u, err := url.Parse(uri); err == nil && u.Scheme == "file" {
		return filepath.FromSlash(u.Path)
	}
	return strings.TrimPrefix(uri, "file://")
}
