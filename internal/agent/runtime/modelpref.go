package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/memcode-ai/memcode/catalog"
	"github.com/memcode-ai/memcode/internal/agent/tools"
	"github.com/memcode-ai/memcode/internal/config"
)

// modelpref.go — the conversational surface over the DELEGATED model pin.
//
// The shape is deliberately one-directional:
//
//	user instruction -> tool call -> persisted delegated pin -> runtime reads it
//
// and never:
//
//	model -> picks a model for this particular agent() call
//
// The difference is the whole point. A model translating "use Sonnet for your
// subagents" into a settings write is doing what it was told. A model deciding
// mid-task that some work looks cheap enough for a smaller model is routing,
// which is the thing that got deleted. Nothing in this file inspects a task.
//
// The agent tool has no model parameter, so there is no per-invocation override
// to reach for even if a model wanted one.

// modelPreferenceTool sets (or clears) the delegated model pin at a scope.
func (s *Session) modelPreferenceTool(ctx context.Context, input json.RawMessage) toolResult {
	var in tools.ModelPreferenceInput
	if err := json.Unmarshal(input, &in); err != nil {
		return errResult(err.Error())
	}
	label := strings.TrimSpace(strings.ToLower(in.Model))
	if label == "" {
		return errResult("model_preference needs `model` — a model label, or \"inherit\".")
	}

	scope := strings.TrimSpace(strings.ToLower(in.Scope))
	switch scope {
	case "", "workspace", "user", "session":
	default:
		return errResult("scope must be session, workspace or user.")
	}
	if scope == "" {
		scope = "workspace"
	}

	// "inherit" is the reset: delegated work goes back to the user's own model.
	// It has to be expressible, or a preference could only ever be replaced and
	// never undone.
	inherit := label == "inherit" || label == "primary" || label == "same" || label == "default"
	window := 0
	if !inherit {
		m, ok := catalog.LookupModel(label)
		if !ok {
			return errResult(fmt.Sprintf("%q is not a model in the catalog — run /models to see what is available.", in.Model))
		}
		if !m.Pinnable {
			return errResult(fmt.Sprintf("%q can't be pinned.", label))
		}
		label, window = m.Label, m.Window
	} else {
		label = ""
	}

	// Session scope is in-memory only, by definition: it must not outlive the
	// session, so it never touches a file.
	if scope != "session" {
		cfg, err := config.Load(s.root)
		if err != nil {
			return errResult("couldn't open this project's config: " + err.Error())
		}
		if err := config.SetDelegatedPin(cfg, scope, label, window); err != nil {
			return errResult("couldn't save the preference: " + err.Error())
		}
	}
	s.SetDelegatedPin(label, window)

	// Say it out loud. A model preference that changes what the user is billed
	// for should never be a silent side effect of a sentence.
	where := map[string]string{"session": "this session", "workspace": "this repo", "user": "everywhere"}[scope]
	if inherit {
		s.printf("%s\n", metaStyle.Render("  ⊙ delegated work → your own model ("+where+")"))
		return textResult("Delegated work (sub-agents, scouts, plan research) now runs on the same model you do, for " + where + ".")
	}
	s.printf("%s\n", metaStyle.Render("  ⊙ delegated work → "+label+" ("+where+")"))
	return textResult("Delegated work (sub-agents, scouts, plan research) now runs on " + label + " for " + where +
		". Your own model is unchanged.")
}
