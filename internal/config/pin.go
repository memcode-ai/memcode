package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/memcode-ai/memcode/catalog"
	"github.com/memcode-ai/memcode/internal/atomicfile"
)

// pin.go — the ONE authority for "what model is this session on".
//
// There is exactly one model per session and the user chose it. Automatic
// routing, which re-decided the model on every turn, is gone; what replaces it
// is not a smarter chooser but a remembered choice.
//
// Resolution order:
//
//	session override (--model, /model this session)
//	  -> workspace last-used   (.memcode/config.json)
//	  -> user last-used        (~/.config/memcode/prefs.json)
//	  -> default_model seed    (catalog)
//
// The seed fires exactly once, on an install that has never chosen anything,
// and is PERSISTED immediately — so the next run reads a concrete pin rather
// than re-deriving it. That distinction is the whole point: a value consulted
// once at initialization is a default; a value consulted on every request is a
// routing decision wearing a default's clothes.
//
// Nothing else in the codebase may read catalog.DefaultModel(). A guard test
// enforces that.

// prefsFile is the user-level (cross-workspace) model memory. It lives beside
// the existing ~/.config/memcode/.env and gateway.yaml.
type prefsFile struct {
	PinnedModel  string `json:"pinned_model,omitempty"`
	PinnedWindow int    `json:"pinned_window,omitempty"`
	// DelegatedModel is the user-level DELEGATED pin (see ResolveDelegatedPin).
	// Empty means "inherit the primary" — it is never seeded.
	DelegatedModel  string `json:"delegated_model,omitempty"`
	DelegatedWindow int    `json:"delegated_window,omitempty"`
}

// UserPrefsPath returns $XDG_CONFIG_HOME/memcode/prefs.json, else
// ~/.config/memcode/prefs.json. "" when no home directory can be determined —
// callers treat that as "no user-level memory", never as an error.
func UserPrefsPath() string {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "memcode", "prefs.json")
}

// loadUserPrefs reads the user-level model memory. A missing or unreadable file
// is simply "nothing remembered" — this is a convenience layer, and a corrupt
// prefs file must never stop a session from starting.
func loadUserPrefs() prefsFile {
	path := UserPrefsPath()
	if path == "" {
		return prefsFile{}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return prefsFile{}
	}
	var p prefsFile
	if json.Unmarshal(b, &p) != nil {
		return prefsFile{}
	}
	return p
}

// writeUserPrefs persists the whole prefs file. Callers mutate a loaded copy so
// one pin never clobbers the other — changing the primary must not silently
// reset an explicitly configured delegated model.
func writeUserPrefs(p prefsFile) {
	path := UserPrefsPath()
	if path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return
	}
	_ = atomicfile.WriteFile(path, append(b, '\n'), 0o600)
}

// SaveUserPin records the PRIMARY model at the USER level, so a different repo
// starts on the same one. Best-effort by design: failing to remember a
// preference must never fail the operation the user actually asked for.
func SaveUserPin(label string, window int) {
	if label == "" {
		return
	}
	p := loadUserPrefs()
	p.PinnedModel, p.PinnedWindow = label, window
	writeUserPrefs(p)
}

// SaveUserDelegatedPin records the DELEGATED model at the user level. An empty
// label clears it, which means "inherit the primary" — that is how a reset is
// expressed, and it must be distinguishable from "never set".
func SaveUserDelegatedPin(label string, window int) {
	p := loadUserPrefs()
	p.DelegatedModel, p.DelegatedWindow = label, window
	writeUserPrefs(p)
}

// ResolvePin returns the model this session runs on, plus its context window.
//
// override is a session-only choice (--model) and is never persisted: it is
// this invocation's model, not a new preference.
//
// When resolution reaches the seed, the pin is persisted to BOTH stores before
// returning, so this is the only run that ever consults default_model.
func ResolvePin(cfg *Config, override string) (label string, window int) {
	if override != "" {
		return override, catalog.ContextWindow(override)
	}
	if cfg != nil && cfg.PinnedModel != "" {
		return cfg.PinnedModel, cfg.PinnedWindow
	}
	if p := loadUserPrefs(); p.PinnedModel != "" {
		// Remembered at the user level but not in this workspace — adopt it here
		// too, so the workspace answers for itself next time.
		if cfg != nil {
			cfg.PinnedModel, cfg.PinnedWindow = p.PinnedModel, p.PinnedWindow
			_ = cfg.Save()
		}
		return p.PinnedModel, p.PinnedWindow
	}

	seed := catalog.DefaultModel()
	if seed == "" {
		// No seed declared: return empty and let selection refuse with its own
		// message. Inventing a model here is the one thing this must not do.
		return "", 0
	}
	w := catalog.ContextWindow(seed)
	if cfg != nil {
		cfg.PinnedModel, cfg.PinnedWindow = seed, w
		_ = cfg.Save()
	}
	SaveUserPin(seed, w)
	return seed, w
}

// ResolveDelegatedPin returns the model DELEGATED work runs on: agent-tool
// workers, explore/research scouts, plan-mode scouts, and any future delegated
// execution.
//
// It is a second PIN, not a router. The chain mirrors the primary's —
// session override -> workspace -> user — but ends differently: there is no
// seed. An unset delegated pin means INHERIT THE PRIMARY, so the default
// behaviour is "everything runs on the model you chose", and configuring this
// is an explicit opt-in to spending differently on delegated work.
//
// That distinction is load-bearing. Seeding it from the catalog would make the
// split happen to people who never asked for it, which is the automatic
// routing this replaced. Inheriting means the only way delegated work lands on
// a different model is because someone said so.
//
// Nothing derives this value. It is written only by an explicit user
// instruction (the model_preference tool) and read deterministically here.
func ResolveDelegatedPin(cfg *Config, override, primary string, primaryWindow int) (label string, window int) {
	if override != "" {
		return override, catalog.ContextWindow(override)
	}
	if cfg != nil && cfg.DelegatedModel != "" {
		return cfg.DelegatedModel, cfg.DelegatedWindow
	}
	if p := loadUserPrefs(); p.DelegatedModel != "" {
		return p.DelegatedModel, p.DelegatedWindow
	}
	return primary, primaryWindow // inherit
}

// SetDelegatedPin writes the delegated pin at the given scope and reports what
// it wrote. An empty label RESETS to inherit-the-primary at that scope.
//
// Scope vocabulary matches the resolution chain: "workspace" (this repo) and
// "user" (everywhere). "session" is not persisted and is handled by the caller,
// which holds the session state.
func SetDelegatedPin(cfg *Config, scope, label string, window int) error {
	switch scope {
	case "user":
		SaveUserDelegatedPin(label, window)
		// Clear the workspace value too, or it would keep shadowing the
		// user-level choice the person just made "from now on".
		if cfg != nil && cfg.DelegatedModel != "" {
			cfg.DelegatedModel, cfg.DelegatedWindow = "", 0
			if err := cfg.Save(); err != nil {
				return err
			}
		}
		return nil
	case "workspace", "":
		if cfg == nil {
			return errNoProject
		}
		cfg.DelegatedModel, cfg.DelegatedWindow = label, window
		return cfg.Save()
	default:
		return fmt.Errorf("unknown scope %q (want workspace or user)", scope)
	}
}

var errNoProject = errors.New("no project config to write to")
