package transcodeworker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
)

// Room admission (docs/transcode/DEFERRED_CANDIDATE_PUBLICATION.md, "Room").
//
// A node never waits for room. When a deferred job's slot comes up, and under
// the capacity lock, the node works out
//
//	need = the source (unless this job's own copy is already cached)
//	       + the output reserve + the free-disk floor
//	can  = free disk, less the reserves of encodes that have not written yet,
//	       plus the cached sources it could delete
//
// and either deletes as few freeable sources as it needs, oldest first, and
// takes the job, or refuses it at once with the numbers. Freeable sources are
// cache entries that are not this job's and that no running job holds (a
// running job holds a hard link, so deleting the shared name would free
// nothing). Finished candidates waiting for approval and the files of running
// jobs are never freeable.

// Why a refused job was refused (JobRecord.RetentionBlocked).
const (
	retentionBlockedBudget = "retention_budget"
	retentionBlockedDisk   = "free_disk"
	retentionBlockedHold   = "node_held"
)

// retentionRefusal is a room limit the node enforced. Its message is worded as
// the "worker busy" error the API maps to 429 worker_busy.
type retentionRefusal struct {
	reason string
	msg    string
}

func (e *retentionRefusal) Error() string { return e.msg }

// cacheEntry is a shared source cache file.
type cacheEntry struct {
	name string
	size int64
	mod  time.Time
}

// freeableCacheEntries lists the cache entries a node may delete for a job
// whose own source is own ("": none), oldest first. An entry another job holds
// is a hard link and frees nothing, so it is left out.
func (w *Worker) freeableCacheEntries(own string) []cacheEntry {
	if !w.sourceCacheEnabled() {
		return nil
	}
	dir := w.sourceCacheDir()
	names, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []cacheEntry
	for _, e := range names {
		name := e.Name()
		if strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".meta.json") || strings.HasSuffix(name, ".lock") || strings.HasSuffix(name, ".part") {
			continue
		}
		if own != "" && sourceCacheKeyOfData(name) == own {
			continue
		}
		full := filepath.Join(dir, name)
		fi, err := os.Lstat(full)
		if err != nil || !fi.Mode().IsRegular() || !unlinkFreesBlocks(full) {
			continue
		}
		out = append(out, cacheEntry{name: name, size: fi.Size(), mod: fi.ModTime()})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].mod.Before(out[j].mod) })
	return out
}

// ownCacheKey is the cache key of the job's source when a complete copy is
// cached on this node, "" otherwise. The entry is found by what its sidecar
// records (path, size, modification time), since a request that carries no
// digest does not know the key before the copy.
func (w *Worker) ownCacheKey(job *JobRecord) string {
	if !w.sourceCacheEnabled() || job.RoomSourceBytes <= 0 {
		return ""
	}
	dir := w.sourceCacheDir()
	names, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	src := filepath.Clean(job.Source)
	for _, e := range names {
		name := e.Name()
		if !strings.HasSuffix(name, ".meta.json") || strings.HasPrefix(name, ".") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		var meta sourceIdentity
		if json.Unmarshal(raw, &meta) != nil {
			continue
		}
		if meta.Source != src || meta.Size != job.RoomSourceBytes || meta.ModTime != job.RoomSourceMTime {
			continue
		}
		key := strings.TrimSuffix(name, ".meta.json")
		for _, d := range names {
			dn := d.Name()
			if strings.HasSuffix(dn, ".meta.json") || sourceCacheKeyOfData(dn) != key {
				continue
			}
			if fi, err := os.Lstat(filepath.Join(dir, dn)); err == nil && fi.Mode().IsRegular() && fi.Size() == meta.Size {
				return key
			}
		}
	}
	return ""
}

// deleteCacheEntry removes a cache entry and its sidecar.
func (w *Worker) deleteCacheEntry(e cacheEntry) bool {
	dir := w.sourceCacheDir()
	if err := os.Remove(filepath.Join(dir, e.name)); err != nil {
		return false
	}
	_ = os.Remove(cacheMetaPathForData(dir, e.name))
	return true
}

