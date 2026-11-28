package secrets

import (
	"strings"
	"testing"
)

func TestCredentialFilesAndShapes(t *testing.T) {
	for _, p := range []string{"/h/.claude/.credentials.json", "/h/.codex/auth.json", "/h/.config/gh/hosts.yml",
		"/h/.config/memcode/.env", "/h/.docker/config.json", "/h/.kube/config"} {
		if !IsSecretPath(p) {
			t.Errorf("%s should be a secret path", p)
		}
	}
	js := `{"claudeAiOauth": {"accessToken": "abc123secretvalue", "expiresAt": 17}}`
	if got := RedactSecretFile(js); strings.Contains(got, "abc123") || !strings.Contains(got, `"expiresAt": 17`) {
		t.Errorf("JSON credential values not masked (or structure lost): %s", got)
	}
	r := &Redactor{}
	if got := r.Redact("key is sk-ant-api03-" + strings.Repeat("x", 30)); strings.Contains(got, "xxxx") {
		t.Errorf("known token shape not masked: %s", got)
	}
	for _, n := range []string{"MEMCODE_ENDPOINT_KEY", "DATABASE_URL", "PG_DSN", "GCP_SERVICE_ACCOUNT_KEY"} {
		if !SensitiveName(n) {
			t.Errorf("%s should be a sensitive name", n)
		}
	}
	if SensitiveName("MONKEY") || SensitiveName("PATH") {
		t.Error("ordinary names must not be sensitive")
	}
}

func TestChildEnvDropsOwnCredentials(t *testing.T) {
	got := ChildEnv([]string{"PATH=/bin", "MEMCODE_API_TOKEN=t", "MEMCODE_ENDPOINT_KEY=k", "OPENAI_API_KEY=o"})
	if strings.Join(got, ",") != "PATH=/bin,OPENAI_API_KEY=o" {
		t.Fatalf("ChildEnv = %v", got)
	}
}

func TestGrepAndDiffMasking(t *testing.T) {
	out := RedactSecretGrep("src/a.go:3:token := cfg\n.env:1:API_KEY=hunter2hunter2", ".")
	if strings.Contains(out, "hunter2") || !strings.Contains(out, "src/a.go:3:token := cfg") {
		t.Fatalf("grep masking wrong: %q", out)
	}
	if got := RedactSecretGrep("1:API_KEY=hunter2hunter2", ".env"); strings.Contains(got, "hunter2") {
		t.Fatalf("single-file grep not masked: %q", got)
	}
	diff := "diff --git a/.env b/.env\n--- a/.env\n+++ b/.env\n@@ -1 +1 @@\n-API_KEY=old1secret\n+API_KEY=new1secret\ndiff --git a/x.go b/x.go\n+fmt.Println(1)"
	got := RedactSecretDiff(diff)
	if strings.Contains(got, "1secret") || !strings.Contains(got, "+fmt.Println(1)") {
		t.Fatalf("diff masking wrong: %q", got)
	}
}
