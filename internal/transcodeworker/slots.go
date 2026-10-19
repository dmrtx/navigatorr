package transcodeworker

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
)

// Per-kind worker slots.
//
// max_parallel_jobs stays the hard ceiling and the boot default: with nothing
// else configured every kind draws on one shared pool of that size, as before.
// An operator can divide the ceiling between video work (full conversions and
// benchmarks) and podcast work (transcription, ad detection and rendering); the
// server pushes the split to /v1/slots and the worker stores it in the state
// directory, where the daemon and every spawned job process read it. A change
// applies at the next admission: a running job is never interrupted, so a
// pool lowered below its current use simply admits nothing until it drains.

const slotSplitFile = "worker-slots.json"

// slotKind is the pool a job draws its slot from.
type slotKind int

const (
	slotKindVideo slotKind = iota
	slotKindPodcast
)

func (k slotKind) String() string {
	if k == slotKindPodcast {
		return "podcast"
	}
	return "video"
}

// jobSlotKind classifies a transcode-record job. Benchmarks are always video.
func jobSlotKind(job *JobRecord) slotKind {
	if job != nil && job.Plan != nil && job.Plan.Podcast != nil {
		return slotKindPodcast
	}
	return slotKindVideo
}

// slotSplitRecord is the stored split.
type slotSplitRecord struct {
	VideoSlots   int       `json:"video_slots"`
	PodcastSlots int       `json:"podcast_slots"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// slotLimits is the admission rule in force.
type slotLimits struct {
	Ceiling int
	Video   int // pool size for video work; the ceiling when !Split
	Podcast int // pool size for podcast work; the ceiling when !Split
	Split   bool
}

// hasRoom reports whether one more job of kind may start given the jobs
// already running. Without a split the two kinds share the ceiling.
func (l slotLimits) hasRoom(kind slotKind, video, podcast int) bool {
	if !l.Split {
		return video+podcast < l.Ceiling
	}
	if kind == slotKindPodcast {
		return podcast < l.Podcast
	}
	return video < l.Video
}

// videoPool is how many video jobs can run at once under these limits.
func (l slotLimits) videoPool() int {
	if !l.Split {
		return l.Ceiling
	}
	return l.Video
}

// slotSplitCache re-reads the split file only when its stat changes, so the
// hot admission and thread-sizing paths cost one stat call.
type slotSplitCache struct {
	mu      sync.Mutex
	modTime time.Time
	size    int64
	valid   bool
	rec     slotSplitRecord
}

func (w *Worker) slotCeiling() int {
	if w == nil || w.cfg == nil {
		return 1
	}
	return max(1, w.cfg.MaxParallelJobs)
}

func (w *Worker) slotSplitPath() string {
	if w == nil || w.cfg == nil || w.cfg.StateDir == "" {
		return ""
	}
	return filepath.Join(w.cfg.StateDir, slotSplitFile)
}

// slotLimits returns the split in force: the stored one when it is valid for
// the current ceiling, else one shared pool of the ceiling. A stored split
// that no longer fits (the ceiling was lowered) is ignored rather than
// scaled, so a worker never admits more than its ceiling.
func (w *Worker) slotLimits() slotLimits {
	ceiling := w.slotCeiling()
	shared := slotLimits{Ceiling: ceiling, Video: ceiling, Podcast: ceiling}
	path := w.slotSplitPath()
	if path == "" {
		return shared
	}
	c := &w.slotSplit
	c.mu.Lock()
	defer c.mu.Unlock()
	fi, err := os.Stat(path)
	if err != nil {
		c.valid, c.modTime, c.size = false, time.Time{}, 0
		return shared
	}
	if !c.modTime.Equal(fi.ModTime()) || c.size != fi.Size() {
		c.valid, c.modTime, c.size = false, fi.ModTime(), fi.Size()
		if data, err := os.ReadFile(path); err == nil {
			var rec slotSplitRecord
			if json.Unmarshal(data, &rec) == nil {
				c.rec, c.valid = rec, true
			}
		}
	}
	if !c.valid {
		return shared
	}
	if _, err := transcode.ValidateSlotSplit(transcode.SlotSplit{VideoSlots: c.rec.VideoSlots, PodcastSlots: c.rec.PodcastSlots}, ceiling, 0); err != nil || (c.rec.VideoSlots == 0 && c.rec.PodcastSlots == 0) {
		return shared
	}
	return slotLimits{Ceiling: ceiling, Video: c.rec.VideoSlots, Podcast: c.rec.PodcastSlots, Split: true}
}

// SetSlotSplit validates and stores a split, or clears it when both values are
// zero. It returns warnings about the CPU budget for a split that was applied.
func (w *Worker) SetSlotSplit(split transcode.SlotSplit) ([]string, error) {
	path := w.slotSplitPath()
	if path == "" {
		return nil, errors.New("worker has no state_dir to store the slot split")
	}
	warnings, err := transcode.ValidateSlotSplit(split, w.slotCeiling(), hostCPUCount())
	if err != nil {
		return nil, err
	}
	if split.Shared() {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		w.noteSlotSplitChanged()
		return nil, nil
	}
	data, err := json.MarshalIndent(slotSplitRecord{VideoSlots: split.VideoSlots, PodcastSlots: split.PodcastSlots, UpdatedAt: time.Now().UTC()}, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "worker-slots-*.tmp")
	if err != nil {
		return nil, err
	}
	name := tmp.Name()
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr); err != nil {
		_ = os.Remove(name)
		return nil, err
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return nil, err
	}
	w.noteSlotSplitChanged()
	return warnings, nil
}

// slotUse is a read-only view of the slots in use. Unlike
// countActiveJobsByKind it never reconciles a stopped runner, so the
// observational endpoints and thread sizing leave job records alone.
type slotUse struct {
	Video, Podcast int
	Queued         []*JobRecord // durable queued jobs without a runner yet
	kinds          map[string]slotKind
}

// observeSlotUse scans the state directory. A job or benchmark counts when its
// record says running or queued with a runner PID; with checkAlive a runner
// that is no longer alive does not.
func (w *Worker) observeSlotUse(checkAlive bool) (slotUse, error) {
	use := slotUse{kinds: map[string]slotKind{}}
	entries, err := os.ReadDir(w.cfg.StateDir)
	if err != nil {
		return use, err
	}
	add := func(kind slotKind) {
		if kind == slotKindPodcast {
			use.Podcast++
		} else {
			use.Video++
		}
	}
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		dir := filepath.Join(w.cfg.StateDir, entry.Name())
		if job, err := LoadJob(filepath.Join(dir, "job.json")); err == nil && job.ID == entry.Name() {
			use.kinds[job.ID] = jobSlotKind(job)
			if (job.Status == "running" || job.Status == "queued") && job.PID > 0 {
				if !checkAlive || w.jobAlive(job) {
					add(jobSlotKind(job))
				}
			} else if job.Status == "queued" {
				use.Queued = append(use.Queued, job)
			}
		}
		if bench, err := LoadBenchmark(filepath.Join(dir, "benchmark.json")); err == nil && bench.ID == entry.Name() {
			if (bench.Status == "running" || bench.Status == "queued") && bench.PID > 0 {
				if !checkAlive || IsBenchmarkExecutionAlive(bench) {
					add(slotKindVideo)
				}
			}
		}
	}
	return use, nil
}

// slotsStatus reports the limits and current use for /v1/ready.
func (w *Worker) slotsStatus() transcode.WorkerSlots {
	l := w.slotLimits()
	out := transcode.WorkerSlots{Ceiling: l.Ceiling, HostCPUs: hostCPUCount(), Split: l.Split, Video: transcode.SlotPool{Total: l.Video}, Podcast: transcode.SlotPool{Total: l.Podcast}}
	if use, err := w.observeSlotUse(false); err == nil {
		out.Video.Used, out.Podcast.Used = use.Video, use.Podcast
	}
	return out
}

// videoInUseTTL bounds how stale the cached count of running video jobs can
// get. The count only sizes thread pools for a job about to start, and a scan
// of the state directory reads every job record.
const videoInUseTTL = 3 * time.Second

type videoInUseCache struct {
	mu    sync.Mutex
	at    time.Time
	video int
	ok    bool
}

// videoJobsSharingCPU is how many video jobs the host CPUs are shared between
// right now: the video jobs and benchmarks running, which include the caller
// when it is one. With none counted (or the count unavailable) it assumes the
// video pool is full, which never gives a job more threads than its share.
func (w *Worker) videoJobsSharingCPU() int {
	if w == nil || w.cfg == nil {
		return 1
	}
	if w.videoInUseFn != nil {
		if n, ok := w.videoInUseFn(); ok && n > 0 {
			return n
		}
		return max(1, w.slotLimits().videoPool())
	}
	c := &w.videoInUse
	c.mu.Lock()
	if time.Since(c.at) > videoInUseTTL || c.at.IsZero() {
		use, err := w.observeSlotUse(true)
		c.at, c.video, c.ok = time.Now(), use.Video, err == nil
	}
	n, ok := c.video, c.ok
	c.mu.Unlock()
	if ok && n > 0 {
		return n
	}
	return max(1, w.slotLimits().videoPool())
}

// noteSlotSplitChanged drops caches that depend on the split and re-proves the
// x265 partitions the new split can produce.
func (w *Worker) noteSlotSplitChanged() {
	w.videoInUse.mu.Lock()
	w.videoInUse.at = time.Time{}
	w.videoInUse.mu.Unlock()
	w.slotSplit.mu.Lock()
	w.slotSplit.modTime = time.Time{}
	w.slotSplit.mu.Unlock()
	w.recheckX265PartitionAfterSlotChange()
}

func slotBusyMessage(l slotLimits, kind slotKind) string {
	if !l.Split {
		return fmt.Sprintf("worker busy: maximum parallel jobs (%d) reached", l.Ceiling)
	}
	return fmt.Sprintf("worker busy: all %d %s slots are in use", map[slotKind]int{slotKindVideo: l.Video, slotKindPodcast: l.Podcast}[kind], kind)
}
