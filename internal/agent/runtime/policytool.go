package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/memcode-ai/memcode/internal/agent/tools"
	"github.com/memcode-ai/memcode/internal/policy"
)

// policytool.go — the conversational surface over user policy.
//
// The contract, one-directional:
//
//	user instruction -> tool call -> typed policy store -> runtime resolves it
//
// and never:
//
//	model -> decides this task deserves a different model
//
// A model translating "always review plans with grok" into a Set call is doing
// what it was told. A model deciding mid-task that something looks hard enough
// to warrant a stronger model is routing, which memcode deleted. Nothing in
// this file inspects a task, and no policy has a condition to match.

func (s *Session) policyTool(ctx context.Context, input json.RawMessage) toolResult {
	var in tools.PolicyInput
	if err := json.Unmarshal(input, &in); err != nil {
		return errResult(err.Error())
	}
	switch strings.TrimSpace(strings.ToLower(in.Action)) {
	case "", "show":
		return textResult(s.policySummary())
	case "set":
		return s.policySet(in)
	case "unset":
		return s.policyUnset(in)
	default:
		return errResult("action must be set, unset or show.")
	}
}

func (s *Session) policySet(in tools.PolicyInput) toolResult {
	target := policy.Target(strings.TrimSpace(strings.ToLower(in.Target)))
	schema, ok := policy.Lookup(target)
	if !ok {
		return errResult("unknown target " + string(target) + ". Known: " + strings.Join(targetNames(), ", "))
	}
	if len(in.Fields) == 0 {
		return errResult("set needs at least one field for " + string(target) + ".")
	}

	scope := strings.TrimSpace(strings.ToLower(in.Scope))
	if scope == "" {
		scope = "workspace"
	}

	// Validate EVERYTHING before writing anything: a half-applied policy is
	// worse than a refused one, because the user believes it took.
	clean := map[string]any{}
	for k, v := range in.Fields {
		norm, err := policy.Validate(target, k, v)
		if err != nil {
			return errResult(err.Error())
		}
		clean[k] = norm
	}

	switch scope {
	case "operation":
		// Scoped to the operation in flight and discarded when it ends. Only
		// plan-shaped targets have an operation to attach to today.
		if !s.planCtl.Planning() {
			return errResult("there's no plan in progress to scope that to — say it while planning, or set it for the session instead.")
		}
		if target != policy.PlanReview && target != policy.PlanAdvisor {
			return errResult(string(target) + " has no per-operation scope; use session, workspace or user.")
		}
		s.planCtl.SetPolicyOverride(string(target), clean)
	case "session":
		for k, v := range clean {
			s.policy.Session.Put(target, k, v)
		}
	case "workspace":
		for k, v := range clean {
			if err := policy.SetField(policy.WorkspacePath(s.root), target, k, v); err != nil {
				return errResult("couldn't save: " + err.Error())
			}
			s.policy.Workspace.Put(target, k, v)
		}
	case "user":
		for k, v := range clean {
			if err := policy.SetField(policy.UserPath(), target, k, v); err != nil {
				return errResult("couldn't save: " + err.Error())
			}
			s.policy.User.Put(target, k, v)
		}
	default:
		return errResult("scope must be operation, session, workspace or user.")
	}

	// Say it out loud. A setting that changes what the user is billed for should
	// never be a silent side effect of a sentence.
	desc := describeFields(clean)
	s.printf("%s\n", metaStyle.Render("  ⊙ "+string(target)+" → "+desc+" ("+scopeWord(scope)+")"))
	return textResult(fmt.Sprintf("%s (%s) is now %s for %s.", target, schema.Doc, desc, scopeWord(scope)))
}

func (s *Session) policyUnset(in tools.PolicyInput) toolResult {
	target := policy.Target(strings.TrimSpace(strings.ToLower(in.Target)))
	if _, ok := policy.Lookup(target); !ok {
		return errResult("unknown target " + string(target) + ".")
	}
	scope := strings.TrimSpace(strings.ToLower(in.Scope))
	if scope == "" {
		scope = "workspace"
	}
	switch scope {
	case "session":
		s.policy.Session.Clear(target)
	case "workspace":
		if err := policy.UnsetTarget(policy.WorkspacePath(s.root), target); err != nil {
			return errResult(err.Error())
		}
		s.policy.Workspace.Clear(target)
	case "user":
		if err := policy.UnsetTarget(policy.UserPath(), target); err != nil {
			return errResult(err.Error())
		}
		s.policy.User.Clear(target)
	default:
		return errResult("scope must be session, workspace or user.")
	}
	s.printf("%s\n", metaStyle.Render("  ⊙ "+string(target)+" reset ("+scopeWord(scope)+")"))
	return textResult(string(target) + " is back to its default for " + scopeWord(scope) + ".")
}

// policySummary answers "how have I customized memcode?" — every target, its
// effective value, and WHERE each value came from. Per-field resolution across
// four layers is only comprehensible if the source is visible.
func (s *Session) PolicySummary() string { return s.policySummary() }

func (s *Session) policySummary() string {
	var b strings.Builder
	b.WriteString("Effective policy (value ← where it came from):\n")
	for _, t := range policy.Targets() {
		schema, _ := policy.Lookup(t)
		res := s.policy.Resolve(t)
		var rows []string
		for _, f := range schema.Fields {
			v := res.Values[f.Name]
			if v.Source == policy.ScopeDefault && isBlank(v.Raw) {
				continue // unset and no default worth reporting
			}
			rows = append(rows, fmt.Sprintf("    %-16s %-22v ← %s", f.Name, v.Raw, v.Source))
		}
		if len(rows) == 0 {
			continue
		}
		b.WriteString("\n  " + string(t) + "  (" + schema.Doc + ")\n")
		b.WriteString(strings.Join(rows, "\n") + "\n")
	}
	b.WriteString("\nAnything marked default or inherited is not something you set.")
	return b.String()
}

func isBlank(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(x) == ""
	case []string:
		return len(x) == 0
	}
	return false
}

func describeFields(f map[string]any) string {
	keys := make([]string, 0, len(f))
	for k := range f {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v", k, f[k]))
	}
	return strings.Join(parts, " ")
}

func scopeWord(scope string) string {
	switch scope {
	case "operation":
		return "this plan"
	case "session":
		return "this session"
	case "user":
		return "everywhere"
	default:
		return "this repo"
	}
}

func targetNames() []string {
	ts := policy.Targets()
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, string(t))
	}
	return out
}
