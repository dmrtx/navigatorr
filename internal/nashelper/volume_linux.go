package nashelper

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// Volumes reports the space of the filesystem under each served root.
func (r *Roots) Volumes() ([]Volume, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.fds == nil {
		return nil, errClosed
	}
	out := make([]Volume, 0, len(r.fds))
	for i, fd := range r.fds {
		var sfs unix.Statfs_t
		if err := unix.Fstatfs(fd, &sfs); err != nil {
			return nil, fmt.Errorf("statfs %s: %w", r.paths[i], err)
		}
		out = append(out, volumeFrom(r.paths[i], uint64(sfs.Bsize), sfs.Blocks, sfs.Bfree, sfs.Bavail,
			fmt.Sprintf("%08x%08x", uint32(sfs.Fsid.Val[0]), uint32(sfs.Fsid.Val[1])), fmt.Sprintf("%x", int64(sfs.Type))))
	}
	return out, nil
}
