package transcodeworker

import (
	"fmt"
	"io"
	"log"
	"os"
	"sync"
)

// DefaultLogMaxMB is the size the serve process lets its log file reach before
// it rotates it, when log_max_mb is not set.
const DefaultLogMaxMB = 20

// CappedLog is a writer for a log file that someone else opened, typically the
// regular file a service manager hands the daemon as stderr. When the next
// write would take the file past max bytes it keeps the newest max bytes as
// "<path>.1" (replacing the previous one) and truncates the file, so the log
// never holds more than about twice max across both files. The file's own
// descriptor stays in use, so the service manager needs no signal or restart.
type CappedLog struct {
	mu   sync.Mutex
	f    *os.File
	path string // where f lives; empty when the platform cannot tell, then the old text is dropped
	max  int64
	size int64
}

// NewCappedLog wraps f, which must be a regular file, with a cap of max bytes.
func NewCappedLog(f *os.File, max int64) (*CappedLog, error) {
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", f.Name())
	}
	if max <= 0 {
		return nil, fmt.Errorf("log cap must be positive")
	}
	c := &CappedLog{f: f, path: openFilePath(f), max: max, size: fi.Size()}
	if c.size > c.max {
		c.rotateLocked() // a log that grew without a cap is cut down at once
	}
	return c, nil
}

func (c *CappedLog) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.size > 0 && c.size+int64(len(p)) > c.max {
		c.rotateLocked()
	}
	n, err := c.f.Write(p)
	c.size += int64(n)
	return n, err
}

// rotateLocked is best effort: a failure to keep the old text must never stop
// the daemon from logging or from truncating, which is what protects the disk.
func (c *CappedLog) rotateLocked() {
	if c.path != "" {
		_ = c.keepTail(c.path+".1", c.max)
	}
	if err := c.f.Truncate(0); err != nil {
		return
	}
	// A descriptor opened without O_APPEND would otherwise write past the new
	// end and leave a hole the size of the old file.
	_, _ = c.f.Seek(0, io.SeekStart)
	c.size = 0
}

// keepTail writes the last n bytes of the current file to dst, replacing it.
func (c *CappedLog) keepTail(dst string, n int64) error {
	src, err := os.Open(c.path)
	if err != nil {
		return err
	}
	defer src.Close()
	fi, err := src.Stat()
	if err != nil {
		return err
	}
	if fi.Size() > n {
		if _, err := src.Seek(fi.Size()-n, io.SeekStart); err != nil {
			return err
		}
	}
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, cerr := io.Copy(out, src)
	if err := out.Close(); cerr == nil {
		cerr = err
	}
	if cerr != nil {
		_ = os.Remove(tmp)
		return cerr
	}
	return os.Rename(tmp, dst)
}

// CapStderrLog caps the daemon's log when stderr is a regular file, as it is
// under launchd or systemd with a log file configured, and routes the standard
// logger through the cap. It reports whether a cap was installed; a terminal or
// a pipe has no file to grow, and maxMB below zero turns the cap off. A zero
// maxMB selects DefaultLogMaxMB.
func CapStderrLog(maxMB int) (bool, error) {
	if maxMB < 0 {
		return false, nil
	}
	if maxMB == 0 {
		maxMB = DefaultLogMaxMB
	}
	fi, err := os.Stderr.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return false, nil
	}
	c, err := NewCappedLog(os.Stderr, int64(maxMB)<<20)
	if err != nil {
		return false, err
	}
	log.SetOutput(c)
	return true, nil
}
