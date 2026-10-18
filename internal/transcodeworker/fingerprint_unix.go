//go:build !windows

package transcodeworker

import (
	"os"
	"syscall"
)

// fingerprintOf returns the strict stat fingerprint of a local file: device,
// inode, size and nanosecond modification time. A hard link or rename keeps
// it; any ordinary write changes the modification time.
func fingerprintOf(fi os.FileInfo) (fileFingerprint, bool) {
	if fi == nil || !fi.Mode().IsRegular() {
		return fileFingerprint{}, false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fileFingerprint{}, false
	}
	return fileFingerprint{
		dev:     uint64(st.Dev),
		ino:     uint64(st.Ino),
		size:    fi.Size(),
		modNano: fi.ModTime().UnixNano(),
	}, true
}

// deviceOf returns the filesystem device holding path.
func deviceOf(path string) (uint64, bool) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(st.Dev), true
}
