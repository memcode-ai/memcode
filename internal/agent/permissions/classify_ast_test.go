package permissions

import "testing"

// These cases exercise shell structure that string-splitting could never get right:
// quotes, pipelines inside quotes, subshells, command substitution inside double
// quotes, process substitution, and here-docs. They are the reason the classifier
// parses with a real shell grammar instead of slicing on separators.
func TestClassifyBashStructural(t *testing.T) {
	cases := []struct {
		cmd          string
		wantRisk     Risk
		catastrophic bool
	}{
		// `|` inside a quoted arg must not be read as a pipeline separator.
		{`grep -E "a|b|c" file`, Safe, false},
		{`psql -c "INSERT INTO t VALUES('a|b')"`, Dangerous, false}, // pipe inside nested quotes inside SQL
		{`psql -c "SELECT 'a|b|c'"`, Safe, false},
		// Subshell / grouping: the dangerous command inside still counts.
		{`(cd /tmp && rm -rf x)`, Dangerous, true},
		{`{ ls; cat f; }`, Safe, false},
		// Command substitution inside double quotes is still classified.
		{`echo "$(rm -rf /)"`, Dangerous, true},
		{`echo "today is $(date)"`, Safe, false},
		// Process substitution: each inner command is read-only.
		{`diff <(sort a) <(sort b)`, Safe, false},
		// Here-doc body is not a file write.
		{"cat <<EOF\nhello\nEOF", Safe, false},
		// A genuine file write is still a write, even buried in a pipeline.
		{`grep x f | sort > out.txt`, Medium, false},
		// …but /dev/null and fd-dups write nothing.
		{`grep x f > /dev/null 2>&1`, Safe, false},
		// Wrapper + nested quotes survive the round-trip (SQL stays one argument).
		{`sudo docker exec -i pg psql -U legion -c "SELECT 1 WHERE a='x|y'"`, Safe, false},
	}
	for _, c := range cases {
		risk, cat := ClassifyBash(c.cmd)
		if risk != c.wantRisk || cat != c.catastrophic {
			t.Errorf("ClassifyBash(%q) = (%v, %v), want (%v, %v)",
				c.cmd, risk, cat, c.wantRisk, c.catastrophic)
		}
	}
}
