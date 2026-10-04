//go:build darwin

package action

import "golang.org/x/sys/unix"

func renamePromotionNoReplace(from, to string) error {
	return unix.RenameatxNp(unix.AT_FDCWD, from, unix.AT_FDCWD, to, unix.RENAME_EXCL)
}
