package policy

import (
	"path/filepath"
	"testing"
)

// The resolution chain, field by field: override -> session -> workspace ->
// user -> parent -> primary -> default.
func TestResolutionPrecedence(t *testing.T) {
	r := &Resolver{
		Session:   Set{},
		Workspace: Set{},
		User:      Set{},
		Primary:   "opus",
	}

	// Nothing set anywhere: a model chain that declares InheritsPrimaryModel
	// ends at the user's own model. This is the default, and it is why adding
	// policy changes nothing for someone who never sets any.
	got := r.Resolve(AgentDelegated)
	if got.Model("model") != "opus" || got.Source("model") != ScopeInherited {
		t.Fatalf("unset delegated = %q (%s), want opus inherited", got.Model("model"), got.Source("model"))
	}

	for _, tc := range []struct {
		name  string
		apply func()
		want  string
		src   Scope
	}{
		{"user", func() { r.User.Put(AgentDelegated, "model", "sonnet") }, "sonnet", ScopeUser},
		{"workspace beats user", func() { r.Workspace.Put(AgentDelegated, "model", "haiku") }, "haiku", ScopeWorkspace},
		{"session beats workspace", func() { r.Session.Put(AgentDelegated, "model", "luna") }, "luna", ScopeSession},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.apply()
			got := r.Resolve(AgentDelegated)
			if got.Model("model") != tc.want || got.Source("model") != tc.src {
				t.Fatalf("= %q (%s), want %q (%s)", got.Model("model"), got.Source("model"), tc.want, tc.src)
			}
		})
	}

	// An override beats every stored layer, for this call only.
	got = r.Resolve(AgentDelegated, Override(AgentDelegated, map[string]any{"model": "terra"}))
	if got.Model("model") != "terra" || got.Source("model") != ScopeOverride {
		t.Fatalf("override = %q (%s), want terra", got.Model("model"), got.Source("model"))
	}
	// ...and the very next Resolve, without it, is back to the stored value.
	if got := r.Resolve(AgentDelegated); got.Model("model") != "luna" {
		t.Fatalf("after an override the next resolve = %q, want luna — an override must not persist", got.Model("model"))
	}
}

// Fields resolve INDEPENDENTLY, so a user-level model and a workspace-level
// fallback compose instead of one replacing the other wholesale.
func TestFieldsResolveIndependently(t *testing.T) {
	r := &Resolver{User: Set{}, Workspace: Set{}, Primary: "opus"}
	r.User.Put(AgentExplore, "model", "haiku")
	r.Workspace.Put(AgentExplore, "fallback_models", []string{"gemini-flash"})

	got := r.Resolve(AgentExplore)
	if got.Model("model") != "haiku" || got.Source("model") != ScopeUser {
		t.Errorf("model = %q (%s), want haiku from user", got.Model("model"), got.Source("model"))
	}
	if fb := got.List("fallback_models"); len(fb) != 1 || fb[0] != "gemini-flash" {
		t.Errorf("fallback = %v, want [gemini-flash] from workspace", fb)
	}
}

// Inheritance is declared, not special-cased: explore -> delegated -> primary.
func TestParentTargetInheritance(t *testing.T) {
	r := &Resolver{Workspace: Set{}, Primary: "opus"}

	// Nothing anywhere: explore ends at the primary, two hops up.
	if got := r.Resolve(AgentExplore); got.Model("model") != "opus" {
		t.Fatalf("explore with nothing set = %q, want the primary opus", got.Model("model"))
	}
	// Setting the PARENT moves explore with it.
	r.Workspace.Put(AgentDelegated, "model", "sonnet")
	got := r.Resolve(AgentExplore)
	if got.Model("model") != "sonnet" || got.Source("model") != ScopeInherited {
		t.Fatalf("explore = %q (%s), want sonnet inherited from agent.delegated", got.Model("model"), got.Source("model"))
	}
	// Setting explore itself wins over the parent.
	r.Workspace.Put(AgentExplore, "model", "haiku")
	if got := r.Resolve(AgentExplore); got.Model("model") != "haiku" {
		t.Fatalf("explore = %q, want its own haiku", got.Model("model"))
	}
	// ...and the parent is unaffected.
	if got := r.Resolve(AgentDelegated); got.Model("model") != "sonnet" {
		t.Fatalf("delegated = %q, want sonnet — a child must not rewrite its parent", got.Model("model"))
	}
}

