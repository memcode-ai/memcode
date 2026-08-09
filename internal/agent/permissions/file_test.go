package permissions

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFileStoreRoundTrip(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".memcode"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Empty / missing file → no rules.
	if rules, err := Load(root); err != nil || len(rules) != 0 {
		t.Fatalf("empty: %v %v", rules, err)
	}

	if err := Append(root, "find *", false); err != nil {
		t.Fatal(err)
	}
	if err := Append(root, "git push *", true); err != nil {
		t.Fatal(err)
	}
	if err := Append(root, "find *", false); err != nil { // dup is a no-op
		t.Fatal(err)
	}

	rules, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 2 {
		t.Fatalf("expected 2 rules (dup ignored), got %d: %+v", len(rules), rules)
	}
	// The rules actually gate commands, and trusted carries through.
	if _, ok := Match(rules, "find apps -name x", "", false, time.Now()); !ok {
		t.Error("find rule should match")
	}
	if _, ok := Match(rules, "git push origin main", "", true, time.Now()); !ok {
		t.Error("trusted rule should match a catastrophic command")
	}
	if _, ok := Match(rules, "rm -rf /", "", true, time.Now()); ok {
		t.Error("no rule should match an unrelated catastrophic command")
	}

	// The file is human-readable, with a header.
	data, _ := os.ReadFile(FilePath(root))
	if !contains(string(data), "find *") || !contains(string(data), "# memcode permissions") {
		t.Fatalf("file not human-readable:\n%s", data)
	}

	// Remove rewrites the file.
	if ok, err := Remove(root, "find *"); err != nil || !ok {
		t.Fatalf("remove: ok=%v err=%v", ok, err)
	}
	if rules, _ := Load(root); len(rules) != 1 {
		t.Fatalf("after remove expected 1 rule, got %d", len(rules))
	}
}

// A pattern whose last word is literally "trusted" (space-separated) must NOT be parsed as a
// trusted rule — the marker is a TAB + "trusted" (see Append). Splitting on any whitespace
// conflated the two, a permission-escalating misparse of the user-editable file.
func TestParseLineTrustedMarkerVsWord(t *testing.T) {
	// A hand-written pattern ending in the word "trusted" is NOT trusted.
	if a, ok := parseLine("cat trusted"); !ok || a.Trusted {
		t.Errorf(`parseLine("cat trusted") = %+v ok=%v, want pattern "cat trusted", Trusted=false`, a, ok)
	}
	if a, ok := parseLine("cat trusted"); !ok || a.Pattern != "cat trusted" {
		t.Errorf(`parseLine("cat trusted").Pattern = %q, want "cat trusted"`, a.Pattern)
	}
	// The real marker (tab + trusted) IS trusted, and the pattern excludes the marker.
	if a, ok := parseLine("git push *\ttrusted"); !ok || !a.Trusted || a.Pattern != "git push *" {
		t.Errorf(`parseLine("git push *\ttrusted") = %+v, want Pattern "git push *", Trusted=true`, a)
	}
	// Round-trip through Append: a spaced-"trusted" pattern stays untrusted.
	root := t.TempDir()
	_ = os.Mkdir(filepath.Join(root, ".memcode"), 0o755)
	if err := Append(root, "rm -rf trusted", false); err != nil {
		t.Fatal(err)
	}
	rules, _ := Load(root)
	if len(rules) != 1 || rules[0].Trusted {
		t.Fatalf("round-trip: got %+v, want one untrusted rule", rules)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
