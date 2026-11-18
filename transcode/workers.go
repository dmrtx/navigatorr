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
// is a durable identity. Renaming a worker is removing it and adding another,
// unless the old name is kept as an alias: an alias is a former name that still
// finds the worker (a client with the old name keeps working) and that the
// server rewrites to the new name in what it stored (see store.RenameWorker).

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
	// Aliases are former names of this worker. They resolve to it, never
	// name another worker, and are valid worker names.
	Aliases []string
	// Address is the worker's host, for display only.
	Address string
	// Podcast marks the one worker that runs podcast work. Its ad catalog
	// lives in that worker's state directory, so podcast work never moves.
	Podcast bool
	// MaxParallelJobs caps the video jobs the server assigns this worker at
	// once; 0 follows the worker's own video pool.
	MaxParallelJobs int
	// PublishPolicy is "" (on_encode) or PublishPolicyOnApproval: when this
	// worker copies a finished candidate to the share.
	PublishPolicy string
}

// WorkerSet is the configured workers in config order. Config order breaks
// every tie, so scheduling is deterministic. A nil set has no workers.
type WorkerSet struct {
	list []NamedWorker
	// aliases maps a former name to the position of its worker in list.
	aliases map[string]int
}

// NewWorkerSet checks a worker list: at least one worker, valid unique names,
// an executor each, and exactly one podcast worker.
func NewWorkerSet(list []NamedWorker) (*WorkerSet, error) {
	if len(list) == 0 {
		return nil, fmt.Errorf("transcode workers: at least one worker is required")
	}
	names := make(map[string]bool, len(list))
	for _, w := range list {
		names[w.Name] = true
	}
	aliases := map[string]int{}
	podcast := 0
	used := make(map[string]bool, len(list))
	for i, w := range list {
		if !ValidWorkerName(w.Name) {
			return nil, fmt.Errorf("transcode workers: invalid worker name %q (lowercase letters, digits and dashes)", w.Name)
		}
		if used[w.Name] {
			return nil, fmt.Errorf("transcode workers: worker name %q is used twice", w.Name)
		}
		used[w.Name] = true
		for _, alias := range w.Aliases {
			if !ValidWorkerName(alias) {
				return nil, fmt.Errorf("transcode workers: invalid alias %q for worker %q (lowercase letters, digits and dashes)", alias, w.Name)
			}
			if names[alias] {
				return nil, fmt.Errorf("transcode workers: alias %q of worker %q is also a worker name", alias, w.Name)
			}
			if alias == DefaultWorkerName {
				return nil, fmt.Errorf("transcode workers: %q cannot be an alias (it already means the legacy worker)", alias)
			}
			if _, dup := aliases[alias]; dup {
				return nil, fmt.Errorf("transcode workers: alias %q is used twice", alias)
			}
			aliases[alias] = i
		}
		if w.Executor == nil {
			return nil, fmt.Errorf("transcode workers: worker %q has no executor", w.Name)
		}
		if w.MaxParallelJobs < 0 {
			return nil, fmt.Errorf("transcode workers: worker %q max_parallel_jobs must not be negative", w.Name)
		}
		if policy, err := NormalizePublishPolicy(w.PublishPolicy); err != nil || policy != w.PublishPolicy {
			return nil, fmt.Errorf("transcode workers: worker %q has publish_policy %q", w.Name, w.PublishPolicy)
		}
		if w.Podcast {
			podcast++
		}
	}
	if podcast != 1 {
		return nil, fmt.Errorf("transcode workers: exactly one worker must run podcast work, %d do", podcast)
	}
	return &WorkerSet{list: append([]NamedWorker(nil), list...), aliases: aliases}, nil
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

// Get returns the worker with this name, or whose former name (alias) it is.
func (s *WorkerSet) Get(name string) (NamedWorker, bool) {
	if s == nil {
		return NamedWorker{}, false
	}
	for _, w := range s.list {
		if w.Name == name {
			return w, true
		}
	}
	if i, ok := s.aliases[name]; ok {
		return s.list[i], true
	}
	return NamedWorker{}, false
}

// Canonical is the current name of the worker this name (or alias) means, and
// the name itself when no worker has it.
func (s *WorkerSet) Canonical(name string) string {
	if w, ok := s.Get(name); ok {
		return w.Name
	}
	return name
}

// Renames lists every alias with the name it now stands for, in config order,
// for the store to rewrite what it kept under the former name.
func (s *WorkerSet) Renames() [][2]string {
	if s == nil {
		return nil
	}
	var out [][2]string
	for _, w := range s.list {
		for _, alias := range w.Aliases {
			out = append(out, [2]string{alias, w.Name})
		}
	}
	return out
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
