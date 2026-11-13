package transcodeworker

import (
	"os"
	"syscall"
	"unsafe"
)

// fGetPath is F_GETPATH from <sys/fcntl.h>.
const fGetPath = 50

// openFilePath returns the path an open file currently has, or "".
func openFilePath(f *os.File) string {
	var buf [1024]byte // MAXPATHLEN
	if _, _, errno := syscall.Syscall(syscall.SYS_FCNTL, f.Fd(), fGetPath, uintptr(unsafe.Pointer(&buf[0]))); errno != 0 {
		return ""
	}
	for i, b := range buf {
		if b == 0 {
			return string(buf[:i])
		}
	}
	return ""
}
