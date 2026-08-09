package secrets

import (
	"strings"
	"testing"
)

func TestIsSecretPath(t *testing.T) {
	secret := []string{".env", "apps/web/.env.local", "config/prod.env", "key.pem",
		"id_rsa", "deploy.key", ".npmrc", "secrets.yaml", "home/.aws/credentials"}
	notSecret := []string{"main.go", "README.md", "package.json", "src/keyboard.ts", "monkey.txt"}

	for _, p := range secret {
		if !IsSecretPath(p) {
			t.Errorf("IsSecretPath(%q) = false, want true", p)
		}
	}
	for _, p := range notSecret {
		if IsSecretPath(p) {
			t.Errorf("IsSecretPath(%q) = true, want false", p)
		}
	}
}

func TestRedactSecretFile(t *testing.T) {
	in := "# comment\nexport API_KEY=sk-abc123\nDB_URL=postgres://u:p@host/db\n\n"
	out := RedactSecretFile(in)
	if strings.Contains(out, "sk-abc123") || strings.Contains(out, "postgres://u:p@host/db") {
		t.Fatalf("values not masked: %q", out)
	}
	if !strings.Contains(out, "API_KEY=***REDACTED***") || !strings.Contains(out, "DB_URL=***REDACTED***") {
		t.Errorf("keys not preserved: %q", out)
	}
	if !strings.Contains(out, "# comment") {
		t.Error("comment should be preserved")
	}

	// Diff lines (leading +/-) are masked too.
	diff := "+API_KEY=newsecretvalue\n-OLD=removedsecret\n"
	dout := RedactSecretFile(diff)
	if strings.Contains(dout, "newsecretvalue") || strings.Contains(dout, "removedsecret") {
		t.Errorf("diff values not masked: %q", dout)
	}

	// PEM bodies are masked.
	pem := "-----BEGIN PRIVATE KEY-----\nMIIBVgIBADANBg\n-----END PRIVATE KEY-----\n"
	if strings.Contains(RedactSecretFile(pem), "MIIBVgIBADANBg") {
		t.Error("PEM body not masked")
	}
}

func TestRedactorFromEnv(t *testing.T) {
	t.Setenv("MY_TEST_API_KEY", "supersecretvalue123")
	t.Setenv("NOT_SENSITIVE", "plainvalue123")
	r := NewFromEnv()

	got := r.Redact("the key is supersecretvalue123 and plainvalue123 stays")
	if strings.Contains(got, "supersecretvalue123") {
		t.Errorf("secret value not redacted: %q", got)
	}
	if !strings.Contains(got, "plainvalue123") {
		t.Errorf("non-sensitive value should not be redacted: %q", got)
	}
}
