package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/agent/tools"
	"github.com/memcode-ai/memcode/internal/artifacts"
	"github.com/memcode-ai/memcode/internal/provider"
)

// artifactTool publishes/updates/lists/deletes agent-built HTML pages hosted at
// memcode.ai/code/artifact/<id>. list is read-only and ungated; the mutating
// actions are outward-facing (they create/replace/remove a public unlisted URL)
// and gate like skill{load}: Medium-tier — auto/allow-all run without a prompt
// (publishing an unlisted page ≈ opening a PR), ask mode prompts with a
// remembered "don't ask again" persisted to .memcode/artifact-approvals.
func (s *Session) artifactTool(ctx context.Context, input json.RawMessage) toolResult {
	var in tools.ArtifactInput
	if err := json.Unmarshal(input, &in); err != nil {
		return errResult("artifact: bad input: " + err.Error())
	}
	// allowTool hides the tool when unauthenticated; this is the defensive twin.
	if strings.TrimSpace(os.Getenv(provider.EnvAPIToken)) == "" {
		return errResult("artifacts need a memcode.ai account — run `memcode login` first")
	}
	client, err := artifacts.New()
	if err != nil {
		return errResult(err.Error())
	}

	switch in.Action {
	case "list":
		arts, err := client.List(ctx)
		if err != nil {
			return errResult("artifact list failed: " + err.Error())
		}
		s.toolLine(true, "Artifact", "list", fmt.Sprintf("%d published", len(arts)), false)
		if len(arts) == 0 {
			return textResult("no artifacts published yet — publish one with artifact{action:\"publish\", path:..., title:...}")
		}
		var b strings.Builder
		for _, a := range arts {
			fmt.Fprintf(&b, "- %s %s (updated %s) id=%s\n", a.Title, a.URL, a.UpdatedAt, a.ID)
		}
		return textResult(strings.TrimRight(b.String(), "\n"))

	case "publish", "update":
		// Plan mode and read-only sub-agents research; they don't publish.
		if s.readOnly || s.planCtl.Planning() {
			return errResult("denied: artifact " + in.Action + " isn't available while planning/read-only — list is, publishing waits for execution")
		}
		if in.Path == "" {
			return errResult("artifact " + in.Action + " needs `path` — build the HTML file with edit_file first")
		}
		if in.Action == "publish" && strings.TrimSpace(in.Title) == "" {
			return errResult("artifact publish needs a `title`")
		}
		if in.Action == "update" && in.ID == "" {
			return errResult("artifact update needs the `id` from the original publish")
		}
		abs, err := safeJoin(s.root, in.Path)
		if err != nil {
			return errResult("artifact: path escapes the workspace: " + in.Path)
		}
		raw, err := os.ReadFile(abs)
		if err != nil {
			return errResult("artifact: can't read " + in.Path + ": " + err.Error())
		}
		if len(raw) == 0 {
			return errResult("artifact: " + in.Path + " is empty")
		}
		if len(raw) > artifacts.MaxHTMLBytes {
			return errResult(fmt.Sprintf("artifact: %s is %s — over the 1.5MB limit; slim it down (strip unused CSS/JS, compress data: images)", in.Path, humanBytes(len(raw))))
		}
		verb := "Publish artifact"
		if in.Action == "update" {
			verb = "Update published artifact"
		}
		if denied := s.gateArtifact(ctx, verb, in.Title, in.Path, len(raw)); denied != nil {
			return *denied
		}
		var art artifacts.Artifact
		if in.Action == "publish" {
			art, err = client.Publish(ctx, in.Title, string(raw))
		} else {
			art, err = client.Update(ctx, in.ID, in.Title, string(raw))
		}
		if err != nil {
			return errResult("artifact " + in.Action + " failed: " + err.Error())
		}
		label := in.Title
		if label == "" {
			label = art.ID
		}
		s.toolLine(true, "Artifact", label, art.URL, false)
		if in.Action == "publish" {
			return textResult(fmt.Sprintf("published %q → %s\n(id: %s — replace the content in place, same URL, with artifact{action:\"update\", id:%q, path:...})", in.Title, art.URL, art.ID, art.ID))
		}
		return textResult(fmt.Sprintf("updated %s in place → %s", art.ID, art.URL))

	case "delete":
		if s.readOnly || s.planCtl.Planning() {
			return errResult("denied: artifact delete isn't available while planning/read-only")
		}
		if in.ID == "" {
			return errResult("artifact delete needs `id` — find it with artifact{action:\"list\"}")
		}
		if denied := s.gateArtifact(ctx, "Delete published artifact", in.ID, "", 0); denied != nil {
			return *denied
		}
		if err := client.Delete(ctx, in.ID); err != nil {
			return errResult("artifact delete failed: " + err.Error())
		}
		s.toolLine(true, "Artifact", in.ID, "deleted", false)
		return textResult("deleted artifact " + in.ID + " — its URL now returns 404")

	default:
		return errResult("artifact: unknown action " + in.Action + " (publish | update | list | delete)")
	}
}

// gateArtifact runs the approval gate for a mutating artifact action. Returns nil
// when allowed; a pointer to the denial result otherwise. Medium tier: auto and
// allow-all modes run without a prompt; ask mode prompts, with "don't ask again"
// remembered per-repo (all artifact publishing, not per-artifact — one decision).
func (s *Session) gateArtifact(ctx context.Context, verb, title, path string, size int) *toolResult {
	if s.approvedArtifacts {
		return nil
	}
	if permissions.Decide(s.effectiveMode(), permissions.Medium, false) == permissions.Allow {
		return nil
	}
	detail := "creates/changes a public unlisted page at memcode.ai"
	if path != "" {
		detail = fmt.Sprintf("uploads %s (%s) to a public unlisted page at memcode.ai", path, humanBytes(size))
	}
	d := s.askApproval(ctx, ApprovalRequest{
		Title:    title,
		Label:    verb,
		Detail:   detail,
		Editable: true,
	})
	if d.Interrupt {
		r := errResult("artifact action interrupted — stopped at your request.")
		return &r
	}
	if !d.Allow {
		r := errResult("artifact action declined by the user — leave the page as is or adjust and ask again.")
		return &r
	}
	if d.Remember {
		s.rememberArtifactApproval()
	}
	return nil
}

// artifactApprovalsPath remembers the repo-scoped "don't ask again" for artifact
// publishing — same shape as skill-approvals (one marker line).
func artifactApprovalsPath(root string) string {
	return filepath.Join(root, ".memcode", "artifact-approvals")
}

// loadArtifactApproval reports whether this repo already approved artifact publishing.
func loadArtifactApproval(root string) bool {
	b, err := os.ReadFile(artifactApprovalsPath(root))
	return err == nil && strings.Contains(string(b), "publish")
}

// rememberArtifactApproval records the don't-ask-again in memory AND on disk.
// Best-effort persistence — an unwritable file just means re-asking next session.
func (s *Session) rememberArtifactApproval() {
	if s.approvedArtifacts {
		return
	}
	s.approvedArtifacts = true
	path := artifactApprovalsPath(s.root)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	_ = os.WriteFile(path, []byte("publish\n"), 0o644)
}

// humanBytes renders a byte count compactly (12.3KB / 1.2MB).
func humanBytes(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1fMB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1fKB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%dB", n)
	}
}
