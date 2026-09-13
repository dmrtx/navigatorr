//go:build windows

package transcodeworker

import (
	"os"
)

// openNoFollow opens a file for reading after verifying it is not a symlink on Windows.
func openNoFollow(path string) (*os.File, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return nil, os.ErrInvalid
	}
	return os.Open(path)
}
