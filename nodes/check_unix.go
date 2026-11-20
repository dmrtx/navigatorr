//go:build linux || darwin

package nodes

import (
	"os"
	"path/filepath"
	"syscall"
)

func dirOf(path string) string { return filepath.Dir(path) }

// ownerOf is the owning uid and the permission bits of a file.
func ownerOf(fi os.FileInfo) (int, os.FileMode) {
	uid := -1
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		uid = int(st.Uid)
	}
	return uid, fi.Mode().Perm()
}
