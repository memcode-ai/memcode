package policy

import (
	"fmt"
	"sort"
	"strings"

	"github.com/memcode-ai/memcode/catalog"
)

func sortTargets(t []Target) {
	sort.Slice(t, func(i, j int) bool { return t[i] < t[j] })
}

// Scope names where a resolved value came from. Every resolved field reports
// one, because per-field resolution across four layers is only comprehensible
// if the user can ask "why is it that, and who set it?".
type Scope string

const (
	ScopeOverride  Scope = "override"  // this operation only
	ScopeSession   Scope = "session"   // this session
	ScopeWorkspace Scope = "workspace" // this repo
	ScopeUser      Scope = "user"      // everywhere
	ScopeInherited Scope = "inherited" // a parent target, or the primary pin
	ScopeDefault   Scope = "default"   // the schema's declared default
)

// PersistedScopes are the scopes backed by a file. Session and override are
// deliberately absent: they are held in memory by whoever owns them.
var PersistedScopes = []Scope{ScopeWorkspace, ScopeUser}

// Set is one scope's contents: target -> field -> value.
type Set map[Target]map[string]any

func (s Set) get(t Target, field string) (any, bool) {
	if s == nil {
		return nil, false
	}
	fields, ok := s[t]
	if !ok {
		return nil, false
	}
	v, ok := fields[field]
	return v, ok
}

// Put stores a validated value. Callers should go through Validate first;
// SetField does both.
func (s Set) Put(t Target, field string, v any) {
	if s[t] == nil {
		s[t] = map[string]any{}
	}
	s[t][field] = v
}

// Clear removes a whole target — the granularity "reset how explore agents
// behave" needs.
func (s Set) Clear(t Target) { delete(s, t) }

// Value is one resolved field plus where it came from.
type Value struct {
	Raw    any
	Source Scope
}

// Resolved is a target's effective policy.
type Resolved struct {
	Target Target
	Values map[string]Value
}

// Model returns a model-label field, "" when unset all the way down.
func (r Resolved) Model(field string) string { return r.str(field) }

// Enum returns an enum/strategy field.
func (r Resolved) Enum(field string) string { return r.str(field) }

// Mode is the conventional name for a target's off/offer/always field.
func (r Resolved) Mode() string { return r.str("mode") }

func (r Resolved) str(field string) string {
	v, ok := r.Values[field]
	if !ok || v.Raw == nil {
		return ""
	}
	s, _ := v.Raw.(string)
	return s
}

