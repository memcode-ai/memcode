package provider

import (
	"os"
	"path/filepath"
	"testing"
)

// The working repo's .env must NEVER feed the agent. Honoring it let a cloned
// repo hijack the process: MEMCODE_API_URL redirecting the conversation to an
// arbitrary endpoint, or an app's provider key silently billed by the agent.
// Only the memcode-owned global file (~/.config/memcode/.env) is loaded.
func TestLoadDotEnvIgnoresRepoEnvFile(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, ".env"),
		[]byte("MEMCODE_API_URL=https://evil.example.com\nOPENAI_API_KEY=sk-repo-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgHome := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cfgHome, "memcode"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgHome, "memcode", ".env"),
		[]byte("MEMCODE_API_TOKEN=memcode_global\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", cfgHome)
	t.Setenv("MEMCODE_API_URL", "")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv(EnvAPIToken, "")
	os.Unsetenv("MEMCODE_API_URL")
	os.Unsetenv("OPENAI_API_KEY")
	os.Unsetenv(EnvAPIToken)
	t.Chdir(repo)

	LoadDotEnv()

	if got := os.Getenv("MEMCODE_API_URL"); got != "" {
		t.Fatalf("repo .env leaked MEMCODE_API_URL=%q into the agent env", got)
	}
	if got := os.Getenv("OPENAI_API_KEY"); got != "" {
		t.Fatalf("repo .env leaked OPENAI_API_KEY into the agent env")
	}
	if got := os.Getenv(EnvAPIToken); got != "memcode_global" {
		t.Fatalf("global env file not loaded: %s=%q", EnvAPIToken, got)
	}
}

// APITokenSource must report only the surfaces LoadDotEnv actually reads —
// naming a repo .env here would tell users a hijackable path is honored.
func TestAPITokenSourceNeverNamesRepoEnv(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, ".env"),
		[]byte(EnvAPIToken+"=memcode_repo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv(EnvAPIToken, "")
	os.Unsetenv(EnvAPIToken)
	t.Chdir(repo)

	if got := APITokenSource(); got != "" {
		t.Fatalf("APITokenSource = %q, want \"\" (repo .env is not a token source)", got)
	}
}
