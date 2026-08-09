package producer

import "testing"

func TestClassify(t *testing.T) {
	cases := []struct {
		name          string
		author, email string
		subject, body string
		want          Producer
	}{
		{"anthropic email", "Claude", "noreply@anthropic.com", "fix bug", "", ClaudeCode},
		{"claude trailer", "Tim", "tim@x.com", "feat: thing", "Co-Authored-By: Claude <noreply@anthropic.com>", ClaudeCode},
		{"claude generated", "Tim", "tim@x.com", "stuff", "🤖 Generated with Claude Code", ClaudeCode},
		{"codex trailer", "Tim", "tim@x.com", "feat", "Co-Authored-By: Codex <noreply@openai.com>", Codex},
		{"cursor email", "Cursor Agent", "agent@cursor.com", "edit", "", Cursor},
		{"plain human", "Tim Erwin", "tim@x.com", "feat: real work", "details here", Human},
		{"unknown co-author", "Tim", "tim@x.com", "x", "Co-Authored-By: Somebody <a@b.c>", Unknown},
	}
	for _, c := range cases {
		if got := Classify(c.author, c.email, c.subject, c.body); got.Producer != c.want {
			t.Errorf("%s: Classify = %q, want %q (evidence: %s)", c.name, got.Producer, c.want, got.Evidence)
		}
	}
}
