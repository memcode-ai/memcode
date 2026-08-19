package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The v0.25.x era persisted subscription sessions as config ENDPOINT entries
// (claude-sub → api.anthropic.com). Loading such a config must neither serve
// exclusively on it nor keep it on disk — this is the regression that put
// updated installs right back on the kimi-k3-404 bug.
func TestSubscriptionEndpointArtifactPruned(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, DirName)
	if err := os.MkdirAll(filepath.Join(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	raw := `{"models":{"coder":"auto","planner":"auto","explorer":"auto"},"endpoints":[{"name":"claude-sub","base_url":"https://api.anthropic.com","last_model":"kimi-k3"},{"name":"ollama","base_url":"http://localhost:11434/v1"}]}`
	if err := os.WriteFile(filepath.Join(dir, ConfigFile), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	// Resolve() walks to the git toplevel; make the temp root one.
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	t.Logf("endpoints after load: %+v", cfg.Endpoints)
	for _, e := range cfg.Endpoints {
		if e.Name == "claude-sub" {
			t.Fatal("subscription artifact survived Load")
		}
	}
	if len(cfg.Endpoints) != 1 || cfg.Endpoints[0].Name != "ollama" {
		t.Fatalf("real endpoints must survive, got %+v", cfg.Endpoints)
	}
	// The REAL custom endpoint still resolves (first entry = default active);
	// only the subscription artifact is invisible.
	if ep, ok := cfg.ResolveEndpoint(); !ok || ep.Name != "ollama" {
		t.Fatalf("ResolveEndpoint = %+v ok=%v, want ollama", ep, ok)
	}

	// The prune persisted: a re-read of the raw file has no claude-sub.
	b, _ := os.ReadFile(filepath.Join(dir, ConfigFile))
	var onDisk map[string]any
	_ = json.Unmarshal(b, &onDisk)
	if s := string(b); len(s) > 0 && (json.Valid(b) && (stringContains(s, "claude-sub"))) {
		t.Fatal("artifact still on disk after Load")
	}
}

func stringContains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

func TestArtifactOnlyConfigResolvesNothing(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, DirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	raw := `{"models":{"coder":"auto","planner":"auto","explorer":"auto"},"endpoints":[{"name":"claude-sub","base_url":"https://api.anthropic.com","last_model":"kimi-k3"}]}`
	if err := os.WriteFile(filepath.Join(dir, ConfigFile), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if ep, ok := cfg.ResolveEndpoint(); ok {
		t.Fatalf("artifact-only config resolved endpoint %+v — the session would be exclusive again", ep)
	}
}
