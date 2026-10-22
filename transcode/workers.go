package transcode

import (
	"fmt"
	"regexp"
)

// Workers.
//
// The server drives one or more named transcode workers. A job belongs to the
// worker it was assigned when its action was admitted: every status read,
// cancel and cleanup for it goes to that worker and no other, so a worker name
// is a durable identity. Renaming a worker is removing it and adding another.

// DefaultWorkerName names the one worker of the single base_url config form,
// and the node the per-node slot settings used before workers had names.
const DefaultWorkerName = "video-worker"

var workerNameRegex = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// ValidWorkerName reports whether name can name a worker: lowercase letters,
// digits and dashes, starting with a letter or digit.
func ValidWorkerName(name string) bool { return workerNameRegex.MatchString(name) }

// NamedWorker is one configured worker.
type NamedWorker struct {
	Name     string
	Executor Executor
	// Address is the worker's host, for display only.
	Address string
	// Podcast marks the one worker that runs podcast work. Its ad catalog
	// lives in that worker's state directory, so podcast work never moves.
	Podcast bool
	// MaxParallelJobs caps the video jobs the server assigns this worker at
	// once; 0 follows the worker's own video pool.
	MaxParallelJobs int
}

// WorkerSet is the configured workers in config order. Config order breaks
// every tie, so scheduling is deterministic. A nil set has no workers.
type WorkerSet struct {
	list []NamedWorker
}

// NewWorkerSet checks a worker list: at least one worker, valid unique names,
// an executor each, and exactly one podcast worker.
func NewWorkerSet(list []NamedWorker) (*WorkerSet, error) {
	if len(list) == 0 {
		return nil, fmt.Errorf("transcode workers: at least one worker is required")
	}
	seen := make(map[string]bool, len(list))
	podcast := 0
	for _, w := range list {
		if !ValidWorkerName(w.Name) {
			return nil, fmt.Errorf("transcode workers: invalid worker name %q (lowercase letters, digits and dashes)", w.Name)
		}
		if seen[w.Name] {
			return nil, fmt.Errorf("transcode workers: worker name %q is used twice", w.Name)
		}
		seen[w.Name] = true
		if w.Executor == nil {
			return nil, fmt.Errorf("transcode workers: worker %q has no executor", w.Name)
		}
		if w.MaxParallelJobs < 0 {
			return nil, fmt.Errorf("transcode workers: worker %q max_parallel_jobs must not be negative", w.Name)
		}
		if w.Podcast {
			podcast++
		}
	}
	if podcast != 1 {
		return nil, fmt.Errorf("transcode workers: exactly one worker must run podcast work, %d do", podcast)
	}
	return &WorkerSet{list: append([]NamedWorker(nil), list...)}, nil
}

// SingleWorker is a set of one worker named DefaultWorkerName that also runs
// podcast work. A nil executor gives a nil set.
func SingleWorker(executor Executor) *WorkerSet {
	if executor == nil {
		return nil
	}
	return &WorkerSet{list: []NamedWorker{{Name: DefaultWorkerName, Executor: executor, Podcast: true}}}
}

// Len is the number of workers.
func (s *WorkerSet) Len() int {
	if s == nil {
		return 0
	}
	return len(s.list)
}

// All returns the workers in config order.
func (s *WorkerSet) All() []NamedWorker {
	if s == nil {
		return nil
	}
	return append([]NamedWorker(nil), s.list...)
}

// Get returns the worker with this name.
func (s *WorkerSet) Get(name string) (NamedWorker, bool) {
	if s == nil {
		return NamedWorker{}, false
	}
	for _, w := range s.list {
		if w.Name == name {
			return w, true
		}
	}
	return NamedWorker{}, false
}

// Podcast returns the worker that runs podcast work.
func (s *WorkerSet) Podcast() (NamedWorker, bool) {
	if s == nil {
		return NamedWorker{}, false
	}
	for _, w := range s.list {
		if w.Podcast {
			return w, true
		}
	}
	return NamedWorker{}, false
}

// WorkerNotConfiguredError is a job's worker missing from the configuration.
// The job is not moved: it waits until the worker is configured again or the
// operator cancels it.
type WorkerNotConfiguredError struct{ Name string }

func (e *WorkerNotConfiguredError) Error() string {
	return fmt.Sprintf("worker %q is not configured on this server", e.Name)
}
