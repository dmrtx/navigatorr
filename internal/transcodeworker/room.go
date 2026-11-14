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
	"syscall"
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
	// A cached source some admitted job chose as its own copy is that job's
	// until it links it, whoever asks.
	// When the jobs cannot be read nothing is known to be spare.
	taken := map[string]bool{}
	jobs, err := w.startedJobs()
	if err != nil {
		return nil
	}
	for _, j := range jobs {
		if j.RoomOwnKey != "" {
			taken[j.RoomOwnKey] = true
		}
	}
	var out []cacheEntry
	for _, e := range names {
		name := e.Name()
		if taken[sourceCacheKeyOfData(name)] {
			continue
		}
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
	if !w.sourceCacheEnabled() || job.sourceBytesKnown() <= 0 || job.RoomSourceMTime == 0 {
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
		if meta.Source != src || meta.Size != job.sourceBytesKnown() || meta.ModTime != job.RoomSourceMTime {
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
	w.dropHalfCopies()
	held, unwritten, err := w.retainedBytesLocked()
	if err != nil {
		return false, fmt.Errorf("measuring retained candidates: %w", err)
	}
	reserve := job.RetainReserveBytes
	// The retention budget is only for a job whose candidate waits for
	// approval; one that publishes on encode keeps nothing here.
	if limit > 0 && job.deferredPublication() && held+reserve > limit {
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
	job.RoomOwnKey = own
	source := job.sourceBytesKnown()
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
	if have < need {
		// A delete failed: never admit on room the node does not have.
		return false, &retentionRefusal{reason: retentionBlockedDisk, msg: fmt.Sprintf("worker busy (%s): the job needs %s (source %s, output %s, floor %s) and this node could free only %s", transcode.RoomRefusalTag, gb(need), gb(source), gb(reserve), gb(floor), gb(have))}
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
	var fallback bool
	var err error
	prevOwn := job.RoomOwnKey
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
	if fallback || job.RetentionBlocked != "" || job.RoomOwnKey != prevOwn {
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
	// A work directory that does not exist yet reports no capacity: the
	// coordinator then cannot say the job never fits.
	if _, err := os.Stat(dir); err != nil {
		return 0, err
	}
	probe := dir
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
	if job == nil || w.diskHardFloor() <= 0 {
		return ctx
	}
	watch := encodeSizeWatchFrom(ctx)
	if watch == nil {
		watch = &encodeSizeWatch{outputPath: outputPath, progressPath: progressPath}
		ctx = context.WithValue(ctx, sizeWatchContextKey{}, watch)
	}
	id := job.ID
	w.armFloor(ctx, id)
	watch.floor = func() (int64, int64, bool) {
		free, err := w.freeDiskBytes()
		// Only the encode started last stops: the others may well finish
		// once it has gone, and stopping them all would lose all that work.
		return free, w.diskHardFloor(), err == nil && w.newestEncode(id)
	}
	return ctx
}

// halfCopyIdle is how long a partial cache file must have gone unwritten before
// a sweep may take it, so a copy that has only just been created is never
// raced.
const halfCopyIdle = 30 * time.Second

// dropHalfCopies deletes partial cache files (.cache-stage-* and .cache-meta-*)
// that no live copy holds. A running copy keeps an exclusive lock on its file
// and keeps writing it; a copy whose process died holds neither, so its
// leftover is free disk again. It returns the bytes it freed. Callers hold the
// capacity lock or run at start.
func (w *Worker) dropHalfCopies() int64 {
	if !w.sourceCacheEnabled() {
		return 0
	}
	dir := w.sourceCacheDir()
	names, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	var freed int64
	for _, e := range names {
		name := e.Name()
		if !strings.HasPrefix(name, ".cache-stage-") && !strings.HasPrefix(name, ".cache-meta-") {
			continue
		}
		full := filepath.Join(dir, name)
		f, err := os.Open(full)
		if err != nil {
			continue
		}
		fi, serr := f.Stat()
		if serr != nil || !fi.Mode().IsRegular() || time.Since(fi.ModTime()) < halfCopyIdle ||
			syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
			_ = f.Close()
			continue
		}
		if os.Remove(full) == nil {
			freed += fi.Size()
			log.Printf("source cache: removed the half copy %s (%d bytes)", name, fi.Size())
		}
		_ = f.Close()
	}
	return freed
}

// armFloor records that the encode of job id has a disk-floor watch, until ctx
// ends. Only encodes with a watch can be stopped, so only they are compared.
func (w *Worker) armFloor(ctx context.Context, id string) {
	w.armedMu.Lock()
	if w.armedFloors == nil {
		w.armedFloors = map[string]int64{}
	}
	w.armedSeq++
	seq := w.armedSeq
	w.armedFloors[id] = seq
	w.armedMu.Unlock()
	context.AfterFunc(ctx, func() {
		w.armedMu.Lock()
		defer w.armedMu.Unlock()
		if w.armedFloors[id] == seq {
			delete(w.armedFloors, id)
		}
	})
}

// newestEncode says whether job id is the encode armed last on this node (the
// one that stops first when the disk runs under its hard floor). A job with no
// armed watch is never compared: a newer job still copying its source cannot
// be stopped, so it must not spare an older encode.
func (w *Worker) newestEncode(id string) bool {
	w.armedMu.Lock()
	defer w.armedMu.Unlock()
	mine, ok := w.armedFloors[id]
	if !ok {
		return true
	}
	for _, seq := range w.armedFloors {
		if seq > mine {
			return false
		}
	}
	return true
}
