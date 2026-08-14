package cmd

import (
	"encoding/json"
	"os"

	"github.com/memcode-ai/memcode/internal/agent/input"
	"github.com/memcode-ai/memcode/internal/agent/runtime"
	"github.com/memcode-ai/memcode/internal/channels"
	gwconfig "github.com/memcode-ai/memcode/internal/gateway/config"
)

// jobContext mirrors the envelope the gateway persists at gwconfig.ContextPath —
// the persona's supplemental context, its extra skill roots, and this task's
// media as spool IDs. The JSON shape is the contract with internal/gateway/server.
type jobContext struct {
	Items      []runtime.ContextItem `json:"items,omitempty"`
	SkillRoots []string              `json:"skill_roots,omitempty"`
	// Attachments are media spool IDs — bare <sha256>.<ext> filenames, resolved
	// STRICTLY inside the gateway media spool (see resolveJobAttachments). Never
	// paths: the spool is the trust boundary, so a corrupted context file cannot
	// point this job at arbitrary local files.
	Attachments []string `json:"attachments,omitempty"`
}

// resolveJobAttachments turns spool IDs into engine attachments. Each ID must
// resolve inside the media spool; anything else — separators, dot-files, a
// missing file, an unsupported kind — is skipped. Audio never reaches the
// engine (the gateway transcribes it before spawning the job).
func resolveJobAttachments(ids []string) []input.Attachment {
	if len(ids) == 0 {
		return nil
	}
	spool, err := gwconfig.MediaDir()
	if err != nil {
		return nil
	}
	var out []input.Attachment
	for _, id := range ids {
		path, err := channels.ResolveSpoolID(spool, id)
		if err != nil {
			continue
		}
		att, ok := input.Resolve(path, spool, "channel")
		if !ok {
			continue
		}
		switch att.Kind {
		case input.KindImage, input.KindPDF, input.KindText:
			out = append(out, att)
		}
	}
	return out
}

// loadJobContext reads the job context the gateway persisted for this session
// (persona context and skill roots composed above the engine). Returns a zero
// envelope when there is none — which is always the case for the interactive
// CLI, since only the gateway sets --session and writes this file, so the
// engine runs unchanged by default. A file in the pre-envelope shape (a bare
// ContextItem array from an older gateway) is still understood.
func loadJobContext(session string) jobContext {
	path, err := gwconfig.ContextPath(session)
	if err != nil {
		return jobContext{}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return jobContext{}
	}
	var jc jobContext
	if json.Unmarshal(data, &jc) == nil {
		return jc
	}
	var items []runtime.ContextItem
	if json.Unmarshal(data, &items) == nil {
		return jobContext{Items: items}
	}
	return jobContext{}
}
