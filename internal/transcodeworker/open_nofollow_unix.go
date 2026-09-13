//go:build !windows

package transcodeworker

import (
	"os"
	"syscall"
)

// openNoFollow opens a file for reading without following symlinks on Unix/macOS platforms.
// If the final path component is a symlink, open fails with an error (e.g. syscall.ELOOP or syscall.EMLINK).
func openNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
}
