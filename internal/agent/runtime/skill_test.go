package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/memcode-ai/memcode/internal/skills"
)

func skillSession(t *testing.T) *Session {
	t.Helper()
	s := newTodoSession(t)
	s.skills = []skills.Skill{
		{Name: "claude-api", Description: "Build Claude API apps.", Path: writeTempSkill(t, "claude-api", "USE PROMPT CACHING")},
	}
	return s
}

func writeTempSkill(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "SKILL.md")
	if err := os.WriteFile(p, []byte("---\nname: "+name+"\ndescription: d\n---\n"+body+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestSkillGatedAndLoaded: an approved skill returns its body to the model.
func TestSkillGatedAndLoaded(t *testing.T) {
	s := skillSession(t)
	asked := 0
	s.approve = func(_ context.Context, r ApprovalRequest) ApprovalDecision {
		asked++
		if r.Title != "claude-api" || r.Label != "Use skill" {
			t.Errorf("unexpected approval request: %+v", r)
		}
		return ApprovalDecision{Allow: true}
	}
	r := s.useSkill(context.Background(), []byte(`{"load":"claude-api"}`))
	out, isErr := r.text(), r.isError
	if isErr {
		t.Fatalf("expected the skill to load: %q", out)
	}
	if asked != 1 {
		t.Errorf("skill load must be gated by exactly one ask, got %d", asked)
	}
	if !strings.Contains(out, "USE PROMPT CACHING") {
		t.Errorf("skill body not returned: %q", out)
	}
}

// TestSkillDeclined: a declined skill is not loaded, and the model is told so.
func TestSkillDeclined(t *testing.T) {
	s := skillSession(t)
	s.approve = func(_ context.Context, _ ApprovalRequest) ApprovalDecision { return ApprovalDecision{} }
	r := s.useSkill(context.Background(), []byte(`{"load":"claude-api"}`))
	out, isErr := r.text(), r.isError
	if !isErr || !strings.Contains(out, "declined") {
		t.Errorf("declined skill should report not-loaded: %q (err=%v)", out, isErr)
	}
	if strings.Contains(out, "USE PROMPT CACHING") {
		t.Error("declined skill body must NOT leak")
	}
}

// TestSkillRememberSkipsReask: "don't ask again" loads without re-prompting.
func TestSkillRememberSkipsReask(t *testing.T) {
	s := skillSession(t)
	asked := 0
	s.approve = func(_ context.Context, _ ApprovalRequest) ApprovalDecision {
		asked++
		return ApprovalDecision{Allow: true, Remember: true}
	}
	for i := 0; i < 3; i++ {
		if r := s.useSkill(context.Background(), []byte(`{"load":"claude-api"}`)); r.isError {
			t.Fatal("load failed")
		}
	}
	if asked != 1 {
		t.Errorf("remember should suppress re-asks: asked %d times, want 1", asked)
	}
}

// TestSkillUnknownName: an unknown skill name is a clean error, not a crash.
func TestSkillUnknownName(t *testing.T) {
	s := skillSession(t)
	r := s.useSkill(context.Background(), []byte(`{"load":"does-not-exist"}`))
	out, isErr := r.text(), r.isError
	if !isErr || !strings.Contains(out, "no installed skill") {
		t.Errorf("unknown skill should error cleanly: %q", out)
	}
}

// TestSkillFind: `find` searches installed skills and lists matches WITHOUT prompting
// (discovery is read-only; only loading is gated).
func TestSkillFind(t *testing.T) {
	s := skillSession(t)
	s.skills = append(s.skills, skills.Skill{Name: "supabase", Description: "Supabase Database, Auth, RLS, Edge Functions."})
	s.approve = func(_ context.Context, _ ApprovalRequest) ApprovalDecision {
		t.Error("find must not prompt for approval")
		return ApprovalDecision{}
	}
	r := s.useSkill(context.Background(), []byte(`{"find":"supabase rls"}`))
	out, isErr := r.text(), r.isError
	if isErr {
		t.Fatalf("find should not error: %q", out)
	}
	if !strings.Contains(out, "supabase") {
		t.Errorf("find should list the matching skill: %q", out)
	}
	if strings.Contains(out, "USE PROMPT CACHING") {
		t.Error("find must not load a body — only list")
	}
}

// TestSkillApprovalPersists: a remembered approval is written to .memcode and reloaded, so
// the user isn't re-prompted across sessions (idempotent — no duplicate lines).
func TestSkillApprovalPersists(t *testing.T) {
	root := t.TempDir()
	s := &Session{root: root}
	s.rememberSkill("supabase")
	s.rememberSkill("supabase") // idempotent

	if got := loadApprovedSkills(root); !got["supabase"] {
		t.Fatalf("approval was not persisted: %v", got)
	}
	b, _ := os.ReadFile(skillApprovalsPath(root))
	if n := strings.Count(strings.TrimSpace(string(b)), "supabase"); n != 1 {
		t.Errorf("approval should be written once, found %d times:\n%s", n, b)
	}
}

// TestSkillNotOfferedToExplorers: read-only sub-agents never get the skill tool.
func TestSkillNotOfferedToExplorers(t *testing.T) {
	s := skillSession(t)
	if !hasTool(s.toolDefs(), "skill") {
		t.Error("chat with skills should offer the skill tool")
	}
	s.readOnly = true
	if hasTool(s.toolDefs(), "skill") {
		t.Error("explorers must NOT be offered the skill tool")
	}
}
