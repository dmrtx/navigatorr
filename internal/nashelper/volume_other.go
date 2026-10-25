//go:build !linux

package nashelper

import (
	"fmt"
	"syscall"
)

// Volumes reports the space of the filesystem under each served root.
func (r *Roots) Volumes() ([]Volume, error) {
	if r.closed.Load() {
		return nil, errClosed
	}
	out := make([]Volume, 0, len(r.paths))
	for _, path := range r.paths {
		var sfs syscall.Statfs_t
		if err := syscall.Statfs(path, &sfs); err != nil {
			return nil, fmt.Errorf("statfs %s: %w", path, err)
		}
		out = append(out, volumeFrom(path, uint64(sfs.Bsize), uint64(sfs.Blocks), uint64(sfs.Bfree), uint64(sfs.Bavail), "dev", "dev"))
	}
	return out, nil
}
