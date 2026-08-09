package tools

import (
	"encoding/json"
	"testing"
)

// Models emit ask_user options inconsistently — sometimes as objects
// {label, description}, sometimes as bare strings. AskOption must decode BOTH so an
// option that arrived as a string isn't dropped or mis-parsed.
func TestAskOptionTolerantDecode(t *testing.T) {
	var in AskUserInput
	raw := `{"question":"Which auth?","options":[
		{"label":"Clerk","description":"hosted, fastest"},
		"No auth",
		{"label":"Custom"}
	]}`
	if err := json.Unmarshal([]byte(raw), &in); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(in.Options) != 3 {
		t.Fatalf("want 3 options, got %d", len(in.Options))
	}
	if in.Options[0].Label != "Clerk" || in.Options[0].Description != "hosted, fastest" {
		t.Errorf("object option mis-decoded: %+v", in.Options[0])
	}
	if in.Options[1].Label != "No auth" || in.Options[1].Description != "" {
		t.Errorf("bare-string option must become Label-only: %+v", in.Options[1])
	}
	if in.Options[2].Label != "Custom" {
		t.Errorf("object without description: %+v", in.Options[2])
	}
}
