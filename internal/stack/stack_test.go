package stack

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestLocalDetect(t *testing.T) {
	root := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(root, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(body), 0o644)
	}
	// a Go module in a subdir (monorepo) + a Next.js web app + Docker + CI
	write("cli/go.mod", "module x\n\ngo 1.26\n\nrequire (\n\tgithub.com/spf13/cobra v1.8.0\n\tcharm.land/bubbletea/v2 v0.20.0\n)\n")
	write("cli/main.go", "package main\nfunc main(){}\n")
	write("www/package.json", `{"dependencies":{"next":"14","react":"18"}}`)
	write("Dockerfile", "FROM golang\n")
	write(".github/workflows/ci.yml", "on: push\n")

	f, err := LocalStackDetector{}.Detect(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	hasGo := false
	for _, l := range f.Languages {
		if l.Name == "Go" {
			hasGo = true
		}
	}
	if !hasGo {
		t.Errorf("expected Go among detected languages, got %+v", f.Languages)
	}
	want := map[string]bool{"Go 1.26": false, "Cobra": false, "Bubble Tea (TUI)": false, "Next.js": false, "Docker": false, "GitHub Actions": false}
	all := append(append(append(append(f.Runtimes, f.Frameworks...), f.CLIs...), f.Infra...), f.CI...)
	for _, tf := range all {
		if _, ok := want[tf.Name]; ok {
			want[tf.Name] = true
			if len(tf.Evidence) == 0 {
				t.Errorf("%s has no evidence", tf.Name)
			}
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("expected to detect %q", name)
		}
	}
}
