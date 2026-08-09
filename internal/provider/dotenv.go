package provider

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// LoadDotEnv loads KEY=VALUE pairs into the process environment WITHOUT
// overriding variables already set, from (in order): the repo's <root>/.env,
// then a user-global file (GlobalEnvPath, e.g. ~/.config/memcode/.env).
//
// Precedence is therefore: real exported env > repo .env > global. So a key set
// once in the global file "just works" in every repository, while a repo-local
// .env (or an explicit export) can still override it. Best-effort: missing files
// are not an error. Secrets live in these gitignored env files, never in
// .memcode config or the database.
func LoadDotEnv(root string) {
	loadEnvFile(filepath.Join(root, ".env"))
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
// (e.g. `memcode doctor`) — "environment", a repo .env path, or the global
// config path — or "" if none is found. Call it BEFORE LoadDotEnv so an
// already-loaded file isn't misreported as the process environment.
func APITokenSource(root string) string {
	if os.Getenv(EnvAPIToken) != "" {
		return "environment"
	}
	if p := filepath.Join(root, ".env"); fileHasKey(p, EnvAPIToken) {
		return p
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
