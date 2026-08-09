package runtime

import (
	"path/filepath"
	"regexp"
	"strings"
)

// The trust guardrail, ENFORCED in the runtime (not just asked for in the prompt —
// the prompt version didn't hold): self-heal may edit implementation code freely,
// but an edit that WEAKENS an existing test/spec/snapshot/fixture (deletes or skips a
// test, removes/alters an assertion, changes an expected value) changes WHAT is
// verified — a product decision. That edit requires explicit approval EVEN in
// allow-all, and fails closed when no one can approve. Adding new tests is free.

// testIntentRe matches an explicit user request to change tests/specs/behavior —
// the case where editing tests is the WORK, not a self-heal cheat. Conservative on
// purpose: a miss just means an extra approval; a false hit would re-open the cheat
// vector, so ambiguous verbs like "fix" (often "fix the code so tests pass") are out.
var testIntentRe = regexp.MustCompile(`(?i)\b(` +
	`(update|change|rewrite|revise|modify|correct|edit|relax|loosen)\s+(the\s+|these\s+|those\s+|all\s+|my\s+|our\s+)?(tests?|specs?|snapshots?|assertions?|fixtures?|expectations?)` +
	`|(update|change)\s+(the\s+)?(behaviou?r|spec|contract|api)` +
	`|(tests?|specs?|assumptions?|snapshots?)\s+(are|is|were|now)\s+\w*\s*(wrong|stale|outdated|obsolete|incorrect|invalid)` +
	`|(old|stale|outdated|wrong)\s+assumptions?` +
	`|update\s+(the\s+)?snapshots?` +
	`)\b`)

// userIntendsTestChange reports whether the user's request explicitly authorizes
// changing tests/specs/behavior. When true, test edits proceed under normal policy;
// when false, weakening a test is gated as trust-critical (self-heal cheat).
func userIntendsTestChange(request string) bool {
	return testIntentRe.MatchString(request)
}

// isTestPath reports whether path is a test / spec / snapshot / fixture file.
func isTestPath(path string) bool {
	p := strings.ToLower(filepath.ToSlash(path))
	base := filepath.Base(p)
	switch {
	case strings.HasSuffix(base, "_test.go"),
		strings.HasSuffix(base, "_spec.rb"), strings.HasSuffix(base, "_test.rb"),
		strings.HasSuffix(base, "_test.py"), base == "conftest.py",
		strings.HasPrefix(base, "test_") && strings.HasSuffix(base, ".py"),
		strings.HasSuffix(base, ".snap"):
		return true
	}
	if strings.Contains(base, ".test.") || strings.Contains(base, ".spec.") {
		return true // foo.test.ts / foo.spec.tsx
	}
	for _, seg := range []string{"__tests__/", "__snapshots__/", "/testdata/", "/fixtures/", "/__fixtures__/"} {
		if strings.Contains(p, seg) {
			return true
		}
	}
	return false
}

// skipRe matches markers that disable/skip a test.
var skipRe = regexp.MustCompile(`(?i)(t\.skip\b|\.skip\(|\bxit\(|\bxdescribe\(|pytest\.mark\.skip|unittest\.skip|@skip\b|t\.Skip\()`)

// weakensTest reports whether replacing old with new in a test file WEAKENS what's
// verified: it removes any meaningful line (a deleted test, a removed assertion, a
// changed expected value — the old line is gone) or introduces a skip marker. Purely
// additive edits (new keeps every meaningful line of old, adds no skip) are fine —
// that's writing more tests. old=="" (creating a new file) is never a weakening.
func weakensTest(old, new string) bool {
	if strings.TrimSpace(old) == "" {
		return false
	}
	if skipRe.MatchString(new) && !skipRe.MatchString(old) {
		return true
	}
	newLines := map[string]int{}
	for _, l := range strings.Split(new, "\n") {
		newLines[strings.TrimSpace(l)]++
	}
	for _, l := range strings.Split(old, "\n") {
		t := strings.TrimSpace(l)
		switch t { // structural/blank lines aren't meaningful on their own
		case "", "{", "}", "})", ")", "});":
			continue
		}
		if newLines[t] > 0 {
			newLines[t]--
			continue
		}
		return true // a meaningful old line vanished → the test was weakened/altered
	}
	return false
}
