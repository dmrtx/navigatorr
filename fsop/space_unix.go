//go:build unix

package fsop

import (
	"os"
	"syscall"
)

// SpaceOf reports the space of the filesystem that holds path. On a network
// mount this is the share's volume, so it works as a fallback for asking the NAS.
func SpaceOf(path string) (Space, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return Space{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return Space{}, err
	}
	size := uint64(st.Bsize)
	space := Space{Total: uint64(st.Blocks) * size, Free: uint64(st.Bfree) * size, Avail: uint64(st.Bavail) * size}
	if sys, ok := info.Sys().(*syscall.Stat_t); ok {
		space.Device = uint64(sys.Dev)
	}
	return space, nil
}
