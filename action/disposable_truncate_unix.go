//go:build linux || darwin

package action

import (
	"errors"
	"os"
	"syscall"
)

// truncateDisposable empties the file at path if it is still the one want
// describes. The file is opened without following a symlink or waiting on a
// special file, and checked on the open descriptor, so a name swapped after
// the caller's Lstat is never emptied: same file, size, modification time and
// no other link.
func truncateDisposable(path string, want os.FileInfo) error {
	f, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	got, err := f.Stat()
	if err != nil {
		return err
	}
	if !got.Mode().IsRegular() || !os.SameFile(want, got) || got.Size() != want.Size() || !got.ModTime().Equal(want.ModTime()) {
		return errors.New("the file changed since it was checked")
	}
	if !candidateLastLink(got) {
		return errors.New("another name may still link to the file")
	}
	return f.Truncate(0)
}
