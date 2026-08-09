package input

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseRouting(t *testing.T) {
	cwd := t.TempDir()
	cases := []struct {
		line string
		want Route
	}{
		{"! stop", Interrupt},
		{"> after this update readme", Queue},
		{"+ also add json", Steer},
		{"stop, that's wrong", Interrupt},
		{"wait no", Interrupt},
		{"after this, fix the UI", Queue},
		{"next, run the tests", Queue},
		{"also handle the empty case", Steer},
		{"make the map command output json", Steer},
		{"$ git status", Shell},
		{"$git status", Shell},
	}
	for _, c := range cases {
		got := Parse(c.line, cwd)
		if got.Route != c.want {
			t.Errorf("Parse(%q).Route = %q, want %q (reason: %s)", c.line, got.Route, c.want, got.Reason)
		}
	}
}

// The `$` shell prefix routes to Shell and yields the bare command (sigil + any
// leading space stripped), so the executor receives exactly what to run.
func TestParseShellCommand(t *testing.T) {
	cwd := t.TempDir()
	for _, line := range []string{"$ git status", "$git status"} {
		d := Parse(line, cwd)
		if d.Route != Shell {
			t.Errorf("Parse(%q).Route = %q, want Shell", line, d.Route)
		}
		if d.Bundle.Text != "git status" {
			t.Errorf("Parse(%q).Text = %q, want %q", line, d.Bundle.Text, "git status")
		}
	}
}

// Parse never turns text into attachments — even when words match real repo
// paths. This is the bug that made pasting prose explode into file chips.
func TestParseNeverAttachesFromText(t *testing.T) {
	cwd := t.TempDir()
	// Real paths that a prose paste might mention.
	for _, p := range []string{"datasets", "scripts", "stage_dataset.py"} {
		_ = os.MkdirAll(filepath.Join(cwd, "datasets"), 0o755)
		_ = os.MkdirAll(filepath.Join(cwd, "scripts"), 0o755)
		_ = os.WriteFile(filepath.Join(cwd, "stage_dataset.py"), []byte("x"), 0o644)
		_ = p
	}
	text := "reconcile datasets/ and scripts with stage_dataset.py and a lone /"
	d := Parse(text, cwd)
	if len(d.Bundle.Attachments) != 0 {
		t.Fatalf("text must never produce attachments, got %+v", d.Bundle.Attachments)
	}
	if d.Bundle.Text != text {
		t.Errorf("text mangled: %q, want %q (kept verbatim)", d.Bundle.Text, text)
	}
}

// Resolve is the ONLY path to an attachment, and only for a real, path-shaped file.
func TestResolveImage(t *testing.T) {
	cwd := t.TempDir()
	shot := filepath.Join(cwd, "screenshot.png")
	if err := os.WriteFile(shot, []byte("\x89PNG\r\n\x1a\nfakebytes"), 0o644); err != nil { // valid PNG signature
		t.Fatal(err)
	}
	a, ok := Resolve(shot, cwd, "drag_drop")
	if !ok {
		t.Fatal("Resolve should attach a real image path")
	}
	if a.Kind != KindImage || a.Mime != "image/png" {
		t.Errorf("kind/mime = %q/%q, want image/image/png", a.Kind, a.Mime)
	}
	if a.SHA256 == "" || a.SizeBytes == 0 {
		t.Error("expected sha256 + size to be captured")
	}
	if a.Source != "drag_drop" {
		t.Errorf("source = %q, want drag_drop", a.Source)
	}
}

func TestResolveSecretFlagged(t *testing.T) {
	cwd := t.TempDir()
	env := filepath.Join(cwd, ".env")
	if err := os.WriteFile(env, []byte("ANTHROPIC_API_KEY=sk-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	a, ok := Resolve(env, cwd, "attach")
	if !ok || a.Kind != KindSecret {
		t.Fatalf("expected a secret-flagged attachment, got %+v ok=%v", a, ok)
	}
}

// Resolve refuses bare words and non-path tokens — provenance must be a real path.
func TestResolveRejectsBareWords(t *testing.T) {
	cwd := t.TempDir()
	_ = os.MkdirAll(filepath.Join(cwd, "datasets"), 0o755)
	for _, bare := range []string{"datasets", "/", ".", "..", "", "scripts"} {
		if _, ok := Resolve(bare, cwd, "attach"); ok {
			t.Errorf("Resolve(%q) attached a bare word — must require a path shape", bare)
		}
	}
}

func TestLooksLikePath(t *testing.T) {
	yes := []string{"a.png", "dir/file", "/abs/path", `win\path`, "x.go"}
	no := []string{"datasets", "scripts", "/", ".", "..", "", "hello"}
	for _, s := range yes {
		if !looksLikePath(s) {
			t.Errorf("looksLikePath(%q) = false, want true", s)
		}
	}
	for _, s := range no {
		if looksLikePath(s) {
			t.Errorf("looksLikePath(%q) = true, want false", s)
		}
	}
}
