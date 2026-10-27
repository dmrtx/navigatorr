package nashelper

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"runtime"
	"time"
)

// volumeFrom turns statfs block counts into bytes.
func volumeFrom(root string, blockSize, blocks, free, avail uint64, fsid, fsType string) Volume {
	return Volume{
		Root: root, FSID: fsid, FSType: fsType,
		TotalBytes: int64(blocks * blockSize),
		FreeBytes:  int64(free * blockSize),
		AvailBytes: int64(avail * blockSize),
	}
}

func (s *Server) volumes(w http.ResponseWriter, _ *http.Request) {
	vols, err := s.roots.Volumes()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, VolumeResponse{InstanceID: s.instance, Volumes: vols})
}

// startUsage queues a walk that sums the regular files under a directory. Walks
// run at idle I/O priority: a library holds hundreds of thousands of entries, and
// the NAS has other work. Background walks (the default, and what the statistics
// ask for whole roots) run one at a time; a walk asked for with
// ?priority=interactive has its own lane, so a folder someone is looking at does
// not wait for them. An older caller sends no priority and gets the old lane.
func (s *Server) startUsage(w http.ResponseWriter, r *http.Request) {
	path, ok := decodePath(w, r)
	if !ok {
		return
	}
	idBytes := make([]byte, 16)
	if _, err := rand.Read(idBytes); err != nil {
		writeError(w, err)
		return
	}
	// A path that is not a served directory is refused before a job exists.
	dir, err := s.roots.OpenDir(path)
	if err != nil {
		writeError(w, err)
		return
	}
	dir.Close()

	ctx, cancel := context.WithCancel(s.baseCtx)
	j := &job{id: hex.EncodeToString(idBytes), path: path, cancel: cancel, state: JobQueued}
	j.asked.Store(time.Now().UnixNano())

	s.mu.Lock()
	s.expireLocked()
	if s.closed || len(s.jobs) >= maxJobs || s.activeLocked() >= maxActiveJobs {
		s.mu.Unlock()
		cancel()
		writeError(w, &Error{Code: CodeBusy, Message: "too many jobs are waiting or held"})
		return
	}
	s.jobs[j.id] = j
	s.wg.Add(1)
	s.mu.Unlock()

	go s.runUsage(ctx, j, r.URL.Query().Get("priority") == "interactive")
	writeJSON(w, http.StatusAccepted, s.snapshot(j))
}

func (s *Server) runUsage(ctx context.Context, j *job, interactive bool) {
	defer s.wg.Done()
	defer j.cancel()
	go s.reapIfAbandoned(ctx, j)
	sem := s.usageSem
	if interactive {
		sem = s.foldSem
	}
	select {
	case sem <- struct{}{}:
		defer func() { <-sem }()
	case <-ctx.Done():
		s.finishUsage(j, nil, ctx.Err())
		return
	}
	runtime.LockOSThread()
	lowerIOPriority()
	j.set(JobRunning)
	res, err := s.roots.Usage(ctx, j.path, func(bytes int64) { j.read.Store(bytes) })
	if err != nil {
		s.finishUsage(j, nil, err)
		return
	}
	s.finishUsage(j, &res, nil)
}

func (s *Server) finishUsage(j *job, res *UsageResult, err error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.finished = s.now()
	switch {
	case err == nil:
		j.state, j.usage = JobDone, res
	case isCancel(err):
		j.state, j.err = JobCancelled, &Error{Code: CodeCancelled, Message: "the job was cancelled"}
	default:
		j.state, j.err = JobFailed, asError(err)
	}
}

func isCancel(err error) bool { return errors.Is(err, context.Canceled) }
