// Package secrets keeps the agent from leaking credentials. The agent may USE
// secrets (commands inherit the process environment, including values loaded
// from .env) but their values must never reach the model, the event log, or the
// terminal. Two defenses: refuse to read secret-bearing files, and redact known
// secret values from every tool result and printed line.
package secrets

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	minSecretLen = 8
	placeholder  = "***REDACTED***"
)

// Env var names whose values are treated as secrets.
var sensitiveName = regexp.MustCompile(`(?i)(secret|password|passwd|token|api[_-]?key|access[_-]?key|private[_-]?key|credential|_pat\b|auth)`)

// Redactor replaces known secret values with a placeholder.
type Redactor struct{ values []string }

// NewFromEnv collects secret values from the current environment (which, for the
// agent, already includes anything loaded from .env).
func NewFromEnv() *Redactor {
	r := &Redactor{}
	for _, kv := range os.Environ() {
		k, v, ok := strings.Cut(kv, "=")
		if ok && len(v) >= minSecretLen && sensitiveName.MatchString(k) {
			r.values = append(r.values, v)
		}
	}
	return r
}

// Add registers extra secret values to redact.
func (r *Redactor) Add(values ...string) {
	for _, v := range values {
		if len(v) >= minSecretLen {
			r.values = append(r.values, v)
		}
	}
}

// Redact replaces every known secret value found in s with the placeholder.
func (r *Redactor) Redact(s string) string {
	if r == nil {
		return s
	}
	for _, v := range r.values {
		if v != "" && strings.Contains(s, v) {
			s = strings.ReplaceAll(s, v, placeholder)
		}
	}
	return s
}

var secretFile = regexp.MustCompile(`(?i)(^|/)(([^/]*\.)?env(\.[^/]*)?|\.netrc|\.npmrc|\.pypirc|\.git-credentials|credentials|id_rsa|id_ed25519|id_dsa|.+\.pem|.+\.key|.+\.p12|.+\.pfx|secrets?\.(json|ya?ml|toml|env))$`)

// IsSecretPath reports whether a path likely holds credentials, so its VALUES
// should be masked when read or diffed (the file is still usable — the agent
// just operates on it by reference, never seeing the plaintext).
func IsSecretPath(path string) bool {
	p := filepath.ToSlash(path)
	if secretFile.MatchString(p) {
		return true
	}
	return strings.Contains(p, ".aws/credentials") || strings.Contains(p, ".ssh/") ||
		strings.Contains(p, ".gnupg/")
}

// Mask the value of KEY=VALUE / KEY: VALUE lines (tolerating a leading diff
// marker), and the body of PEM blocks. The agent sees which keys exist, never
// their values.
var (
	reKeyValue = regexp.MustCompile(`(?m)^([+\- ]?\s*(?:export\s+)?[A-Za-z_][\w.\-]*\s*[=:]\s*)\S.*$`)
	rePEMBody  = regexp.MustCompile(`(?s)(-----BEGIN [^-]+-----).*?(-----END [^-]+-----)`)
)

// RedactSecretFile masks the values in a credential-bearing file (or a diff of
// one), preserving structure so the agent can still reason about and edit it.
func RedactSecretFile(content string) string {
	content = rePEMBody.ReplaceAllString(content, "$1\n"+placeholder+"\n$2")
	content = reKeyValue.ReplaceAllString(content, "$1"+placeholder)
	return content
}
