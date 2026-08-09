package provider

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// LoadDotEnv loads KEY=VALUE pairs into the process environment WITHOUT
// overriding variables already set, from the user-global file only
// (GlobalEnvPath, e.g. ~/.config/memcode/.env).
//
// The working repo's .env is DELIBERATELY not read. That file belongs to the
// project, not the agent, and honoring it meant a cloned repo could silently
// hijack the agent: its MEMCODE_API_URL/MEMCODE_ENDPOINT_URL would redirect
// the whole conversation to an arbitrary endpoint, and an app's OPENAI_API_KEY
// meant for the app would get picked up and billed by the agent. Credentials
// come from the exported environment or the memcode-owned global file, never
// from the repo being worked on.
//
// Precedence: real exported env > global file. Best-effort: a missing file is
// not an error. Secrets live in the gitignored global env file, never in
// .memcode config or the database.
func LoadDotEnv() {
	loadEnvFile(GlobalEnvPath())
}

// GlobalEnvPath returns the user-level secrets file: $XDG_CONFIG_HOME/memcode/.env
// if XDG_CONFIG_HOME is set, otherwise ~/.config/memcode/.env. Returns "" if no
// home directory can be determined.
func GlobalEnvPath() string {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "memcode", ".env")
}

// APITokenSource reports where the gateway token resolves from, for diagnostics
// (e.g. `memcode doctor`) — "environment" or the global config path — or ""
// if none is found. Call it BEFORE LoadDotEnv so an already-loaded file isn't
// misreported as the process environment.
func APITokenSource() string {
	if os.Getenv(EnvAPIToken) != "" {
		return "environment"
	}
	if g := GlobalEnvPath(); g != "" && fileHasKey(g, EnvAPIToken) {
		return g
	}
	return ""
}

// fileHasKey reports whether path defines a non-empty value for key.
func fileHasKey(path, key string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimPrefix(strings.TrimSpace(sc.Text()), "export ")
		k, v, ok := strings.Cut(line, "=")
		if ok && strings.TrimSpace(k) == key && strings.Trim(strings.TrimSpace(v), `"'`) != "" {
			return true
		}
	}
	return false
}

// loadEnvFile loads one KEY=VALUE file into the environment without overriding
// already-set variables. A missing/unreadable file is silently ignored.
func loadEnvFile(path string) {
	if path == "" {
		return
	}
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.Trim(strings.TrimSpace(val), `"'`)
		if key == "" {
			continue
		}
		if _, exists := os.LookupEnv(key); !exists {
			_ = os.Setenv(key, val)
		}
	}
}
