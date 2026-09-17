//go:build windows

package smbprobe

import (
	"errors"
	"os"
)

func openSecretFile(path string) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("password_file must not be a symlink")
	}
	return os.Open(path)
}
