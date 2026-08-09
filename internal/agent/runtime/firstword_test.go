package runtime

import "testing"

func TestFirstWordSkipsEnvAssignments(t *testing.T) {
	cases := map[string]string{
		"grep -rn foo":                  "grep",
		"SDK=/path/to/sdk grep -rn foo": "grep", // the bug: was "SDK=/path/to/sdk"
		"A=1 B=2 go test ./...":         "go",
		"FOO=bar":                       "",   // assignment only, no command
		"  cd api && go build":          "cd", // leading whitespace, no assignment
		"":                              "",
		"=notvalid cmd":                 "=notvalid", // not a valid NAME= assignment → treated as the token
		"9BAD=x cmd":                    "9BAD=x",    // identifier can't start with a digit → not an assignment
	}
	for cmd, want := range cases {
		if got := firstWord(cmd); got != want {
			t.Errorf("firstWord(%q) = %q, want %q", cmd, got, want)
		}
	}
}

func TestRememberPatternUsesRealBinary(t *testing.T) {
	// The degenerate rule we saw: `SDK=… grep` → `SDK=… *`. Now it's `grep *`.
	if got := rememberPattern("SDK=/x/y grep -rn foo"); got != "grep *" {
		t.Fatalf("rememberPattern = %q, want %q", got, "grep *")
	}
}