// ClearFreeableSourceCache deletes every source cache entry no running job
// holds. An immediate pause asks for it: the node is wanted for something else
// and keeps no spare copies. Finished candidates are untouched. It returns how
// many bytes were freed.
func (w *Worker) ClearFreeableSourceCache() (int64, error) {
	capLock, err := acquireCapacityLock(filepath.Clean(w.cfg.StateDir))
	if err != nil {
		return 0, err
	}
	defer capLock.Unlock()
	var freed int64
	for _, e := range w.freeableCacheEntries("") {
		if w.deleteCacheEntry(e) {
			freed += e.size
		}
	}
	if freed > 0 {
		log.Printf("source cache: cleared %d bytes for an immediate pause", freed)
	}
	return freed, nil
}

// holdRefusal words why the node takes no new job, "" when it takes jobs.
func (w *Worker) holdRefusal() (reason, msg string) {
	switch {
	case w.leaseHolds():
		return retentionBlockedHold, "worker busy (" + transcode.RoomRefusalTag + "): a build is running on this node"
	case w.Held() || w.Suspended():
		return retentionBlockedHold, "worker busy (" + transcode.RoomRefusalTag + "): this node is paused"
	}
	return "", ""
}

func gb(n int64) string { return fmt.Sprintf("%.1f GB", float64(n)/1e9) }

// admitRoomLocked applies the node's room limits to a deferred job whose slot
// has come up. It deletes the cached sources it needs to and returns nil when
// the job may start. fallback is true when the retention budget is full and the
// job did not ask to wait: it starts to publish on encode. A refusal is a
// *retentionRefusal; any other error is a measurement that failed. Callers hold
// the capacity lock.
func (w *Worker) admitRoomLocked(job *JobRecord) (fallback bool, err error) {
	limit, floor := w.cfg.RetainedCandidatesMaxBytes, w.cfg.MinFreeDiskBytes
	if limit <= 0 && floor <= 0 {
		return false, nil
	}
	held, unwritten, err := w.retainedBytesLocked()
	if err != nil {
		return false, fmt.Errorf("measuring retained candidates: %w", err)
	}
	reserve := job.RetainReserveBytes
	if limit > 0 && held+reserve > limit {
		if job.RetentionWait {
			return false, &retentionRefusal{reason: retentionBlockedBudget, msg: fmt.Sprintf("worker busy (%s): waiting for approvals on this node (%s held of %s allowed, this job needs %s)", transcode.RoomRefusalTag, gb(held), gb(limit), gb(reserve))}
		}
		fallback = true
	}
	if floor <= 0 {
		return fallback, nil
	}
	free, err := w.freeDiskBytes()
	if err != nil {
		return false, fmt.Errorf("measuring free disk: %w", err)
	}
	own := w.ownCacheKey(job)
	source := job.RoomSourceBytes
	if own != "" {
		source = 0 // already here
	}
	need := source + reserve + floor
	have := free - unwritten
	if have >= need {
		return fallback, nil
	}
	freeable := w.freeableCacheEntries(own)
	can := have
	for _, e := range freeable {
		can += e.size
	}
	if can < need {
		return false, &retentionRefusal{reason: retentionBlockedDisk, msg: fmt.Sprintf("worker busy (%s): the job needs %s (source %s, output %s, floor %s) and this node can offer %s", transcode.RoomRefusalTag, gb(need), gb(source), gb(reserve), gb(floor), gb(can))}
	}
	for _, e := range freeable {
		if have >= need {
			break
		}
		if w.deleteCacheEntry(e) {
			have += e.size
			log.Printf("source cache: removed %s (%d bytes) to make room for job %s", e.name, e.size, job.ID)
		}
	}
	return fallback, nil
}

