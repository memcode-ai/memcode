// Package policy is memcode's user-authored behavior layer: typed settings on
// named decision points, resolved deterministically at runtime.
//
// The rule the whole package exists to enforce:
//
//	Policy chooses behavior. The model may not synthesize policy.
//
// A user says "always review plans with grok"; the model translates that into
// one Set call; the runtime reads the result. What must never happen is a model
// deciding on its own that some task deserves a different model — that is the
// automatic routing memcode deleted, and it is not allowed back in through a
// settings API.
//
// Two consequences run through the design. Policies are UNCONDITIONAL: every
// one applies every time, and nothing inspects or classifies a task to decide
// whether a policy fires. And fallbacks are OPERATIONAL only: a declared
// fallback exists for a provider that cannot be reached, never for a result
// that looked weak.
//
// This is deliberately NOT internal/prefs. That system infers standing
// preferences from repeated signals and injects advisory prose into the prompt.
// This one is explicit, immediate, programmatic, and never inferred. Neither
// writes to the other.
package policy

// Target names a stable decision point. Adding a controllable behavior means
// registering a Target with a schema and calling Resolve where the decision is
// made — there is no other extension mechanism, and no target-specific
// resolution code anywhere.
type Target string

const (
	// AgentDelegated is the model for delegated work: agent-tool workers,
	// scouts, plan research. Unset inherits the session's primary pin.
	AgentDelegated Target = "agent.delegated"
	// AgentExplore narrows AgentDelegated for read-only explore/scout agents.
	AgentExplore Target = "agent.explore"
	// PlanReview governs the second-model plan critique.
	PlanReview Target = "plan.review"
	// PlanAdvisor governs the plan-mode advisor.
	PlanAdvisor Target = "plan.advisor"
	// StartupModel governs how a session picks its primary model at launch.
	StartupModel Target = "startup.model"
	// UITheme governs the colour theme.
	UITheme Target = "ui.theme"
	// SessionEffort governs the default thinking depth.
	SessionEffort Target = "session.effort"
)

// Kind is the closed set of field types. Values are validated against their
// kind at SET time, so a malformed policy is refused where the user can see the
// refusal, rather than surfacing as a broken turn later.
type Kind string

const (
	KindModel     Kind = "model"      // a pinnable catalog label
	KindModelList Kind = "model_list" // an ordered list of pinnable labels
	KindEnum      Kind = "enum"       // one of Field.Enum
	KindInt       Kind = "int"        // an integer within [Min,Max]
	KindStrategy  Kind = "strategy"   // an enum that selects HOW a value is produced
)

// Field is one typed knob on a target.
type Field struct {
	Name string
	Kind Kind
	Enum []string // KindEnum / KindStrategy: the permitted values
	Min  int      // KindInt
	Max  int      // KindInt
	Def  any      // the component default — what applies when nothing is set anywhere
	Doc  string   // shown in `/policy` and in the tool description
}

// Schema declares a target's fields and where it inherits from.
type Schema struct {
	Target Target
	Doc    string
	Fields []Field

	// Parent is the target an unset field falls through to. Inheritance is
	// DECLARED here, never special-cased in the resolver: agent.explore names
	// agent.delegated as its parent, and a future agent.research or plan.scout
	// does the same and needs no resolution code of its own.
	Parent Target

	// InheritsPrimaryModel ends a model chain at the session's primary pin.
	// This is how "unset delegated means run everything on the user's own
	// model" is expressed as schema rather than as a hand-written chain.
	InheritsPrimaryModel bool
}

// Field returns a field by name.
func (s Schema) Field(name string) (Field, bool) {
	for _, f := range s.Fields {
		if f.Name == name {
			return f, true
		}
	}
	return Field{}, false
}

const (
	// ModeOff / ModeOffer / ModeAlways are the shared vocabulary for a step
	// that can be disabled, offered, or run every time. They are an enum rather
	// than a bool because "never review", "offer review" and "always review
	// with X" are three independent things, and a bool plus a nil model can
	// only express two of them.
	ModeOff    = "off"
	ModeOffer  = "offer"
	ModeAlways = "always"
)

