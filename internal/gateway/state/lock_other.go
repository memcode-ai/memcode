//go:build !unix

package state

import "os"

// acquireLock is a no-op on platforms without flock. The gateway's service
// installer only supports macOS and Linux, so single-instance enforcement there
// falls to the operator; the durable inbox still behaves correctly for one
// process.
func acquireLock(string) (*os.File, error) { return nil, nil }

// releaseLock is a no-op counterpart.
func releaseLock(*os.File) {}
