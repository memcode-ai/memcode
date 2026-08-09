package permissions

import (
	"strings"
	"time"
)

// Approval is a remembered allow-rule (the matching view of a stored
// permission). It is intentionally decoupled from the storage layer.
type Approval struct {
	ID        string
	Pattern   string
	Cwd       string // "" = any cwd
	Trusted   bool   // may approve catastrophic commands
	ExpiresAt *time.Time
}

// Match reports whether any approval permits the command (in cwd) at time now.
// catastrophic commands match only a Trusted rule. It returns the matched
// approval for bookkeeping.
func Match(approvals []Approval, command, cwd string, catastrophic bool, now time.Time) (Approval, bool) {
	for _, a := range approvals {
		if a.ExpiresAt != nil && now.After(*a.ExpiresAt) {
			continue
		}
		if a.Cwd != "" && a.Cwd != cwd {
			continue
		}
		if catastrophic && !a.Trusted {
			continue
		}
		cmd := strings.TrimSpace(command)
		// Match the raw command OR its EFFECTIVE form (leading env-assignments stripped,
		// $(...) neutralized) so a saved "grep *" rule also matches "SDK=$(go env GOPATH)
		// grep …" — otherwise an assignment prefix makes every command look unique and the
		// rule never fires (the "I keep approving and it keeps asking" bug).
		if globMatch(a.Pattern, cmd) {
			return a, true
		}
		if eff := EffectiveCommand(cmd); eff != cmd && globMatch(a.Pattern, eff) {
			return a, true
		}
		// Anchor at the highest-risk sub-command — the same call rememberPattern keys
		// on — so a rule saved from a compound (`echo … && supabase …` → "supabase *")
		// matches it. Without this, a "don't ask again" for a binary buried mid-pipeline
		// never fires and re-prompts every turn (the "it forgot I just approved" bug).
		if seg := RiskSegment(cmd); seg != cmd && globMatch(a.Pattern, seg) {
			return a, true
		}
	}
	return Approval{}, false
}

// EffectiveCommand strips leading env-assignments (VAR=val, VAR=$(...)), returning the
// command starting at its REAL binary. Used so the "don't ask again" rule keys on and
// matches the actual command, not the assignment prefix. It uses the shell parser: the
// first simple command's first argument is where the binary starts.
func EffectiveCommand(command string) string {
	command = strings.TrimSpace(command)
	if c, ok := firstCall(command); ok {
		if off := int(c.Args[0].Pos().Offset()); off >= 0 && off <= len(command) {
			return strings.TrimSpace(command[off:])
		}
	}
	return command
}

// CommandHead is the real binary of a command (basename, after env-assignments +
// substitutions are stripped) — the token a remembered approval should key on.
func CommandHead(command string) string {
	c, ok := firstCall(strings.TrimSpace(command))
	if !ok {
		return ""
	}
	head := wordText(c.Args[0])
	if i := strings.LastIndexByte(head, '/'); i >= 0 {
		head = head[i+1:]
	}
	return head
}

// globMatch matches a command against a pattern where '*' is a wildcard.
// Without a '*', it requires an exact match.
func globMatch(pattern, s string) bool {
	pattern = strings.TrimSpace(pattern)
	if !strings.Contains(pattern, "*") {
		return pattern == s
	}
	parts := strings.Split(pattern, "*")
	n := len(parts)

	// Anchor the head (unless the pattern starts with '*').
	if parts[0] != "" {
		if !strings.HasPrefix(s, parts[0]) {
			return false
		}
		s = s[len(parts[0]):]
	}
	// Anchor the tail (unless the pattern ends with '*').
	if parts[n-1] != "" {
		if !strings.HasSuffix(s, parts[n-1]) {
			return false
		}
		s = s[:len(s)-len(parts[n-1])]
	}
	// Remaining interior segments must appear in order.
	for i := 1; i < n-1; i++ {
		p := parts[i]
		if p == "" {
			continue
		}
		idx := strings.Index(s, p)
		if idx < 0 {
			return false
		}
		s = s[idx+len(p):]
	}
	return true
}
