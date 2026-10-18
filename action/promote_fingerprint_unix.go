//go:build linux || darwin

package action

import (
	"os"
	"syscall"
)

// promotionFileFingerprint is unavailable without an inode number: some
// network mounts synthesize none, and then only a full hash can tell.
func promotionFileFingerprint(info os.FileInfo) (promotionFingerprint, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Ino == 0 {
		return promotionFingerprint{}, false
	}
	return promotionFingerprint{
		Size:    info.Size(),
		MtimeNs: info.ModTime().UnixNano(),
		CtimeNs: promotionCtimeNs(st),
		Dev:     uint64(st.Dev),
		Ino:     uint64(st.Ino),
	}, true
}