// List returns a model_list field.
func (r Resolved) List(field string) []string {
	v, ok := r.Values[field]
	if !ok || v.Raw == nil {
		return nil
	}
	switch x := v.Raw.(type) {
	case []string:
		return x
	case []any: // came back through JSON
		out := make([]string, 0, len(x))
		for _, e := range x {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// Int returns an int field, falling back to the schema default.
func (r Resolved) Int(field string) int {
	v, ok := r.Values[field]
	if !ok || v.Raw == nil {
		return 0
	}
	switch x := v.Raw.(type) {
	case int:
		return x
	case float64: // came back through JSON
		return int(x)
	}
	return 0
}

// Source reports which scope supplied a field.
func (r Resolved) Source(field string) Scope {
	if v, ok := r.Values[field]; ok {
		return v.Source
	}
	return ScopeDefault
}

// Resolver holds the layers and answers Resolve. One per session.
type Resolver struct {
	Session   Set
	Workspace Set
	User      Set

	// Primary is the session's primary model pin — where a model chain ends
	// for a schema that declares InheritsPrimaryModel.
	Primary string
}

// Option customises one Resolve call.
type Option func(*resolveOpts)

type resolveOpts struct{ override Set }

// Override supplies values scoped to THIS operation. It is passed in by
// whoever owns the operation and is never stored: "review this plan with kimi"
// belongs to that plan and disappears when the plan does. There is no global
// consume-on-next-use state that could leak into an unrelated later operation.
func Override(t Target, fields map[string]any) Option {
	return func(o *resolveOpts) {
		if o.override == nil {
			o.override = Set{}
		}
		for k, v := range fields {
			o.override.Put(t, k, v)
		}
	}
}

// Resolve computes a target's effective policy, field by field.
//
// Per field: override -> session -> workspace -> user -> parent target ->
// primary pin (if the schema ends there) -> the schema default. Per-field
// rather than per-target means a user-level model and a workspace-level
// fallback list compose instead of one silently replacing the other.
func (r *Resolver) Resolve(t Target, opts ...Option) Resolved {
	var o resolveOpts
	for _, fn := range opts {
		fn(&o)
	}
	out := Resolved{Target: t, Values: map[string]Value{}}
	schema, ok := Lookup(t)
	if !ok {
		return out
	}
	for _, f := range schema.Fields {
		out.Values[f.Name] = r.resolveField(t, f, o.override, map[Target]bool{})
	}
	return out
}

func (r *Resolver) resolveField(t Target, f Field, override Set, seen map[Target]bool) Value {
	if seen[t] { // a Parent cycle would otherwise hang; treat it as unset
		return Value{Raw: f.Def, Source: ScopeDefault}
	}
	seen[t] = true

	for _, layer := range []struct {
		set   Set
		scope Scope
	}{
		{override, ScopeOverride},
		{r.Session, ScopeSession},
		{r.Workspace, ScopeWorkspace},
		{r.User, ScopeUser},
	} {
		if v, ok := layer.set.get(t, f.Name); ok && !isEmpty(v) {
			return Value{Raw: v, Source: layer.scope}
		}
	}

	schema, _ := Lookup(t)
	// Declared inheritance: fall through to the parent target's same-named
	// field. This is why a new sub-target needs no resolution code.
	if schema.Parent != "" {
		if pf, ok := mustSchema(schema.Parent).Field(f.Name); ok {
			if v := r.resolveField(schema.Parent, pf, override, seen); !isEmpty(v.Raw) {
				return Value{Raw: v.Raw, Source: ScopeInherited}
			}
		}
	}
	// A model chain can end at the session's own model.
	if schema.InheritsPrimaryModel && f.Kind == KindModel && r.Primary != "" {
		return Value{Raw: r.Primary, Source: ScopeInherited}
	}
	return Value{Raw: f.Def, Source: ScopeDefault}
}

func mustSchema(t Target) Schema { s, _ := Lookup(t); return s }

func isEmpty(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(x) == ""
	case []string:
		return len(x) == 0
	case []any:
		return len(x) == 0
	}
	return false
}

// Validate normalises and checks a value against its field's kind. It runs at
// SET time so a bad policy is refused where the user can see it, not two turns
// later inside a failing call.
func Validate(t Target, field string, raw any) (any, error) {
	schema, ok := Lookup(t)
	if !ok {
		return nil, fmt.Errorf("unknown policy target %q", t)
	}
	f, ok := schema.Field(field)
	if !ok {
		return nil, fmt.Errorf("%s has no field %q", t, field)
	}
	switch f.Kind {
	case KindModel:
		s, _ := raw.(string)
		return validModel(strings.TrimSpace(strings.ToLower(s)))
	case KindModelList:
		var in []string
		switch x := raw.(type) {
		case []string:
			in = x
		case []any:
			for _, e := range x {
				if s, ok := e.(string); ok {
					in = append(in, s)
				}
			}
		default:
			return nil, fmt.Errorf("%s.%s wants a list of models", t, field)
		}
		out := make([]string, 0, len(in))
		for _, s := range in {
			m, err := validModel(strings.TrimSpace(strings.ToLower(s)))
			if err != nil {
				return nil, err
			}
			out = append(out, m.(string))
		}
		return out, nil
	case KindEnum, KindStrategy:
		s := strings.TrimSpace(strings.ToLower(fmt.Sprint(raw)))
		if len(f.Enum) == 0 { // an open enum (theme names) — the consumer validates
			return s, nil
		}
		for _, e := range f.Enum {
			if s == e {
				return s, nil
			}
		}
		return nil, fmt.Errorf("%s.%s must be one of %s", t, field, strings.Join(f.Enum, ", "))
	case KindInt:
		var n int
		switch x := raw.(type) {
		case int:
			n = x
		case float64:
			n = int(x)
		default:
			return nil, fmt.Errorf("%s.%s wants a number", t, field)
		}
		if n < f.Min || n > f.Max {
			return nil, fmt.Errorf("%s.%s must be between %d and %d", t, field, f.Min, f.Max)
		}
		return n, nil
	}
	return nil, fmt.Errorf("%s.%s has an unknown kind", t, field)
}

func validModel(label string) (any, error) {
	if label == "" {
		return nil, fmt.Errorf("a model label is required")
	}
	m, ok := catalog.LookupModel(label)
	if !ok {
		return nil, fmt.Errorf("%q is not a model in the catalog", label)
	}
	if !m.Pinnable {
		return nil, fmt.Errorf("%q can't be pinned", label)
	}
	return m.Label, nil
}
