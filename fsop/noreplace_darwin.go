//go:build darwin

package fsop

import "golang.org/x/sys/unix"

// renameNoReplace renames without ever replacing an existing destination.
// supported is false when the filesystem cannot do it.
func renameNoReplace(from, to string) (supported bool, err error) {
	err = unix.RenamexNp(from, to, unix.RENAME_EXCL)
	switch err {
	case unix.ENOSYS, unix.EINVAL, unix.ENOTSUP:
		return false, err
	}
	return true, err
}
