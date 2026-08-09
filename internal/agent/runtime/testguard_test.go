package runtime

import "testing"

func TestIsTestPath(t *testing.T) {
	yes := []string{"validate_test.go", "pkg/foo_test.go", "src/foo.test.ts", "a/b.spec.tsx",
		"test_thing.py", "thing_test.py", "conftest.py", "ui/__tests__/a.js", "x.snap", "pkg/testdata/in.json"}
	no := []string{"validate.go", "main.py", "src/index.ts", "README.md", "internal/foo/bar.go"}
	for _, p := range yes {
		if !isTestPath(p) {
			t.Errorf("isTestPath(%q) = false, want true", p)
		}
	}
	for _, p := range no {
		if isTestPath(p) {
			t.Errorf("isTestPath(%q) = true, want false", p)
		}
	}
}

func TestWeakensTest(t *testing.T) {
	const twoTests = "func TestA(t *testing.T){ if x()==nil {t.Fatal(\"a\")} }\nfunc TestB(t *testing.T){ if y()!=nil {t.Fatal(\"b\")} }"

	cases := []struct {
		name     string
		old, new string
		want     bool
	}{
		{"create new file is fine", "", twoTests, false},
		{"delete a test weakens", twoTests, "func TestA(t *testing.T){ if x()==nil {t.Fatal(\"a\")} }", true},
		{"change expected value weakens", "if got != 5 {t.Fatal()}", "if got != 6 {t.Fatal()}", true},
		{"adding a test is fine", "func TestA(t *testing.T){}", "func TestA(t *testing.T){}\nfunc TestC(t *testing.T){}", false},
		{"injecting a skip weakens", "func TestA(t *testing.T){ check() }", "func TestA(t *testing.T){ t.Skip(\"flaky\"); check() }", true},
	}
	for _, c := range cases {
		if got := weakensTest(c.old, c.new); got != c.want {
			t.Errorf("%s: weakensTest = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestUserIntendsTestChange(t *testing.T) {
	yes := []string{"update the tests to allow empty names", "change the behavior and update tests",
		"this test encodes an old assumption — rewrite it", "the tests are stale", "update the snapshots"}
	no := []string{"run the tests and make them pass", "fix the failing build", "make it green",
		"why are the tests failing?", "add a feature"}
	for _, s := range yes {
		if !userIntendsTestChange(s) {
			t.Errorf("userIntendsTestChange(%q) = false, want true", s)
		}
	}
	for _, s := range no {
		if userIntendsTestChange(s) {
			t.Errorf("userIntendsTestChange(%q) = true, want false", s)
		}
	}
}
