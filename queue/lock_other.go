//go:build !unix

package queue

import "os"

// lockFile is a no-op where flock is unavailable. Running two navigatorr
// processes against one queue file on such a platform lets them overwrite each
// other's requests.
func lockFile(path string) (*os.File, error) {
	return nil, nil
}
