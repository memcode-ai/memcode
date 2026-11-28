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
	"sort"
	"strings"
)

const (
	minSecretLen = 8
	placeholder  = "***REDACTED***"
)

// Env var names whose values are treated as secrets: the usual words, any
// *_KEY (MEMCODE_ENDPOINT_KEY, VLLM_KEY), service-account blobs, and connection
// strings that carry a password (DATABASE_URL, *_DSN).
var sensitiveName = regexp.MustCompile(`(?i)(secret|password|passwd|token|api[_-]?key|access[_-]?key|private[_-]?key|credential|_pat\b|auth|(^|_)key$|service[_-]?account|(^|_)dsn$|database_url|_sk$)`)

// SensitiveName reports whether an environment variable's NAME marks its value
// as a secret.
func SensitiveName(name string) bool { return sensitiveName.MatchString(name) }

// tokenShape matches credentials by their published formats, so a key is masked
// even when it never passed through the environment (read from a file, echoed
// by a CLI): Anthropic/OpenAI sk-, GitHub ghp_/gho_/ghs_/github_pat_, AWS
// access key ids, Slack xox*, Google AIza, and PEM private-key bodies.
var tokenShape = regexp.MustCompile(`\b(sk-(?:ant-)?[A-Za-z0-9_\-]{20,}|gh[pousr]_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{40,}|AKIA[0-9A-Z]{16}|xox[abposr]-[A-Za-z0-9\-]{10,}|AIza[0-9A-Za-z_\-]{35})`)

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
	r.sortByLen()
	return r
}

// Add registers extra secret values to redact.
func (r *Redactor) Add(values ...string) {
	for _, v := range values {
		if len(v) >= minSecretLen {
			r.values = append(r.values, v)
		}
	}
	r.sortByLen()
}

// sortByLen keeps values longest-first, so a secret that is a SUBSTRING of
// another (e.g. a token and the same token with a suffix) never masks the
// shorter one first and leaves the longer one's tail unredacted.
func (r *Redactor) sortByLen() {
	sort.SliceStable(r.values, func(i, j int) bool { return len(r.values[i]) > len(r.values[j]) })
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
	return tokenShape.ReplaceAllString(s, placeholder)
}

// ownCredentials are memcode's own credentials. User commands, hooks, MCP
// servers and language servers never need them, so they are not passed down:
// a prompt-injected `curl … $MEMCODE_API_TOKEN` or a hostile MCP package then
// has nothing to read.
var ownCredentials = []string{"MEMCODE_API_TOKEN", "MEMCODE_ENDPOINT_KEY"}

// ChildEnv returns env (os.Environ() when nil) without memcode's own
// credentials, for every process memcode starts on the user's or model's
// behalf.
func ChildEnv(env []string) []string {
	if env == nil {
		env = os.Environ()
	}
	out := make([]string, 0, len(env))
next:
	for _, kv := range env {
		for _, k := range ownCredentials {
			if strings.HasPrefix(kv, k+"=") {
				continue next
			}
		}
		out = append(out, kv)
	}
	return out
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
	for _, frag := range credentialPaths {
		if strings.Contains(p, frag) {
			return true
		}
	}
	return false
}

// credentialPaths are where common tools keep logins: cloud CLIs, package
// registries, container and cluster configs, and the OAuth files of the
// assistants memcode can ride on (Claude Code, Codex, GitHub CLI, memcode's own).
var credentialPaths = []string{
	".aws/credentials", ".ssh/", ".gnupg/",
	".claude/.credentials.json", ".codex/auth.json", ".config/gh/hosts.yml",
	".config/memcode/", ".memcode/.env", ".docker/config.json", ".kube/config",
	".config/gcloud/", ".azure/", ".terraform.d/credentials", ".cargo/credentials",
	".gem/credentials", ".config/hub",
}

// Mask the value of KEY=VALUE / KEY: VALUE lines (tolerating a leading diff
// marker), and the body of PEM blocks. The agent sees which keys exist, never
// their values.
var (
	reKeyValue = regexp.MustCompile(`(?m)^([+\- ]?\s*(?:export\s+)?[A-Za-z_][\w.\-]*\s*[=:]\s*)\S.*$`)
	// JSON "name": "value" pairs (credential files are often JSON): the string
	// value is masked, numbers/booleans/structure stay.
	reJSONString = regexp.MustCompile(`("(?:[^"\\]|\\.)*"\s*:\s*)"(?:[^"\\]|\\.)*"`)
	rePEMBody    = regexp.MustCompile(`(?s)(-----BEGIN [^-]+-----).*?(-----END [^-]+-----)`)
)

// RedactSecretFile masks the values in a credential-bearing file (or a diff of
// one), preserving structure so the agent can still reason about and edit it.
func RedactSecretFile(content string) string {
	content = rePEMBody.ReplaceAllString(content, "$1\n"+placeholder+"\n$2")
	content = reJSONString.ReplaceAllString(content, `$1"`+placeholder+`"`)
	content = reKeyValue.ReplaceAllString(content, "$1"+placeholder)
	return tokenShape.ReplaceAllString(content, placeholder)
}

// RedactSecretGrep masks matched lines that come from credential files in
// grep/rg output ("path:line:text", or "line:text" when one file was searched).
// searched is the path the search was scoped to.
func RedactSecretGrep(out, searched string) string {
	whole := searched != "" && IsSecretPath(searched)
	lines := strings.Split(out, "\n")
	for i, ln := range lines {
		file, rest, ok := strings.Cut(ln, ":")
		if !ok {
			continue
		}
		switch {
		case whole:
			lines[i] = file + ":" + maskLine(rest) // "N:text" — file is the line number
		case IsSecretPath(file):
			num, text, ok := strings.Cut(rest, ":")
			if ok {
				lines[i] = file + ":" + num + ":" + maskLine(text)
			} else {
				lines[i] = file + ":" + maskLine(rest)
			}
		}
	}
	return strings.Join(lines, "\n")
}

// maskLine masks one line from a credential file: its value when it reads as
// key=value / key: value / "key": "value", otherwise the whole line.
func maskLine(text string) string {
	if m := RedactSecretFile(text); m != text {
		return m
	}
	if strings.TrimSpace(text) == "" {
		return text
	}
	return placeholder
}

// RedactSecretDiff masks the changed and context lines of every credential
// file in a unified diff, leaving headers and other files intact.
func RedactSecretDiff(diff string) string {
	lines := strings.Split(diff, "\n")
	secret := false
	for i, ln := range lines {
		if rest, ok := strings.CutPrefix(ln, "diff --git "); ok {
			a, b, _ := strings.Cut(rest, " ")
			secret = IsSecretPath(strings.TrimPrefix(a, "a/")) || IsSecretPath(strings.TrimPrefix(b, "b/"))
			continue
		}
		if !secret || strings.HasPrefix(ln, "---") || strings.HasPrefix(ln, "+++") || strings.HasPrefix(ln, "@@") ||
			strings.HasPrefix(ln, "index ") || strings.HasPrefix(ln, "new file") || strings.HasPrefix(ln, "deleted file") {
			continue
		}
		if ln != "" && (ln[0] == '+' || ln[0] == '-' || ln[0] == ' ') {
			lines[i] = ln[:1] + maskLine(ln[1:])
		}
	}
	return strings.Join(lines, "\n")
}
