package runtime

// Admin-session tool surface. The engine deliberately does not import the
// gateway layer (TestEngineDoesNotImportGateway): the actual operations over
// gateway config and state are injected by cmd as an AdminExecutor. The
// runtime's share of the work is the permission gate — every mutating admin
// call is approved by the user before the executor runs.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/agent/tools"
)

// AdminExecutor performs one admin tool operation (cmd-injected; see
// cmd/admin_tools.go). It is only invoked after the runtime's gate approves a
// mutation; read-only calls run directly.
type AdminExecutor func(ctx context.Context, name string, input json.RawMessage) (string, error)

// adminReadOnly reports whether an admin call needs no approval: pure reads.
func adminReadOnly(name string, input json.RawMessage) bool {
	if name == tools.GwOverview {
		return true
	}
	if name == tools.GwService {
		var in struct {
			Action string `json:"action"`
		}
		_ = json.Unmarshal(input, &in)
		return strings.EqualFold(strings.TrimSpace(in.Action), "status")
	}
	return false
}

// adminTool gates and dispatches an admin tool call to the injected executor.
func (s *Session) adminTool(ctx context.Context, name string, input json.RawMessage) toolResult {
	if s.adminExec == nil {
		return errResult("admin tools are unavailable in this session")
	}
	if !adminReadOnly(name, input) {
		title := name
		if compact := compactAdminInput(input); compact != "" {
			title = fmt.Sprintf("%s %s", name, compact)
		}
		if ok, reason := s.gate(ctx, permissions.Medium, false, ApprovalRequest{
			Title: title, Label: "Gateway change", Risk: permissions.Medium.String(),
		}); !ok {
			return errResult("denied: " + reason)
		}
	}
	out, err := s.adminExec(ctx, name, input)
	if err != nil {
		return errResult(err.Error())
	}
	return textResult(out)
}

// compactAdminInput renders tool input as a short single-line summary for the
// approval card, "" when it is empty.
func compactAdminInput(input json.RawMessage) string {
	var m map[string]any
	if json.Unmarshal(input, &m) != nil || len(m) == 0 {
		return ""
	}
	b, err := json.Marshal(m)
	if err != nil {
		return ""
	}
	s := string(b)
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	return s
}
