package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/memcode-ai/memcode/internal/provider"
)

// Init makes .memcode self-ignoring via an in-dir `*` .gitignore (no `!.gitignore`
// exception — that would leave the dir visible in git status), without touching the
// user's root .gitignore.
func TestInitWritesSelfGitignore(t *testing.T) {
	dir := t.TempDir()
	if _, err := Init(dir, false); err != nil {
		t.Fatalf("Init: %v", err)
	}
	gi := filepath.Join(dir, DirName, ".gitignore")
	body, err := os.ReadFile(gi)
	if err != nil {
		t.Fatalf("expected %s: %v", gi, err)
	}
	if !strings.Contains(string(body), "*") || strings.Contains(string(body), "!.gitignore") {
		t.Errorf("self-ignore must be bare `*` so .memcode fully disappears from git status; got:\n%s", body)
	}
	// The user's ROOT .gitignore must NOT be created/modified.
	if _, err := os.Stat(filepath.Join(dir, ".gitignore")); !os.IsNotExist(err) {
		t.Error("must not touch the user's root .gitignore")
	}
}

// The real bug: an ALREADY-INITIALIZED project (Init is never called on launch)
// must still get the self-ignore. EnsureGitignore is called directly by openProject
// on every launch, independent of Init.
func TestEnsureGitignoreWithoutInit(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, DirName), 0o755); err != nil { // .memcode already exists
		t.Fatal(err)
	}
	EnsureGitignore(dir) // no Init — simulates an existing project's launch
	body, err := os.ReadFile(filepath.Join(dir, DirName, ".gitignore"))
	if err != nil {
		t.Fatalf("EnsureGitignore must write .memcode/.gitignore on an existing project: %v", err)
	}
	if !strings.Contains(string(body), "*") || strings.Contains(string(body), "!.gitignore") {
		t.Errorf("want bare `*`, got:\n%s", body)
	}
}

