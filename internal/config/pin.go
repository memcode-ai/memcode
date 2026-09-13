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
func writeUserPrefs(p prefsFile) error {
	path := UserPrefsPath()
	if path == "" {
		return errors.New("no user config directory")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.WriteFile(path, append(b, '\n'), 0o600)
}

// SaveUserPin records the PRIMARY model at the USER level, so a different repo
// starts on the same one. Best-effort by design: failing to remember a
// preference must never fail the operation the user actually asked for.
func SaveUserPin(label string, window int) error {
	if label == "" {
		return nil
	}
	p := loadUserPrefs()
	p.PinnedModel, p.PinnedWindow = label, window
	return writeUserPrefs(p)
}

// ResolvePin returns the model this session runs on, plus its context window.
//
// override is a session-only choice (--model) and is never persisted: it is
// this invocation's model, not a new preference.
//
// When resolution reaches the seed, the pin is persisted to BOTH stores before
// returning, so this is normally the only run that ever consults default_model.
//
// Persistence is BEST-EFFORT and deliberately so: failing to remember a
// preference must never fail the operation the user actually asked for. But a
// failure is not nothing — if neither store is writable, every run re-seeds from
// default_model, and because that value legitimately changes as models are added
// and retired, the user's model can drift between releases while everything here
// believes the pin is stable. ResolvePinSeeded reports that case so a caller can
// say so once; ResolvePin keeps the plain signature for callers that cannot act
// on it anyway.
func ResolvePin(cfg *Config, override string) (label string, window int) {
	label, window, _ = ResolvePinSeeded(cfg, override)
	return label, window
}

// ResolvePinSeeded is ResolvePin plus the seed-persistence outcome: warn is
// non-nil only when this run had to seed from default_model AND could not record
// it, which means the next run will seed again.
func ResolvePinSeeded(cfg *Config, override string) (label string, window int, warn error) {
	if override != "" {
		return override, catalog.ContextWindow(override), nil
	}
	if cfg != nil && cfg.PinnedModel != "" {
		return cfg.PinnedModel, cfg.PinnedWindow, nil
	}
	if p := loadUserPrefs(); p.PinnedModel != "" {
		// Remembered at the user level but not in this workspace — adopt it here
		// too, so the workspace answers for itself next time. A failure here is
		// not reported: the pin IS remembered, just not yet in this workspace,
		// so the next run still resolves to the same model.
		if cfg != nil {
			cfg.PinnedModel, cfg.PinnedWindow = p.PinnedModel, p.PinnedWindow
			_ = cfg.Save()
		}
		return p.PinnedModel, p.PinnedWindow, nil
	}

	seed := catalog.DefaultModel()
	if seed == "" {
		// No seed declared: return empty and let selection refuse with its own
		// message. Inventing a model here is the one thing this must not do.
		return "", 0, nil
	}
	w := catalog.ContextWindow(seed)

	// Seeding is the one branch whose whole purpose is to not happen again, so
	// this is where a write failure actually costs something. Report it only if
	// BOTH stores failed — either one alone still pins the model for next time.
	var wsErr, usrErr error
	if cfg != nil {
		cfg.PinnedModel, cfg.PinnedWindow = seed, w
		wsErr = cfg.Save()
	} else {
		wsErr = errors.New("no workspace config")
	}
	usrErr = SaveUserPin(seed, w)
	if wsErr != nil && usrErr != nil {
		warn = fmt.Errorf("could not record the model pin (workspace: %v; user: %v) — "+
			"this session runs on %s, but the next one will seed again", wsErr, usrErr, seed)
	}
	return seed, w, warn
}

// The DELEGATED pin lived here briefly and moved to internal/policy as the
// target agent.delegated. It was never released, so there is no compatibility
// layer: a model chain that ends at the primary pin is now expressed as schema
// (Schema.InheritsPrimaryModel) rather than as a second hand-written chain.
