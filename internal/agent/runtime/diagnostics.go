package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/memcode-ai/memcode/internal/agent/tools"
	"github.com/memcode-ai/memcode/internal/lsp"
)

// diagnosticsTool returns type/compile diagnostics for a file or the repo — the "see the
// errors" capability memcode lacked (the agent otherwise guesses from source or re-runs a
// full build). This is the diagnostics SLICE the review scoped: one-shot checkers per
// language (Go: `gopls check`, falling back to `go build`; TypeScript: `tsc --noEmit`),
// not a persistent LSP server — so there's no lifecycle to manage or leak. A follow-on
// could add a resident LSP for hover/defs/references.
func (s *Session) diagnosticsTool(ctx context.Context, input json.RawMessage) toolResult {
	var in tools.DiagnosticsInput
	if err := json.Unmarshal(input, &in); err != nil {
		return errResult(err.Error())
	}
	cctx, cancel := context.WithTimeout(ctx, bashTimeout)
	defer cancel()

	// Prefer the RESIDENT language server for a specific file when its server is on PATH:
	// incremental (fast), and for TS/Python it's the only real static type-error source.
	// Falls through to the one-shot checkers when no server is available (or for a dir/repo
	// scope, where the whole-project checkers are the right call).
	if in.Path != "" {
		if lang, ok := s.lsp().Supported(in.Path); ok {
			if diags, served, err := s.lsp().Diagnostics(cctx, in.Path); served && err == nil {
				return s.lspDiagResult(lang, in.Path, diags)
			}
		}
	}

	switch diagnosticsLang(s.root, in.Path) {
	case "go":
		return s.goDiagnostics(cctx, in.Path)
	case "ts":
		return s.tsDiagnostics(cctx, in.Path)
	default:
		return errResult("diagnostics: couldn't detect a supported language (Go or TypeScript) for that path. Supported: go.mod repos and tsconfig.json projects.")
	}
}

// codeNavTool answers a semantic query (definition/references/hover) via the resident
// language server — the "read the real code graph" capability grep can't give. Reports an
// install hint when the language's server isn't on PATH.
func (s *Session) codeNavTool(ctx context.Context, input json.RawMessage) toolResult {
	var in tools.CodeNavInput
	if err := json.Unmarshal(input, &in); err != nil {
		return errResult(err.Error())
	}
	if in.Path == "" || in.Line <= 0 || in.Col <= 0 {
		return errResult("code_nav needs `path`, `line`, and `col` (1-based).")
	}
	m := s.lsp()
	lang, ok := m.Supported(in.Path)
	if !ok {
		if hint := m.InstallHint(in.Path); hint != "" {
			return errResult("code_nav: the " + lang + " language server (" + hint + ") isn't on PATH — install it to enable semantic navigation.")
		}
		return errResult("code_nav: no language server configured for that file type.")
	}
	cctx, cancel := context.WithTimeout(ctx, bashTimeout)
	defer cancel()

	switch in.Action {
	case "definition":
		locs, _, err := m.Definition(cctx, in.Path, in.Line, in.Col)
		if err != nil {
			return errResult("code_nav definition: " + err.Error())
		}
		s.toolLine(true, "CodeNav", "definition", "", false)
		return textResult(orNone(m.FormatLocations(locs), "no definition found"))
	case "references":
		locs, _, err := m.References(cctx, in.Path, in.Line, in.Col)
		if err != nil {
			return errResult("code_nav references: " + err.Error())
		}
		s.toolLine(true, "CodeNav", "references", strconv.Itoa(len(locs)), false)
		// Cap the list so a hot symbol (hundreds of refs) can't flood context; the count is
		// still reported, and `impact` is the tool for understanding a large reference set.
		note := ""
		if len(locs) > maxCodeNavRefs {
			note = fmt.Sprintf("\n… %d more (showing %d of %d — use action:impact for the call graph)", len(locs)-maxCodeNavRefs, maxCodeNavRefs, len(locs))
			locs = locs[:maxCodeNavRefs]
		}
		return textResult(orNone(m.FormatLocations(locs)+note, "no references found"))
	case "hover":
		h, _, err := m.Hover(cctx, in.Path, in.Line, in.Col)
		if err != nil {
			return errResult("code_nav hover: " + err.Error())
		}
		s.toolLine(true, "CodeNav", "hover", "", false)
		return textResult(orNone(strings.TrimSpace(h), "no hover info"))
	case "impact":
		return s.impact(cctx, m, in.Path, in.Line, in.Col, in.Depth)
	default:
		return errResult("code_nav action must be definition, references, hover, or impact.")
	}
}

// editDiagnostics re-checks a just-edited file through the resident language server and
// returns a compact block of any ERRORS it introduced (empty when clean, unsupported, or
// only warnings) — appended to the edit tool's result so the model fixes breakage in the
// same turn, the way Claude Code surfaces post-edit diagnostics. Best-effort: never blocks
// or fails the edit.
func (s *Session) editDiagnostics(ctx context.Context, path string) string {
	if _, ok := s.lsp().Supported(path); !ok {
		return ""
	}
	dctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	diags, served, err := s.lsp().DiagnoseAfterEdit(dctx, path)
	if !served || err != nil {
		return ""
	}
	var b strings.Builder
	for _, d := range diags {
		if d.Severity != 1 { // errors only — warnings/hints are noise on an edit result
			continue
		}
		fmt.Fprintf(&b, "%s:%d:%d %s\n", path, d.Range.Start.Line+1, d.Range.Start.Character+1, d.Message)
	}
	if b.Len() == 0 {
		return ""
	}
	return "\n\nLanguage-server errors introduced by this edit (fix them before finishing):\n" + strings.TrimRight(b.String(), "\n")
}

