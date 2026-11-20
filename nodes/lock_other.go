//go:build !(linux || darwin)

package nodes

import "os"

// lockFile is a no-op where flock is not available.
func lockFile(string) (*os.File, error) { return nil, nil }
