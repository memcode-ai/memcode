package runtime

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/agent/tools"
)

// Upload paths must resolve INSIDE the project root after symlink evaluation —
// a symlink inside the repo pointing at a file outside (e.g. ~/.ssh keys) is
// refused. No Chrome needed: this is pure path logic.
func TestResolveUploadPathConfinement(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "id_rsa")
	if err := os.WriteFile(secret, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	ok := filepath.Join(root, "doc.txt")
	if err := os.WriteFile(ok, []byte("fine"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "innocent.txt")
	if err := os.Symlink(secret, link); err != nil {
		t.Fatal(err)
	}

	if _, _, err := resolveUploadPath(root, "doc.txt"); err != nil {
		t.Errorf("a file inside the root must be uploadable: %v", err)
	}
	if _, _, err := resolveUploadPath(root, link); !errors.Is(err, errUploadOutside) {
		t.Errorf("a symlink escaping the root must be refused generically, got %v", err)
	}
	if _, _, err := resolveUploadPath(root, root); err == nil {
		t.Error("a directory must be refused (regular files only)")
	}

	// NO EXISTENCE ORACLE: an outside path that exists and one that doesn't
	// must be indistinguishable — the identical generic refusal, decided
	// lexically before the filesystem is touched.
	_, _, errExists := resolveUploadPath(root, secret)
	_, _, errMissing := resolveUploadPath(root, filepath.Join(outside, "no-such-file"))
	if !errors.Is(errExists, errUploadOutside) || !errors.Is(errMissing, errUploadOutside) {
		t.Fatalf("outside paths must both refuse generically: exists=%v missing=%v", errExists, errMissing)
	}
	if errExists.Error() != errMissing.Error() {
		t.Errorf("refusals must be identical (no oracle): %q vs %q", errExists, errMissing)
	}
	_, _, errDotDot := resolveUploadPath(root, "../"+filepath.Base(outside)+"/id_rsa")
	_, _, errDotDotMissing := resolveUploadPath(root, "../definitely-missing-dir/x")
	if !errors.Is(errDotDot, errUploadOutside) || !errors.Is(errDotDotMissing, errUploadOutside) {
		t.Errorf("dot-dot traversals must both refuse generically: %v / %v", errDotDot, errDotDotMissing)
	}
}

// A tool that can never run must not be advertised: browser_eval is Dangerous,
// and a detached job (gateway/background) has no approver — outside allow-all
// every call would be auto-denied, so the def is dropped.
func TestEvalNotAdvertisedWithoutApprover(t *testing.T) {
	s := &Session{browserEnabled: true, mode: permissions.ModeAuto}
	s.SetNoApprover(true)
	if s.allowTool(tools.BrowserEval) {
		t.Error("browser_eval must not be advertised to an approver-less auto-mode session")
	}
	if !s.allowTool(tools.BrowserNavigate) {
		t.Error("other browser tools stay advertised")
	}
	// allow-all can actually run Dangerous without a prompt — eval stays.
	s2 := &Session{browserEnabled: true, mode: permissions.ModeAllowAll}
	s2.SetNoApprover(true)
	if !s2.allowTool(tools.BrowserEval) {
		t.Error("allow-all keeps browser_eval (it runs without prompting)")
	}
	// An interactive session (approver present) keeps eval in any mode.
	s3 := &Session{browserEnabled: true, mode: permissions.ModeAuto}
	if !s3.allowTool(tools.BrowserEval) {
		t.Error("an interactive session keeps browser_eval")
	}
}