// The earlier `*`+`!.gitignore` template (which didn't hide the dir) is migrated.
func TestInitMigratesOldGitignore(t *testing.T) {
	dir := t.TempDir()
	md := filepath.Join(dir, DirName)
	if err := os.MkdirAll(md, 0o755); err != nil {
		t.Fatal(err)
	}
	old := "# memcode's local state — not part of your repo\n*\n!.gitignore\n"
	if err := os.WriteFile(filepath.Join(md, ".gitignore"), []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Init(dir, false); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(filepath.Join(md, ".gitignore"))
	if strings.Contains(string(body), "!.gitignore") {
		t.Errorf("the old template should have been migrated to bare `*`, got:\n%s", body)
	}

	// A user-customized .gitignore must NOT be clobbered.
	custom := "*\n!keep-this-file\n"
	if err := os.WriteFile(filepath.Join(md, ".gitignore"), []byte(custom), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Init(dir, false); err != nil {
		t.Fatal(err)
	}
	if body, _ := os.ReadFile(filepath.Join(md, ".gitignore")); string(body) != custom {
		t.Errorf("must not clobber a user-edited .gitignore; got:\n%s", body)
	}
}

func TestInitAndLoad(t *testing.T) {
	dir := t.TempDir()

	created, err := Init(dir, false)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if !created {
		t.Fatal("expected config to be created on first init")
	}

	// Second init without force should be a no-op.
	created, err = Init(dir, false)
	if err != nil {
		t.Fatalf("Init (second): %v", err)
	}
	if created {
		t.Fatal("expected no-op on second init without force")
	}

	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Models.Coder == "" {
		t.Error("expected a default coder model")
	}
	if len(cfg.Exclude) == 0 {
		t.Error("expected default exclude patterns")
	}
	if cfg.Root != dir {
		t.Errorf("Root = %q, want %q", cfg.Root, dir)
	}
}

func TestLoadWalksUp(t *testing.T) {
	dir := t.TempDir()
	if _, err := Init(dir, false); err != nil {
		t.Fatalf("Init: %v", err)
	}

	nested := filepath.Join(dir, "a", "b", "c")
	cfg, err := Load(nested)
	if err != nil {
		t.Fatalf("Load from nested dir: %v", err)
	}
	if cfg.Root != dir {
		t.Errorf("Root = %q, want %q", cfg.Root, dir)
	}
}

func TestLoadNotInitialized(t *testing.T) {
	if _, err := Load(t.TempDir()); err == nil {
		t.Fatal("expected error when not initialized")
	}
}

// TestModePersists: a saved permission mode round-trips through config so the
// choice survives across sessions.
func TestModePersists(t *testing.T) {
	dir := t.TempDir()
	if _, err := Init(dir, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Mode != "" {
		t.Errorf("fresh config should have no saved mode, got %q", cfg.Mode)
	}
	cfg.Mode = "allow-all"
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	reloaded, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Mode != "allow-all" {
		t.Errorf("mode did not persist: got %q, want allow-all", reloaded.Mode)
	}
}

// TestThemePersists: a saved theme name round-trips through config so the
// choice survives across sessions.
func TestThemePersists(t *testing.T) {
	dir := t.TempDir()
	if _, err := Init(dir, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Theme != "" {
		t.Errorf("fresh config should have no saved theme, got %q", cfg.Theme)
	}
	cfg.Theme = "dracula"
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	reloaded, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Theme != "dracula" {
		t.Errorf("theme did not persist: got %q, want dracula", reloaded.Theme)
	}
}

// TestThemeEmptyPersists: an empty theme (the default) round-trips cleanly.
func TestThemeEmptyPersists(t *testing.T) {
	dir := t.TempDir()
	if _, err := Init(dir, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Theme = ""
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	reloaded, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Theme != "" {
		t.Errorf("empty theme should remain empty after round-trip, got %q", reloaded.Theme)
	}
}

// clearEndpointEnv strips the endpoint env inputs so each case is explicit.
func clearEndpointEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{provider.EnvEndpointURL, provider.EnvEndpointKey, provider.EnvEndpointModel, "OLLAMA_TEST_KEY"} {
		t.Setenv(k, "")
	}
}

// TestEndpointsPersistAndSelect: the named endpoint list round-trips through
// config.json (no key material — KeyEnv only), and ActiveEndpoint honors the
// selection ("" = first, dangling name = none).
func TestEndpointsPersistAndSelect(t *testing.T) {
	dir := t.TempDir()
	if _, err := Init(dir, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Endpoints = []Endpoint{
		{Name: "ollama", BaseURL: "http://localhost:11434/v1", Models: []string{"qwen3:4b"}},
		{Name: "lab", BaseURL: "http://lab:8000/v1", KeyEnv: "OLLAMA_TEST_KEY", LastModel: "glm-local"},
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	cfg, err = Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Endpoints) != 2 || cfg.Endpoints[1].KeyEnv != "OLLAMA_TEST_KEY" || cfg.Endpoints[1].LastModel != "glm-local" {
		t.Fatalf("endpoints did not round-trip: %+v", cfg.Endpoints)
	}
	if ep, ok := cfg.ActiveEndpoint(); !ok || ep.Name != "ollama" {
		t.Errorf("empty selection must pick the first entry, got %+v ok=%v", ep, ok)
	}
	cfg.Endpoint = "lab"
	if ep, ok := cfg.ActiveEndpoint(); !ok || ep.Name != "lab" {
		t.Errorf("named selection wrong: %+v ok=%v", ep, ok)
	}
	cfg.Endpoint = "typo"
	if _, ok := cfg.ActiveEndpoint(); ok {
		t.Error("a dangling endpoint name must select NOTHING, never a different backend")
	}
}

// TestResolveEndpointPrecedence: env base URL wins over the config selection;
// a remembered last-model beats the env INITIAL model; the entry's KeyEnv
// fills a missing env key; the curated list seeds the model when nothing else
// names one.
func TestResolveEndpointPrecedence(t *testing.T) {
	clearEndpointEnv(t)
	cfg := &Config{Endpoints: []Endpoint{
		{Name: "ollama", BaseURL: "http://localhost:11434/v1", KeyEnv: "OLLAMA_TEST_KEY", LastModel: "remembered:model", Models: []string{"curated:one"}},
		{Name: "lab", BaseURL: "http://lab:8000/v1"},
	}}

	// Config-only: the selected entry, key from its env var, remembered model.
	t.Setenv("OLLAMA_TEST_KEY", "sk-from-env-var")
	ep, ok := cfg.ResolveEndpoint()
	if !ok || ep.Name != "ollama" || ep.Key != "sk-from-env-var" || ep.Model != "remembered:model" {
		t.Fatalf("config resolve wrong: %+v ok=%v", ep, ok)
	}

	// Env URL matching an entry: enriched with its memory/allowlist/key.
	t.Setenv(provider.EnvEndpointURL, "http://localhost:11434/v1")
	t.Setenv(provider.EnvEndpointModel, "env-initial:model")
	ep, ok = cfg.ResolveEndpoint()
	if !ok || ep.Model != "remembered:model" {
		t.Errorf("a remembered /model choice must beat the env INITIAL model, got %+v", ep)
	}
	if len(ep.Models) != 1 || ep.Models[0] != "curated:one" {
		t.Errorf("the matching entry's curated list must ride along, got %+v", ep.Models)
	}

	// Env URL with no matching entry: env values verbatim.
	t.Setenv(provider.EnvEndpointURL, "http://elsewhere:9000/v1")
	ep, ok = cfg.ResolveEndpoint()
	if !ok || ep.Model != "env-initial:model" || ep.Name != "elsewhere:9000" {
		t.Errorf("unmatched env endpoint wrong: %+v", ep)
	}

	// No env, entry without LastModel: first curated model seeds the session.
	clearEndpointEnv(t)
	cfg2 := &Config{Endpoints: []Endpoint{{Name: "lab", BaseURL: "http://lab:8000/v1", Models: []string{"a-model", "b-model"}}}}
	if ep, ok := cfg2.ResolveEndpoint(); !ok || ep.Model != "a-model" {
		t.Errorf("curated list must seed the initial model, got %+v", ep)
	}

	// Nothing configured anywhere.
	if _, ok := (&Config{}).ResolveEndpoint(); ok {
		t.Error("no env, no entries must resolve to nothing")
	}
}

// TestRememberEndpointModel: a /model choice lands on the matching entry (by
// base URL, slash-insensitive) and an env-defined endpoint gets an entry
// appended so the choice survives relaunches.
func TestRememberEndpointModel(t *testing.T) {
	cfg := &Config{Endpoints: []Endpoint{{Name: "ollama", BaseURL: "http://localhost:11434/v1/"}}}
	cfg.RememberEndpointModel(provider.Endpoint{Name: "ollama", BaseURL: "http://localhost:11434/v1"}, "qwen3:4b")
	if cfg.Endpoints[0].LastModel != "qwen3:4b" || len(cfg.Endpoints) != 1 {
		t.Fatalf("matching entry must be updated in place: %+v", cfg.Endpoints)
	}
	cfg.RememberEndpointModel(provider.Endpoint{BaseURL: "http://other:8000/v1"}, "m2")
	if len(cfg.Endpoints) != 2 || cfg.Endpoints[1].LastModel != "m2" || cfg.Endpoints[1].Name != "other:8000" {
		t.Fatalf("env endpoint must gain an entry: %+v", cfg.Endpoints)
	}
}

// A non-git directory under $HOME must NOT adopt $HOME as its project root just
// because ~/.memcode (memcode's user-global state dir) exists. Doing so pointed
// state and every repo-wide walk at the user's whole home tree.
func TestResolveNeverAdoptsHomeAsRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, DirName), 0o755); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(home, "code", "not-a-repo")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	root, src, err := Resolve(work)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if root != work || src != SourceCWD {
		t.Errorf("Resolve = %q (%s), want %q (%s)", root, src, work, SourceCWD)
	}
}

// A real project .memcode below home is still found by walking up.
func TestResolveFindsProjectBelowHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	proj := filepath.Join(home, "code", "proj")
	if err := os.MkdirAll(filepath.Join(proj, DirName), 0o755); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(proj, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	root, src, err := Resolve(sub)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if root != proj || src != SourceExisting {
		t.Errorf("Resolve = %q (%s), want %q (%s)", root, src, proj, SourceExisting)
	}
}