func orNone(s, none string) string {
	if strings.TrimSpace(s) == "" {
		return none
	}
	return s
}

// lspDiagResult formats resident-LSP diagnostics for a file as file:line:col severity:
// message lines — no diagnostics is the clean signal.
func (s *Session) lspDiagResult(lang, path string, diags []lsp.Diagnostic) toolResult {
	if len(diags) == 0 {
		s.toolLine(true, "Diagnostics", lang+" (lsp)", "clean", false)
		return textResult("No diagnostics — " + path + " checks clean (" + lang + " language server).")
	}
	var b strings.Builder
	for _, d := range diags {
		fmt.Fprintf(&b, "%s:%d:%d %s: %s\n", path, d.Range.Start.Line+1, d.Range.Start.Character+1, d.SeverityLabel(), d.Message)
	}
	out := strings.TrimRight(b.String(), "\n")
	s.toolLine(true, "Diagnostics", lang+" (lsp)", strconv.Itoa(len(diags))+" issue(s)", true)
	s.toolResult(linesPreview(out, maxDiagPreviewLines)) // show WHAT the issues are, not just a red count
	return textResult(s.redactor.Redact(truncate(out, maxToolOutput)))
}

// diagnosticsLang picks the language from the path extension, else the repo markers.
func diagnosticsLang(root, path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".go":
		return "go"
	case ".ts", ".tsx":
		return "ts"
	}
	if exists(filepath.Join(root, "go.mod")) {
		return "go"
	}
	if exists(filepath.Join(root, "tsconfig.json")) {
		return "ts"
	}
	return ""
}

// goDiagnostics runs `gopls check` when available (fast, file-scoped diagnostics), else
// `go build` over the package (compile errors). Returns a clean report or the diagnostics.
func (s *Session) goDiagnostics(ctx context.Context, path string) toolResult {
	if _, err := exec.LookPath("gopls"); err == nil && path != "" && strings.HasSuffix(path, ".go") {
		cmd := exec.CommandContext(ctx, "gopls", "check", path)
		cmd.Dir = s.root
		out, _ := cmd.CombinedOutput()
		return s.diagResult("go", strings.TrimSpace(string(out)))
	}
	// Fallback (no gopls, or a dir/whole-repo scope): `go build` surfaces compile errors.
	pkg := "./..."
	if path != "" {
		pkg = path
		if strings.HasSuffix(path, ".go") { // build the file's PACKAGE, not the bare file
			pkg = "./" + filepath.Dir(path)
		}
	}
	cmd := exec.CommandContext(ctx, "go", "build", "-o", os.DevNull, pkg)
	cmd.Dir = s.root
	out, _ := cmd.CombinedOutput()
	return s.diagResult("go", strings.TrimSpace(string(out)))
}

// tsDiagnostics runs `tsc --noEmit` (type errors without emitting) via a local or npx tsc.
func (s *Session) tsDiagnostics(ctx context.Context, path string) toolResult {
	bin, args := "tsc", []string{"--noEmit", "--pretty", "false"}
	if _, err := exec.LookPath("tsc"); err != nil {
		if _, err := exec.LookPath("npx"); err != nil {
			return errResult("diagnostics: neither tsc nor npx is on PATH — install TypeScript to get type diagnostics.")
		}
		bin, args = "npx", append([]string{"tsc"}, args...)
	}
	// tsc checks the whole project (tsconfig); a single-file check needs no project, so pass
	// the file directly when scoped to one.
	if strings.HasSuffix(path, ".ts") || strings.HasSuffix(path, ".tsx") {
		args = append(args, path)
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = s.root
	out, _ := cmd.CombinedOutput()
	return s.diagResult("ts", strings.TrimSpace(string(out)))
}

// maxDiagPreviewLines caps the ⎿ preview shown under a Diagnostics marker — generous
// relative to Bash's 4 (a diagnostics dump is usually several DISTINCT errors, each worth
// seeing at a glance), still bounded so a huge dump doesn't flood scrollback.
const maxDiagPreviewLines = 8

// diagResult formats a checker's output: a clean pass when empty, else the diagnostics
// (truncated/redacted). No diagnostics is the SUCCESS signal. The marker line alone (a red
// "N line(s)") told the user THAT something failed but never WHAT — toolResult below prints
// an actual preview, same as a failed Bash command gets.
func (s *Session) diagResult(lang, out string) toolResult {
	if out == "" {
		s.toolLine(true, "Diagnostics", lang, "clean", false)
		return textResult("No diagnostics — " + lang + " checks clean.")
	}
	n := strings.Count(out, "\n") + 1
	s.toolLine(true, "Diagnostics", lang, strconv.Itoa(n)+" line(s)", true)
	s.toolResult(linesPreview(out, maxDiagPreviewLines))
	return textResult(s.redactor.Redact(truncate(out, maxToolOutput)))
}
