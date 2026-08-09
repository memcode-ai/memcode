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
