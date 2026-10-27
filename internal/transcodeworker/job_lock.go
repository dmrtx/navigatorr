package transcodeworker

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

type jobFileLock struct {
	file *os.File
}

func acquireJobLock(jobDir string) (*jobFileLock, error) {
	if err := os.MkdirAll(jobDir, 0755); err != nil {
		return nil, fmt.Errorf("creating job dir for lock: %w", err)
	}
	lockFile := filepath.Join(jobDir, ".lock")
	f, err := os.OpenFile(lockFile, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("opening lock file %s: %w", lockFile, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("locking %s: %w", lockFile, err)
	}
	return &jobFileLock{file: f}, nil
}

// acquireExistingJobLock locks a job directory that must already exist. Unlike
// acquireJobLock it never creates the directory: a caller holding a stale path
// (the job was moved to or from its hidden retained directory in the
// meantime) gets fs.ErrNotExist instead of an empty shell directory that would
// shadow the real one.
func acquireExistingJobLock(jobDir string) (*jobFileLock, error) {
	if fi, err := os.Stat(jobDir); err != nil {
		return nil, fmt.Errorf("job dir for lock: %w", err)
	} else if !fi.IsDir() {
		return nil, fmt.Errorf("job dir for lock: %s is not a directory", jobDir)
	}
	lockFile := filepath.Join(jobDir, ".lock")
	// O_CREATE is safe here: it cannot recreate the directory, so a concurrent
	// move makes it fail with fs.ErrNotExist.
	f, err := os.OpenFile(lockFile, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("opening lock file %s: %w", lockFile, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("locking %s: %w", lockFile, err)
	}
	return &jobFileLock{file: f}, nil
}

func acquireCapacityLock(stateDir string) (*jobFileLock, error) {
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		return nil, fmt.Errorf("creating state dir for capacity lock: %w", err)
	}
	lockFile := filepath.Join(stateDir, ".capacity.lock")
	f, err := os.OpenFile(lockFile, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("opening capacity lock file %s: %w", lockFile, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("locking %s: %w", lockFile, err)
	}
	return &jobFileLock{file: f}, nil
}

func (l *jobFileLock) Unlock() {
	if l != nil && l.file != nil {
		_ = syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
		_ = l.file.Close()
	}
}
