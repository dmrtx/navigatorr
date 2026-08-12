//go:build unix

package queue

import (
	"fmt"
	"os"
	"syscall"
)

// lockFile takes a non-blocking exclusive advisory lock, held until the file is
// closed. The lock is released automatically if the process dies, so a crash
// cannot leave a stale lock behind the way a marker file would.
func lockFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening queue lock %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("queue %s is already open in another navigatorr process", path)
	}
	return f, nil
}
