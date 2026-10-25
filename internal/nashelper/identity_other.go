//go:build !linux

package nashelper

import (
	"os"
	"syscall"
)

// identityOf on a development machine: the fields every platform has. Subvolume
// and generation stay empty, so Complete is false.
func identityOf(f *os.File) (Identity, error) {
	info, err := f.Stat()
	if err != nil {
		return Identity{}, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return Identity{}, os.ErrInvalid
	}
	return Identity{
		FSID:    "dev",
		FSType:  "dev",
		Ino:     uint64(st.Ino),
		Size:    info.Size(),
		MtimeNs: info.ModTime().UnixNano(),
		CtimeNs: statCtimeNs(st),
	}, nil
}

func lowerIOPriority() {}
