package transcodeworker

import (
	"fmt"
	"os"
)

// openFilePath returns the path an open file currently has, or "".
func openFilePath(f *os.File) string {
	p, err := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", f.Fd()))
	if err != nil {
		return ""
	}
	return p
}
