//go:build linux

package action

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// promotionCloneFile reflinks src into dst with FICLONE.
func promotionCloneFile(dst, src *os.File) error {
	return promotionWithFds(dst, src, func(d, s int) error { return unix.IoctlFileClone(d, s) })
}

// promotionCopyRange copies up to n bytes from src's offset to dst's offset in
// the kernel, advancing both. It returns 0 at EOF.
func promotionCopyRange(dst, src *os.File, n int) (int, error) {
	var copied int
	err := promotionWithFds(dst, src, func(d, s int) error {
		for {
			var err error
			copied, err = unix.CopyFileRange(s, nil, d, nil, n, 0)
			if !errors.Is(err, unix.EINTR) {
				return err
			}
		}
	})
	return copied, err
}

func promotionWithFds(dst, src *os.File, fn func(d, s int) error) error {
	dc, err := dst.SyscallConn()
	if err != nil {
		return err
	}
	sc, err := src.SyscallConn()
	if err != nil {
		return err
	}
	var opErr error
	if err := dc.Control(func(d uintptr) {
		if err := sc.Control(func(s uintptr) { opErr = fn(int(d), int(s)) }); err != nil {
			opErr = err
		}
	}); err != nil {
		return err
	}
	return opErr
}
