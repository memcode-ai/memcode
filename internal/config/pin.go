package config

import (
	"encoding/json"
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

// SaveUserPin records the model at the USER level, so a different repo starts
// on the same one. Best-effort by design: failing to remember a preference must
// never fail the operation the user actually asked for.
func SaveUserPin(label string, window int) {
	path := UserPrefsPath()
	if path == "" || label == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	b, err := json.MarshalIndent(prefsFile{PinnedModel: label, PinnedWindow: window}, "", "  ")
	if err != nil {
		return
	}
	_ = atomicfile.WriteFile(path, append(b, '\n'), 0o600)
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
