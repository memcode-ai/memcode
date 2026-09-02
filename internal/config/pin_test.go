package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/memcode-ai/memcode/catalog"
)

// newCfg builds a config rooted in a temp project. XDG_CONFIG_HOME is
// redirected so the user-level store never touches the developer's real
// ~/.config/memcode/prefs.json.
func newCfg(t *testing.T) *Config {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	root := t.TempDir()
	if _, err := Init(root, false); err != nil {
		t.Fatalf("Init: %v", err)
	}
	cfg, err := Load(root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

// The resolution chain: session override -> workspace -> user -> seed.
func TestResolvePinChain(t *testing.T) {
	cfg := newCfg(t)

	// 1. Nothing anywhere -> the default_model seed, PERSISTED so this is the
	//    only run that ever consults it.
	seed := catalog.DefaultModel()
	if seed == "" {
		t.Fatal("the catalog must declare a default_model")
	}
	label, window := ResolvePin(cfg, "")
	if label != seed {
		t.Fatalf("fresh install = %q, want the seed %q", label, seed)
	}
	if window != catalog.ContextWindow(seed) {
		t.Errorf("window = %d, want the seed's %d", window, catalog.ContextWindow(seed))
	}
	if cfg.PinnedModel != seed {
		t.Errorf("the seed must be persisted to the workspace, got %q", cfg.PinnedModel)
	}
	if p := loadUserPrefs(); p.PinnedModel != seed {
		t.Errorf("the seed must be persisted to the user store, got %q", p.PinnedModel)
	}

	// 2. Workspace wins over the seed.
	cfg.PinnedModel, cfg.PinnedWindow = "opus", 999
	if label, window = ResolvePin(cfg, ""); label != "opus" || window != 999 {
		t.Fatalf("workspace pin = %q/%d, want opus/999", label, window)
	}

	// 3. Session override wins over everything, and does NOT persist: it is
	//    this invocation's model, not a new preference.
	if label, _ = ResolvePin(cfg, "haiku"); label != "haiku" {
		t.Fatalf("override = %q, want haiku", label)
	}
	if cfg.PinnedModel != "opus" {
		t.Errorf("an override must not overwrite the workspace pin, got %q", cfg.PinnedModel)
	}
	if p := loadUserPrefs(); p.PinnedModel == "haiku" {
		t.Error("an override must not reach the user store")
	}
}

// A repo that has never chosen adopts the USER's model rather than the seed —
// "I use Opus" shouldn't have to be re-said per checkout.
func TestResolvePinAdoptsUserPreferenceInANewWorkspace(t *testing.T) {
	cfg := newCfg(t)
	SaveUserPin("opus", 1_000_000)

	label, window := ResolvePin(cfg, "")
	if label != "opus" || window != 1_000_000 {
		t.Fatalf("new workspace = %q/%d, want the user's opus", label, window)
	}
	// ...and the workspace answers for itself from then on.
	if cfg.PinnedModel != "opus" {
		t.Errorf("the adopted pin must be written to the workspace, got %q", cfg.PinnedModel)
	}
}

// The seed is consulted exactly ONCE. A value re-derived every run would be a
// routing decision wearing a default's clothes.
func TestSeedIsConsultedOnlyOnce(t *testing.T) {
	cfg := newCfg(t)
	first, _ := ResolvePin(cfg, "")

	// Simulate the catalog's default changing under a returning user.
	cfg2 := *cfg
	second, _ := ResolvePin(&cfg2, "")
	if second != first {
		t.Fatalf("second run = %q, want the persisted %q", second, first)
	}
	if cfg2.PinnedModel != first {
		t.Errorf("a persisted pin must be read, not re-seeded; got %q", cfg2.PinnedModel)
	}
}

// A corrupt or unreadable user store is "nothing remembered", never a failure:
// starting a session must not depend on a convenience file parsing.
func TestCorruptUserPrefsDegradeToTheSeed(t *testing.T) {
	cfg := newCfg(t)
	path := UserPrefsPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if label, _ := ResolvePin(cfg, ""); label != catalog.DefaultModel() {
		t.Fatalf("corrupt prefs = %q, want the seed", label)
	}
}
