package browser

import "context"

// Controller is the stable browser boundary shared by ephemeral and brokered
// backends. Calls remain typed; autonomous agents never receive raw MCP access.
type Controller interface {
	Close() error
	Navigate(context.Context, string) error
	NewTab(context.Context, string) error
}
