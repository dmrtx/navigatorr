//go:build linux

package fsop

import "golang.org/x/sys/unix"

// renameNoReplace renames without ever replacing an existing destination.
// supported is false when the kernel or filesystem cannot do it.
func renameNoReplace(from, to string) (supported bool, err error) {
	err = unix.Renameat2(unix.AT_FDCWD, from, unix.AT_FDCWD, to, unix.RENAME_NOREPLACE)
	switch err {
	case unix.ENOSYS, unix.EINVAL, unix.ENOTSUP:
		return false, err
	}
	return true, err
}
