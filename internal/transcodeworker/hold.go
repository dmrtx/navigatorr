package transcodeworker

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// Hold.
//
// While the server's Pause all (worker drain) is on, the worker keeps every
// job it has already accepted but not started waiting in its queue: nothing
// leaves the queue, so only the encodes already running finish. A running job
// is never interrupted, and a job past its encode still finalizes and
// publishes. The server turns the hold on and off through /v1/hold and
// re-sends it until the worker confirms, and the state directory keeps it, so
// a restart of the worker does not release the queue.

const holdFile = "worker-hold.json"

type holdRecord struct {
	Hold      bool      `json:"hold"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (w *Worker) holdPath() string {
	if w == nil || w.cfg == nil || w.cfg.StateDir == "" {
		return ""
	}
	return filepath.Join(w.cfg.StateDir, holdFile)
}

// Held reports whether the worker is holding its queue. An unreadable record
// holds too: starting a job into a deploy is the worse mistake.
func (w *Worker) Held() bool {
	path := w.holdPath()
	if path == "" {
		return false
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	if err != nil {
		return true
	}
	var rec holdRecord
	if json.Unmarshal(data, &rec) != nil {
		return true
	}
	return rec.Hold
}

// SetHold turns the hold on or off. Turning it off removes the record and
// lets the next scheduler pass start the queue.
func (w *Worker) SetHold(hold bool) error {
	path := w.holdPath()
	if path == "" {
		return errors.New("worker has no state_dir to store the hold")
	}
	w.holdWrite.Lock()
	defer w.holdWrite.Unlock()
	if hold {
		// A scheduler pass in progress holds the capacity lock and may still be
		// starting jobs. Wait for it, so that once the hold is confirmed no
		// pass can start another job.
		capLock, err := acquireCapacityLock(filepath.Clean(w.cfg.StateDir))
		if err != nil {
			return err
		}
		defer capLock.Unlock()
	}
	if !hold {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	data, err := json.MarshalIndent(holdRecord{Hold: true, UpdatedAt: time.Now().UTC()}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "worker-hold-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}