// modelFields are the two fields every model-bearing target shares. fallback is
// OPERATIONAL: it is consulted when the chosen model cannot be REACHED, under
// the same provider/transport semantics the catalog's declared chain already
// uses. Nothing may consult it because output looked poor or a task looked
// hard — there is no path from a quality judgment to a model change.
func modelFields(doc string) []Field {
	return []Field{
		{Name: "model", Kind: KindModel, Doc: doc},
		{Name: "fallback_models", Kind: KindModelList,
			Doc: "models to try when the chosen one cannot be reached (provider errors only, never because a result looked weak); the catalog's declared chain continues after these"},
	}
}

var schemas = map[Target]Schema{
	AgentDelegated: {
		Target:               AgentDelegated,
		Doc:                  "delegated work: sub-agents, scouts, plan research",
		InheritsPrimaryModel: true,
		Fields:               modelFields("the model delegated work runs on; unset means your own model"),
	},
	AgentExplore: {
		Target: AgentExplore,
		Doc:    "read-only explore/scout agents",
		Parent: AgentDelegated,
		Fields: append(modelFields("the model explore agents run on; unset inherits delegated work's model"),
			Field{Name: "concurrency", Kind: KindInt, Min: 1, Max: 32, Def: 6,
				Doc: "how many explorers may read at once"}),
	},
	PlanReview: {
		Target: PlanReview,
		Doc:    "the second-model plan critique",
		Parent: AgentDelegated,
		Fields: append([]Field{
			{Name: "mode", Kind: KindEnum, Enum: []string{ModeOff, ModeOffer, ModeAlways}, Def: ModeOffer,
				Doc: "off = never review; offer = show it on the approval card; always = review every plan"},
		}, modelFields("the model that critiques plans")...),
	},
	PlanAdvisor: {
		Target: PlanAdvisor,
		Doc:    "the plan-mode advisor's second opinion",
		Parent: AgentDelegated,
		Fields: append([]Field{
			{Name: "mode", Kind: KindEnum, Enum: []string{ModeOff, ModeOffer, ModeAlways}, Def: ModeOffer,
				Doc: "off = never; offer = show it on the approval card; always = advise on every plan"},
		}, modelFields("the model that advises on plans")...),
	},
	StartupModel: {
		Target: StartupModel,
		Doc:    "how a new session picks its model",
		Fields: []Field{
			{Name: "strategy", Kind: KindStrategy, Enum: []string{"pinned", "random", "rotate"}, Def: "pinned",
				Doc: "pinned = your remembered model; random = pick from candidates each launch; rotate = take the next candidate in order"},
			{Name: "candidates", Kind: KindModelList,
				Doc: "the models random/rotate choose from"},
		},
	},
	UITheme: {
		Target: UITheme,
		Doc:    "the colour theme",
		Fields: []Field{
			{Name: "strategy", Kind: KindStrategy, Enum: []string{"fixed", "random"}, Def: "fixed",
				Doc: "fixed = always `value`; random = a fresh theme each launch"},
			{Name: "value", Kind: KindEnum, Doc: "the theme name when strategy is fixed"},
		},
	},
	SessionEffort: {
		Target: SessionEffort,
		Doc:    "default thinking depth",
		Fields: []Field{
			// The shipped behavior is deliberately session-only ("resets to auto
			// on restart"). The default preserves that; persisting it is
			// available to someone who explicitly asks, and only then.
			{Name: "default", Kind: KindEnum, Enum: []string{"off", "medium", "high", "auto"}, Def: "auto",
				Doc: "thinking depth new turns start at"},
		},
	},
}

// Lookup returns a target's schema.
func Lookup(t Target) (Schema, bool) { s, ok := schemas[t]; return s, ok }

// Targets lists every registered target in a stable order.
func Targets() []Target {
	out := make([]Target, 0, len(schemas))
	for t := range schemas {
		out = append(out, t)
	}
	sortTargets(out)
	return out
}
