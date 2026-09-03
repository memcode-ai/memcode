package config

import (
	"testing"

	"github.com/memcode-ai/memcode/catalog"
)

// (1) No delegated preference → delegated work inherits the primary pin.
// This is the default, and it is why adding this feature changes nothing for
// anyone who does not ask for it.
func TestNoDelegatedPreferenceInheritsPrimary(t *testing.T) {
	cfg := newCfg(t)
	primary, win := ResolvePin(cfg, "")

	got, gotWin := ResolveDelegatedPin(cfg, "", primary, win)
	if got != primary || gotWin != win {
		t.Fatalf("delegated = %q/%d, want the primary %q/%d", got, gotWin, primary, win)
	}
	// And nothing was written: "unset" must stay unset, never seeded.
	if cfg.DelegatedModel != "" {
		t.Errorf("resolving must not seed a delegated pin, got %q", cfg.DelegatedModel)
	}
	if p := loadUserPrefs(); p.DelegatedModel != "" {
		t.Errorf("resolving must not seed the user store, got %q", p.DelegatedModel)
	}
}

// (3) The preference survives at the scope it was set at, and (4) changing the
// primary does not overwrite it.
func TestDelegatedPinPersistsPerScopeAndSurvivesPrimaryChanges(t *testing.T) {
	for _, scope := range []string{"workspace", "user"} {
		t.Run(scope, func(t *testing.T) {
			cfg := newCfg(t)
			primary, win := ResolvePin(cfg, "")

			if err := SetDelegatedPin(cfg, scope, "haiku", 200000); err != nil {
				t.Fatalf("SetDelegatedPin: %v", err)
			}
			if got, _ := ResolveDelegatedPin(cfg, "", primary, win); got != "haiku" {
				t.Fatalf("delegated = %q, want haiku", got)
			}
			// It landed in the right store, and only that one.
			switch scope {
			case "workspace":
				if cfg.DelegatedModel != "haiku" {
					t.Errorf("workspace scope must write the project config, got %q", cfg.DelegatedModel)
				}
			case "user":
				if p := loadUserPrefs(); p.DelegatedModel != "haiku" {
					t.Errorf("user scope must write the user store, got %q", p.DelegatedModel)
				}
			}

			// (4) Changing the PRIMARY must not disturb an explicit delegated pin.
			cfg.PinnedModel, cfg.PinnedWindow = "opus", 1_000_000
			if err := cfg.Save(); err != nil {
				t.Fatal(err)
			}
			SaveUserPin("opus", 1_000_000)
			if got, _ := ResolveDelegatedPin(cfg, "", "opus", 1_000_000); got != "haiku" {
				t.Fatalf("after a primary change delegated = %q, want haiku still", got)
			}
		})
	}
}

// (5) Resetting to inherit restores primary-pin behaviour.
func TestDelegatedPinResetRestoresInherit(t *testing.T) {
	cfg := newCfg(t)
	primary, win := ResolvePin(cfg, "")
	if err := SetDelegatedPin(cfg, "workspace", "haiku", 200000); err != nil {
		t.Fatal(err)
	}
	if err := SetDelegatedPin(cfg, "workspace", "", 0); err != nil {
		t.Fatal(err)
	}
	if got, gotWin := ResolveDelegatedPin(cfg, "", primary, win); got != primary || gotWin != win {
		t.Fatalf("after reset delegated = %q/%d, want the primary %q/%d", got, gotWin, primary, win)
	}
}

// A user-scope write clears the workspace value, or the workspace would keep
// shadowing the choice the person just made "from now on".
func TestUserScopeClearsAShadowingWorkspacePin(t *testing.T) {
	cfg := newCfg(t)
	if err := SetDelegatedPin(cfg, "workspace", "haiku", 200000); err != nil {
		t.Fatal(err)
	}
	if err := SetDelegatedPin(cfg, "user", "sonnet", 1_000_000); err != nil {
		t.Fatal(err)
	}
	if cfg.DelegatedModel != "" {
		t.Errorf("workspace pin should be cleared, got %q", cfg.DelegatedModel)
	}
	if got, _ := ResolveDelegatedPin(cfg, "", "opus", 0); got != "sonnet" {
		t.Fatalf("delegated = %q, want the user-scope sonnet", got)
	}
}

// The session override wins over both stores and is never written down.
func TestDelegatedSessionOverrideBeatsStoresAndDoesNotPersist(t *testing.T) {
	cfg := newCfg(t)
	if err := SetDelegatedPin(cfg, "workspace", "haiku", 200000); err != nil {
		t.Fatal(err)
	}
	if got, _ := ResolveDelegatedPin(cfg, "sonnet", "opus", 0); got != "sonnet" {
		t.Fatalf("override = %q, want sonnet", got)
	}
	if cfg.DelegatedModel != "haiku" {
		t.Errorf("an override must not rewrite the stored pin, got %q", cfg.DelegatedModel)
	}
}

// The delegated pin never consults default_model. The seed belongs to the
// primary alone; seeding here would split models for people who never asked.
func TestDelegatedPinNeverSeedsFromTheCatalogDefault(t *testing.T) {
	cfg := newCfg(t)
	got, _ := ResolveDelegatedPin(cfg, "", "opus", 1_000_000)
	if got == catalog.DefaultModel() && got != "opus" {
		t.Fatalf("delegated resolved to the catalog seed %q — it must inherit the primary instead", got)
	}
	if got != "opus" {
		t.Fatalf("delegated = %q, want the primary opus", got)
	}
}
