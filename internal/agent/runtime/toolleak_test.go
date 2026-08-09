package runtime

import "testing"

func TestDetectLeakedToolCall(t *testing.T) {
	cases := []struct {
		name string
		text string
		want string
	}{
		{"clean glm leak", "<tool_call>bash<arg_key>command</arg_key><arg_value>ls</arg_value></tool_call>", "bash"},
		{"with prose before", "Let me write it.\n<tool_call>edit_file<arg_key>path</arg_key><arg_value>x.go</arg_value>", "edit_file"},
		{"truncated mid-arg (the real failure)", "<tool_call>bash<arg_key>command</arg_key><arg_value>cat > x.sql << 'EOF'\n-- migration", "bash"},
		{"normal prose, no envelope", "I'll update the file and run the tests.", ""},
		{"tool_call mentioned but no arg envelope", "the <tool_call> tag is how GLM serializes calls", ""},
		{"empty", "", ""},
	}
	for _, c := range cases {
		if got := detectLeakedToolCall(c.text); got != c.want {
			t.Errorf("%s: detectLeakedToolCall(%q) = %q, want %q", c.name, c.text, got, c.want)
		}
	}
}
