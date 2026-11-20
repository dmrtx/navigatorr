//go:build !(linux || darwin)

package nodes

import (
	"errors"
	"os"
)

// lockFile refuses to run where flock is not available: without the lock two
// processes could write the same nodes.db.
func lockFile(string) (*os.File, error) {
	return nil, errors.New("nodes.db needs flock, which this platform does not have")
}
