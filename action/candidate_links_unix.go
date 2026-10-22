//go:build linux || darwin

package action

import (
	"os"
	"syscall"
)

// candidateLastLink reports whether removing this name frees the file's
// bytes: it is the only link left. An unknown link count frees nothing, so
// the reported space is never overstated.
func candidateLastLink(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && st.Nlink == 1
}