// Defaults come from the schema when nothing is set and nothing is inherited.
func TestSchemaDefaults(t *testing.T) {
	r := &Resolver{Primary: "opus"}
	for _, tc := range []struct {
		target Target
		field  string
		want   string
	}{
		{PlanReview, "mode", ModeOffer},
		{PlanAdvisor, "mode", ModeOffer},
		{StartupModel, "strategy", "pinned"},
		{UITheme, "strategy", "fixed"},
		{SessionEffort, "default", "auto"},
	} {
		if got := r.Resolve(tc.target).Enum(tc.field); got != tc.want {
			t.Errorf("%s.%s = %q, want the default %q", tc.target, tc.field, got, tc.want)
		}
	}
	if n := r.Resolve(AgentExplore).Int("concurrency"); n != 6 {
		t.Errorf("explore concurrency = %d, want the default 6", n)
	}
}

// Modes are three-valued on purpose: a bool plus a nil model cannot express
// "never", "offer" and "always with X" independently.
func TestModeIsThreeValued(t *testing.T) {
	for _, target := range []Target{PlanReview, PlanAdvisor} {
		s, _ := Lookup(target)
		f, ok := s.Field("mode")
		if !ok {
			t.Fatalf("%s has no mode field", target)
		}
		if len(f.Enum) != 3 {
			t.Fatalf("%s.mode = %v, want off/offer/always", target, f.Enum)
		}
	}
}

// Bad values are refused at SET time, where the user can see the refusal.
func TestValidationRefusesBadValues(t *testing.T) {
	for _, tc := range []struct {
		name   string
		target Target
		field  string
		raw    any
	}{
		{"unknown model", AgentDelegated, "model", "model-nobody-added"},
		{"unpinnable model", AgentDelegated, "model", "gpt-oss-120b"},
		{"empty model", AgentDelegated, "model", ""},
		{"bad enum", PlanReview, "mode", "sometimes"},
		{"bad strategy", StartupModel, "strategy", "vibes"},
		{"int out of range", AgentExplore, "concurrency", 999},
		{"unknown field", AgentDelegated, "colour", "red"},
		{"unknown target", Target("nope.nope"), "model", "sonnet"},
		{"bad model in list", AgentDelegated, "fallback_models", []string{"sonnet", "not-a-model"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Validate(tc.target, tc.field, tc.raw); err == nil {
				t.Fatalf("%v should be refused", tc.raw)
			}
		})
	}
	// ...and good values normalise.
	if v, err := Validate(AgentDelegated, "model", "  SONNET "); err != nil || v != "sonnet" {
		t.Fatalf("normalisation = %v, %v; want sonnet", v, err)
	}
}

// Per-scope persistence: unset at one scope leaves the other alone.
func TestPersistenceIsPerScope(t *testing.T) {
	dir := t.TempDir()
	ws := filepath.Join(dir, "ws.json")
	user := filepath.Join(dir, "user.json")

	if err := SetField(ws, AgentExplore, "model", "haiku"); err != nil {
		t.Fatal(err)
	}
	if err := SetField(user, AgentExplore, "model", "sonnet"); err != nil {
		t.Fatal(err)
	}
	if err := UnsetTarget(ws, AgentExplore); err != nil {
		t.Fatal(err)
	}
	if _, ok := Load(ws)[AgentExplore]; ok {
		t.Error("workspace target should be gone")
	}
	if got, _ := Load(user).get(AgentExplore, "model"); got != "sonnet" {
		t.Errorf("user scope = %v, want sonnet — unsetting one scope must not touch another", got)
	}
}

// A corrupt policy file is "no policy", never a failure to start.
func TestCorruptFileDegradesToEmpty(t *testing.T) {
	p := filepath.Join(t.TempDir(), "policy.json")
	if err := writeRaw(p, "{not json"); err != nil {
		t.Fatal(err)
	}
	if got := Load(p); len(got) != 0 {
		t.Fatalf("corrupt file = %v, want empty", got)
	}
}

// Every registered target must be resolvable and internally consistent — the
// schema IS the extension point, so a malformed one is a build-time-ish bug.
func TestEverySchemaIsWellFormed(t *testing.T) {
	r := &Resolver{Primary: "opus"}
	for _, target := range Targets() {
		s, _ := Lookup(target)
		if len(s.Fields) == 0 {
			t.Errorf("%s declares no fields", target)
		}
		if s.Parent != "" {
			if _, ok := Lookup(s.Parent); !ok {
				t.Errorf("%s names a parent %q that is not registered", target, s.Parent)
			}
		}
		for _, f := range s.Fields {
			if f.Doc == "" {
				t.Errorf("%s.%s has no doc — it is shown to the user and the model", target, f.Name)
			}
			if (f.Kind == KindEnum || f.Kind == KindStrategy) && f.Def != nil {
				if _, err := Validate(target, f.Name, f.Def); err != nil {
					t.Errorf("%s.%s default %v is invalid: %v", target, f.Name, f.Def, err)
				}
			}
		}
		r.Resolve(target) // must not panic
	}
}
