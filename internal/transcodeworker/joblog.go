package transcodeworker

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Job-log bounding constants. ffmpeg.log is append-only runner output; reads
// are always capped at MaxJobLogTailBytes regardless of caller input so memory
// and response size stay bounded no matter how large the log grew.
const (
	// DefaultJobLogTailBytes is the tail size returned when the caller does not
	// request a specific size.
	DefaultJobLogTailBytes int64 = 64 * 1024
	// MaxJobLogTailBytes is the hard upper bound on any job-log read/response.
	MaxJobLogTailBytes int64 = 256 * 1024
)

// ErrJobLogNotFound distinguishes a missing per-job ffmpeg.log from an empty
// log and from other read failures. Callers map it to HTTP 404.
var ErrJobLogNotFound = errors.New("job log not found")

// IsJobLogNotFound reports whether err wraps ErrJobLogNotFound.
func IsJobLogNotFound(err error) bool {
	return errors.Is(err, ErrJobLogNotFound)
}

// JobLog is a bounded tail of a job's ffmpeg.log. Truncated reports whether
// earlier bytes were dropped because the file is larger than the returned
// tail; SizeBytes is the full on-disk size so a truncated tail is unambiguous.
type JobLog struct {
	JobID     string `json:"job_id"`
	Content   string `json:"content"`
	Truncated bool   `json:"truncated"`
	SizeBytes int64  `json:"size_bytes"`
}

// readBoundedTail reads at most max bytes from the tail of path without ever
// loading the whole file into memory. max<=0 selects DefaultJobLogTailBytes;
// any max above MaxJobLogTailBytes is clamped down to the hard bound. It
// returns the tail bytes, the full file size, and whether earlier bytes were
// dropped (file strictly larger than the returned tail). The open is
// no-follow, so a symlinked final path component is rejected rather than
// read (see openNoFollow); the Lstat pre-checks in ReadJobLog handle the
// directory components and the message, this closes the remaining race.
func readBoundedTail(path string, max int64) ([]byte, int64, bool, error) {
	if max <= 0 {
		max = DefaultJobLogTailBytes
	}
	if max > MaxJobLogTailBytes {
		max = MaxJobLogTailBytes
	}

	f, err := openNoFollow(path)
	if err != nil {
		return nil, 0, false, err
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return nil, 0, false, err
	}
	if fi.IsDir() {
		return nil, 0, false, fmt.Errorf("job log %s is a directory", path)
	}

	size := fi.Size()
	if size <= max {
		data := make([]byte, size)
		if _, err := io.ReadFull(f, data); err != nil {
			return nil, 0, false, err
		}
		return data, size, false, nil
	}

	if _, err := f.Seek(size-max, io.SeekStart); err != nil {
		return nil, 0, false, err
	}
	data := make([]byte, max)
	if _, err := io.ReadFull(f, data); err != nil {
		return nil, 0, false, err
	}
	return data, size, true, nil
}

// jobLogError maps a raw filesystem error to a job-log error. Genuine
// not-exist becomes ErrJobLogNotFound (HTTP 404); everything else (including
// symlink rejection) stays a plain fail-closed error (HTTP 500), never a
// successful read.
func jobLogError(jobID string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: job %q", ErrJobLogNotFound, jobID)
	}
	return fmt.Errorf("reading job log for %q: %w", jobID, err)
}

// ensureRealDirNoSymlink fails closed unless path names an existing real
// directory that is not itself a symlink. Any symlink (inside or outside
// StateDir) is rejected so a job directory can never be redirected.
func ensureRealDirNoSymlink(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("job log directory %s is a symlink (fail closed)", path)
	}
	if !fi.IsDir() {
		return fmt.Errorf("job log path %s is not a directory (fail closed)", path)
	}
	return nil
}

// ensureRealFileNoSymlink fails closed unless path names an existing real
// non-symlink entry. Missing paths return the raw not-exist error so callers
// preserve 404 semantics; symlinks are rejected, never followed.
func ensureRealFileNoSymlink(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("job log %s is a symlink (fail closed)", path)
	}
	return nil
}

// ensureResolvedWithinStateDir is defense in depth: after resolving symlinks
// in the parent components, the job directory must still live inside the
// resolved StateDir. The job directory itself is already proven not to be a
// symlink, so this can only fire if StateDir resolution itself escapes.
func ensureResolvedWithinStateDir(stateDir, jobDir string) error {
	realState, err := filepath.EvalSymlinks(stateDir)
	if err != nil {
		return err
	}
	realJob, err := filepath.EvalSymlinks(jobDir)
	if err != nil {
		return err
	}
	if realJob != realState && !strings.HasPrefix(realJob, realState+string(os.PathSeparator)) {
		return fmt.Errorf("resolved job log path %s escapes state dir %s (fail closed)", realJob, realState)
	}
	return nil
}

// ReadJobLog returns a bounded tail of <StateDir>/<jobID>/ffmpeg.log. The job
// ID is validated with the shared fail-closed path-safety rules and the
// resolved path is proven to remain within StateDir. Symlink hardening: the
// job directory must be a real directory and ffmpeg.log a real regular entry,
// neither may be a symlink (inside or outside StateDir), and the open is
// no-follow so a redirected log can never be read. A missing log is a
// distinguishable ErrJobLogNotFound, never an empty success. tailBytes<=0
// selects the default; larger values are clamped to the hard max.
func (w *Worker) ReadJobLog(jobID string, tailBytes int64) (JobLog, error) {
	if w == nil || w.cfg == nil {
		return JobLog{}, errors.New("worker is not configured")
	}
	if err := ValidateTranscodeJobID(jobID); err != nil {
		return JobLog{}, err
	}

	cleanStateDir := filepath.Clean(w.cfg.StateDir)
	jobDir := filepath.Join(cleanStateDir, jobID)
	rel, err := filepath.Rel(cleanStateDir, jobDir)
	if err != nil || rel != jobID || strings.HasPrefix(rel, "..") ||
		strings.ContainsRune(rel, os.PathSeparator) ||
		strings.ContainsRune(rel, '/') || strings.ContainsRune(rel, '\\') {
		return JobLog{}, fmt.Errorf("invalid transcode job id %q (path traversal)", jobID)
	}

	if err := ensureRealDirNoSymlink(jobDir); err != nil {
		return JobLog{}, jobLogError(jobID, err)
	}
	logPath := filepath.Join(jobDir, "ffmpeg.log")
	if err := ensureRealFileNoSymlink(logPath); err != nil {
		return JobLog{}, jobLogError(jobID, err)
	}
	if err := ensureResolvedWithinStateDir(cleanStateDir, jobDir); err != nil {
		return JobLog{}, jobLogError(jobID, err)
	}

	data, size, truncated, err := readBoundedTail(logPath, tailBytes)
	if err != nil {
		return JobLog{}, jobLogError(jobID, err)
	}
	return JobLog{JobID: jobID, Content: string(data), Truncated: truncated, SizeBytes: size}, nil
}
