//go:build linux || darwin

package action

import (
	"os"
	"syscall"
)

// promotionFileFingerprint is unavailable without an inode number or a
// change time: some network mounts synthesize neither, and then only a full
// hash can tell.
func promotionFileFingerprint(info os.FileInfo) (promotionFingerprint, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Ino == 0 {
		return promotionFingerprint{}, false
	}
	ctime := promotionCtimeNs(st)
	if ctime == 0 {
		return promotionFingerprint{}, false
	}
	return promotionFingerprint{
		Size:    info.Size(),
		MtimeNs: info.ModTime().UnixNano(),
		CtimeNs: ctime,
		Dev:     uint64(st.Dev),
		Ino:     uint64(st.Ino),
	}, true
}
