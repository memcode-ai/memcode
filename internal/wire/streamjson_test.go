package wire

import "testing"

// The stream-json handshake is a wire contract like the gateway's: the pin must
// ride the initialize payload as lower_snake_case when set, and stay OFF the wire
// when empty (omitempty — an absent pin means Automatic, not `"pin":""`).
func TestInitializeDataPinRidesTheHandshake(t *testing.T) {
	withPin := mustMarshal(t, InitializeData{Cwd: "/repo", Mode: "allow-all", Pin: "sonnet"})
	assertHasKeys(t, "InitializeData", withPin, "cwd", "mode", "pin")

	noPin := mustMarshal(t, InitializeData{Cwd: "/repo", Mode: "allow-all"})
	assertHasKeys(t, "InitializeData", noPin, "cwd", "mode")
	assertNoKeys(t, "InitializeData", noPin, "pin", "Pin")
}

// The additive v1 fields a desktop/SDK client renders must ride the wire under
// the exact snake_case keys the client mirrors. These lock that contract so a
// rename here fails loudly instead of silently breaking every client.

func TestResumeRidesInitialize(t *testing.T) {
	withResume := mustMarshal(t, InitializeData{Cwd: "/repo", Mode: "ask", Resume: "sess_abc"})
	assertHasKeys(t, "InitializeData", withResume, "resume")

	noResume := mustMarshal(t, InitializeData{Cwd: "/repo", Mode: "ask"})
	assertNoKeys(t, "InitializeData", noResume, "resume", "Resume")
}

func TestUserTurnAttachmentsContract(t *testing.T) {
	withAtt := mustMarshal(t, UserTurnData{
		Text:        "look at this",
		Attachments: []Attachment{{Path: "/a/b.png", Name: "b.png", Mime: "image/png"}},
	})
	assertHasKeys(t, "UserTurnData", withAtt, "text", "attachments")

	// An attachment's own keys must be path/name/mime.
	att := mustMarshal(t, Attachment{Path: "/a/b.png", Name: "b.png", Mime: "image/png"})
	assertHasKeys(t, "Attachment", att, "path", "name", "mime")

	// Path is required; name/mime are optional and stay off the wire when empty.
	bare := mustMarshal(t, Attachment{Path: "/a/b.png"})
	assertHasKeys(t, "Attachment", bare, "path")
	assertNoKeys(t, "Attachment", bare, "name", "mime")

	noAtt := mustMarshal(t, UserTurnData{Text: "hi"})
	assertNoKeys(t, "UserTurnData", noAtt, "attachments", "Attachments")
}

func TestDiffDataContract(t *testing.T) {
	js := mustMarshal(t, DiffData{Path: "main.go", Language: "go", Unified: "@@ -1 +1 @@", Added: 3, Removed: 1, NewFile: false})
	assertHasKeys(t, "DiffData", js, "path", "language", "unified", "added", "removed")
}

func TestTodosDataContract(t *testing.T) {
	js := mustMarshal(t, TodosData{Items: []TodoItem{{Text: "build", Status: "in_progress"}}})
	assertHasKeys(t, "TodosData", js, "items")
	item := mustMarshal(t, TodoItem{Text: "build", Status: "in_progress"})
	assertHasKeys(t, "TodoItem", item, "text", "status")
}

func TestPermissionResponseRememberContract(t *testing.T) {
	js := mustMarshal(t, PermissionResponseData{Allow: true, Remember: true, RememberScope: "project"})
	assertHasKeys(t, "PermissionResponseData", js, "allow", "remember", "remember_scope")

	noRemember := mustMarshal(t, PermissionResponseData{Allow: true})
	assertNoKeys(t, "PermissionResponseData", noRemember, "remember_scope", "RememberScope")
}