// admitAtStartLocked applies the room limits to a queued job whose slot has
// come up, just before it is spawned. Only then does an on_approval job take
// disk: its source is staged and its candidate written as it runs.
//
// A refused job stays queued with the reason on its record; the coordinator
// reads it and takes the job back. Nothing here waits for room. Callers hold
// the capacity lock and the job's lock.
func (w *Worker) admitAtStartLocked(jobFile string, job *JobRecord) (bool, error) {
	if !job.deferredPublication() {
		return true, nil
	}
	var fallback bool
	var err error
	if reason, msg := w.holdRefusal(); reason != "" {
		err = &retentionRefusal{reason: reason, msg: msg}
	} else {
		fallback, err = w.admitRoomLocked(job)
	}
	var refusal *retentionRefusal
	switch {
	case errors.As(err, &refusal):
		if job.RetentionBlocked == refusal.reason && job.RoomNote == refusal.msg {
			return false, nil
		}
		job.RetentionBlocked, job.RoomNote = refusal.reason, refusal.msg
		log.Printf("job %s: %v", job.ID, err)
		return false, SaveJobAtomic(jobFile, job)
	case err != nil:
		return false, err
	}
	if fallback {
		// Saved before the spawn: the runner reads the policy it executes.
		job.PublishFallback = transcode.PublishFallbackRetentionFull
		log.Printf("job %s: the retention budget is full; starting it to publish on encode", job.ID)
	}
	if fallback || job.RetentionBlocked != "" {
		job.RetentionBlocked, job.RoomNote = "", ""
		return true, SaveJobAtomic(jobFile, job)
	}
	return true, nil
}

// roomReport measures what this node can take now, for the coordinator.
// Callers hold the capacity lock.
func (w *Worker) roomReport(now time.Time) (*transcode.RetentionRoom, error) {
	limit, floor := w.cfg.RetainedCandidatesMaxBytes, w.cfg.MinFreeDiskBytes
	held, unwritten, err := w.retainedBytesLocked()
	if err != nil {
		return nil, err
	}
	room := &transcode.RetentionRoom{ObservedAt: now, CapacityBytes: max(limit, 0), HeldBytes: held, FloorBytes: floor}
	if limit > 0 {
		room.RoomBytes = max(0, limit-held)
	}
	if w.leaseHolds() {
		room.Hold = "build"
	} else if w.Held() || w.Suspended() {
		room.Hold = "paused"
	}
	if floor > 0 {
		free, ferr := w.freeDiskBytes()
		total, terr := w.diskTotalBytes()
		if ferr == nil && terr == nil {
			can := free - unwritten
			for _, e := range w.freeableCacheEntries("") {
				can += e.size
			}
			room.DiskCapacityBytes = total
			room.DiskRoomBytes = max(0, can-floor)
		}
	}
	return room, nil
}

// diskTotalBytes is the size of the filesystem holding the local work directory.
func (w *Worker) diskTotalBytes() (int64, error) {
	dir := w.localWorkDir()
	if w.diskTotal != nil {
		return w.diskTotal(dir)
	}
	probe := dir
	for {
		if _, err := os.Stat(probe); err == nil {
			break
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			break
		}
		probe = parent
	}
	_, total, ok := diskSpace(probe)
	if !ok {
		return 0, fmt.Errorf("disk size of %s is unknown", probe)
	}
	return int64(total), nil
}

// diskHardFloor is the free disk under which a running encode is stopped and
// its job sent back to the queue: half the admission floor, so a job admitted
// above the floor stops only after the disk has fallen well past it.
func (w *Worker) diskHardFloor() int64 { return w.cfg.MinFreeDiskBytes / 2 }

// withDiskFloor lets the encode's watch stop the job when free disk falls under
// the hard floor. It arms a watch of its own when the plan has nothing else to
// judge.
func (w *Worker) withDiskFloor(ctx context.Context, job *JobRecord, outputPath, progressPath string) context.Context {
	if job == nil || !job.deferredPublication() || w.diskHardFloor() <= 0 {
		return ctx
	}
	watch := encodeSizeWatchFrom(ctx)
	if watch == nil {
		watch = &encodeSizeWatch{outputPath: outputPath, progressPath: progressPath}
		ctx = context.WithValue(ctx, sizeWatchContextKey{}, watch)
	}
	watch.floor = func() (int64, int64, bool) {
		free, err := w.freeDiskBytes()
		return free, w.diskHardFloor(), err == nil
	}
	return ctx
}
