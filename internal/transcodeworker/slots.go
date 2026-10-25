package transcodeworker

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
)

// Per-kind worker slots.
//
// The ceiling is how many jobs the worker runs at once. max_parallel_jobs is
// its boot default: with nothing else configured every kind draws on one
// shared pool of that size, as before. An operator can override the ceiling
// (1 up to max_parallel_jobs_limit, else the host CPU count) and divide it
// between video work (full conversions) and podcast work (transcription, ad
// detection and rendering); the server pushes both to /v1/slots and the
// worker stores them in the state directory, where the daemon and every
// spawned job process read them. A change applies at the next admission: a
// running job is never interrupted, so a ceiling or pool lowered below its
// current use simply admits nothing until it drains. Lowering the ceiling
// below a split's total returns the split to one shared pool.

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

// jobSlotKind classifies a transcode-record job.
func jobSlotKind(job *JobRecord) slotKind {
	if job != nil && job.Plan != nil && job.Plan.Podcast != nil {
		return slotKindPodcast
	}
	return slotKindVideo
}

// jobIsX265 reports whether a video job encodes with libx265 and so draws on
// the x265 cap. VideoToolbox and video-copy jobs do not, and neither does a job
// whose encode is finished: it only finalizes its output and never runs ffmpeg.
func jobIsX265(job *JobRecord) bool {
	return jobSlotKind(job) == slotKindVideo && job != nil && !job.EncodeComplete && job.Plan != nil && norm(job.Plan.VideoCodec) == transcode.VideoCodecLibX265
}

// jobSlot is the pools a job draws on: its kind, and whether it also counts
// against the x265 cap.
type jobSlot struct {
	kind slotKind
	x265 bool
}

func jobSlotOf(job *JobRecord) jobSlot {
	return jobSlot{kind: jobSlotKind(job), x265: jobIsX265(job)}
}

// slotCounts is the slots in use: video and podcast jobs, and how many of the
// video jobs are x265 encodes.
type slotCounts struct {
	Video, Podcast, X265 int
}

func (c *slotCounts) add(s jobSlot) {
	switch {
	case s.kind == slotKindPodcast:
		c.Podcast++
	case s.x265:
		c.Video++
		c.X265++
	default:
		c.Video++
	}
}

// slotSplitRecord is the stored ceiling override and split. Ceiling zero (or
// absent, in a file written before overrides existed) follows the config, and
// an absent X265Slots (a file written before the cap existed) is no cap.
type slotSplitRecord struct {
	VideoSlots   int       `json:"video_slots"`
	PodcastSlots int       `json:"podcast_slots"`
	Ceiling      int       `json:"ceiling,omitempty"`
	X265Slots    int       `json:"x265_slots,omitempty"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// slotLimits is the admission rule in force.
type slotLimits struct {
	Ceiling int
	Video   int // pool size for video work; the ceiling when !Split
	Podcast int // pool size for podcast work; the ceiling when !Split
	Split   bool
	X265    int // most x265 encodes at once inside the video pool; 0 is no cap
}

// hasRoom reports whether one more job of the given class may start given the
// jobs already running. The ceiling caps both kinds together in every mode: a
// split saved while jobs of the other kind run (or one that lowers a pool
// under running jobs) must not let the worker exceed it.
func (l slotLimits) hasRoom(s jobSlot, use slotCounts) bool {
	if use.Video+use.Podcast >= l.Ceiling {
		return false
	}
	if s.kind == slotKindPodcast {
		return !l.Split || use.Podcast < l.Podcast
	}
	if l.Split && use.Video >= l.Video {
		return false
	}
	return !s.x265 || l.X265 == 0 || use.X265 < l.X265
}

// videoPool is how many video jobs can run at once under these limits.
func (l slotLimits) videoPool() int {
	if !l.Split {
		return l.Ceiling
	}
	return l.Video
}

// x265Pool is how many x265 encodes can run together under these limits: the
// x265 cap when there is one, else every video slot, as before the cap existed.
func (l slotLimits) x265Pool() int {
	if l.X265 > 0 {
		return l.X265
	}
	return l.videoPool()
}

// slotSplitCache re-reads the split file only when its stat changes, so the
// hot admission and thread-sizing paths cost one stat call.
type slotSplitCache struct {
	mu      sync.Mutex
	modTime time.Time
	size    int64
	readAt  time.Time
	valid   bool
	rec     slotSplitRecord
}

// slotSplitRereadAfter bounds how long a cached split survives a rewrite that
// leaves the file's mtime and size unchanged (a 3/1 and a 1/3 split are the
// same length).
const slotSplitRereadAfter = 2 * time.Second

// configCeiling is max_parallel_jobs, the boot default.
func (w *Worker) configCeiling() int {
	if w == nil || w.cfg == nil {
		return 1
	}
	return max(1, w.cfg.MaxParallelJobs)
}

// slotCeilingLimit is the most parallel jobs an override may set:
// max_parallel_jobs_limit when configured, else the host CPU count, and never
// less than the configured default.
func (w *Worker) slotCeilingLimit() int {
	limit := hostCPUCount()
	if w != nil && w.cfg != nil && w.cfg.MaxParallelJobsLimit > 0 {
		limit = w.cfg.MaxParallelJobsLimit
	}
	return max(limit, w.configCeiling())
}

// ceilingFrom is the effective ceiling and its source given the stored
// record. An override outside 1..limit (the limit was lowered since) is
// ignored and the config applies.
func (w *Worker) ceilingFrom(rec slotSplitRecord, ok bool) (int, string) {
	if ok && rec.Ceiling >= 1 && rec.Ceiling <= w.slotCeilingLimit() {
		return rec.Ceiling, transcode.SlotCeilingFromOverride
	}
	return w.configCeiling(), transcode.SlotCeilingFromConfig
}

// slotCeiling is the effective ceiling: the stored override, else
// max_parallel_jobs.
func (w *Worker) slotCeiling() int {
	ceiling, _ := w.ceilingFrom(w.storedSlots())
	return ceiling
}

func (w *Worker) slotSplitPath() string {
	if w == nil || w.cfg == nil || w.cfg.StateDir == "" {
		return ""
	}
	return filepath.Join(w.cfg.StateDir, slotSplitFile)
}

// storedSlots returns the stored record, re-read only when the file changes.
// ok is false without a readable record.
func (w *Worker) storedSlots() (slotSplitRecord, bool) {
	path := w.slotSplitPath()
	if path == "" {
		return slotSplitRecord{}, false
	}
	c := &w.slotSplit
	c.mu.Lock()
	defer c.mu.Unlock()
	fi, err := os.Stat(path)
	if err != nil {
		c.valid, c.modTime, c.size, c.readAt = false, time.Time{}, 0, time.Time{}
		return slotSplitRecord{}, false
	}
	if !c.modTime.Equal(fi.ModTime()) || c.size != fi.Size() || time.Since(c.readAt) > slotSplitRereadAfter {
		c.valid, c.modTime, c.size, c.readAt = false, fi.ModTime(), fi.Size(), time.Now()
		if data, err := os.ReadFile(path); err == nil {
			var rec slotSplitRecord
			if json.Unmarshal(data, &rec) == nil {
				c.rec, c.valid = rec, true
			}
		}
	}
	return c.rec, c.valid
}

// slotLimits returns the limits in force: the effective ceiling, split the
// stored way when that split is valid for it, else one shared pool of the
// ceiling. A stored split that no longer fits (max_parallel_jobs was lowered)
// is ignored rather than scaled, so a worker never admits more than its
// ceiling.
func (w *Worker) slotLimits() slotLimits {
	rec, ok := w.storedSlots()
	ceiling, _ := w.ceilingFrom(rec, ok)
	limits := slotLimits{Ceiling: ceiling, Video: ceiling, Podcast: ceiling}
	if !ok {
		return limits
	}
	if _, err := transcode.ValidateSlotSplit(transcode.SlotSplit{VideoSlots: rec.VideoSlots, PodcastSlots: rec.PodcastSlots}, ceiling, 0); err == nil && (rec.VideoSlots != 0 || rec.PodcastSlots != 0) {
		limits = slotLimits{Ceiling: ceiling, Video: rec.VideoSlots, Podcast: rec.PodcastSlots, Split: true}
	}
	// A cap larger than the video pool that is in force (the ceiling or the
	// split was lowered since) is ignored rather than clamped, as a split that
	// no longer fits is.
	if rec.X265Slots > 0 && rec.X265Slots <= limits.videoPool() {
		limits.X265 = rec.X265Slots
	}
	return limits
}

// slotSplitInvalidError is an operator mistake in a split, as opposed to the
// worker failing to store a valid one.
type slotSplitInvalidError struct{ err error }

func (e *slotSplitInvalidError) Error() string { return e.err.Error() }
func (e *slotSplitInvalidError) Unwrap() error { return e.err }

// SetSlotSplit validates and stores a split, or clears it when both values are
// zero. A positive split.Ceiling also sets the ceiling (the config value
// clears the override); zero keeps the ceiling in force. A split that does not
// fit a ceiling set in the same call returns to one shared pool (see
// transcode.FitSlotSplit). It returns notes about that and warnings about the
// CPU budget for what was applied.
func (w *Worker) SetSlotSplit(split transcode.SlotSplit) ([]string, error) {
	path := w.slotSplitPath()
	if path == "" {
		return nil, errors.New("worker has no state_dir to store the slot split")
	}
	w.slotWrite.Lock()
	defer w.slotWrite.Unlock()
	ceiling, source := w.ceilingFrom(w.storedSlots())
	override := 0
	if source == transcode.SlotCeilingFromOverride {
		override = ceiling
	}
	var notes []string
	if split.Ceiling != 0 {
		if err := transcode.ValidateSlotCeiling(split.Ceiling, w.slotCeilingLimit()); err != nil {
			return nil, &slotSplitInvalidError{err}
		}
		ceiling, override = split.Ceiling, split.Ceiling
		if override == w.configCeiling() {
			override = 0
		}
		split, notes = transcode.FitSlotSplit(split, ceiling)
	}
	warnings, err := transcode.ValidateSlotSplit(split, ceiling, hostCPUCount())
	if err != nil {
		return nil, &slotSplitInvalidError{err}
	}
	warnings = append(notes, warnings...)
	if split.Shared() && override == 0 && split.X265Slots == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		w.noteSlotSplitChanged()
		return warnings, nil
	}
	data, err := json.MarshalIndent(slotSplitRecord{VideoSlots: split.VideoSlots, PodcastSlots: split.PodcastSlots, Ceiling: override, X265Slots: split.X265Slots, UpdatedAt: time.Now().UTC()}, "", "  ")
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
	slotCounts
	Queued []*JobRecord // durable queued jobs without a runner yet
	kinds  map[string]slotKind
}

// observeSlotUse scans the state directory. A job counts when its record
// says running or queued with a runner PID; with checkAlive a runner
// that is no longer alive does not.
func (w *Worker) observeSlotUse(checkAlive bool) (slotUse, error) {
	use := slotUse{kinds: map[string]slotKind{}}
	entries, err := os.ReadDir(w.cfg.StateDir)
	if err != nil {
		return use, err
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
					use.add(jobSlotOf(job))
				}
			} else if job.Status == "queued" {
				use.Queued = append(use.Queued, job)
			}
		}
	}
	return use, nil
}

// slotsStatus reports the limits and current use for /v1/ready.
func (w *Worker) slotsStatus() transcode.WorkerSlots {
	l := w.slotLimits()
	_, source := w.ceilingFrom(w.storedSlots())
	out := transcode.WorkerSlots{Ceiling: l.Ceiling, CeilingLimit: w.slotCeilingLimit(), CeilingSource: source, HostCPUs: hostCPUCount(), Split: l.Split, Video: transcode.SlotPool{Total: l.Video}, Podcast: transcode.SlotPool{Total: l.Podcast}, X265: &transcode.SlotPool{Total: l.X265}}
	if use, err := w.observeSlotUse(false); err == nil {
		out.Video.Used, out.Podcast.Used, out.X265.Used = use.Video, use.Podcast, use.X265
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
	x265  int
	ok    bool
}

// videoJobsSharingCPU is how many video jobs the host CPUs are shared between
// right now: the video jobs running, which include the caller
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
		c.at, c.video, c.x265, c.ok = time.Now(), use.Video, use.X265, err == nil
	}
	n, ok := c.video, c.ok
	c.mu.Unlock()
	if ok && n > 0 {
		return n
	}
	return max(1, w.slotLimits().videoPool())
}

// x265JobsSharingCPU is how many x265 encodes the host CPUs are shared between
// right now, for sizing an x265 thread pool under limits l. Without an x265
// cap that is every running video job (videoJobsSharingCPU), unchanged. With
// one, a VideoToolbox job beside an x265 encode does not take threads from it,
// so only the x265 encodes count. With none counted (or the count
// unavailable) it assumes the x265 pool is full.
func (w *Worker) x265JobsSharingCPU(l slotLimits) int {
	if l.X265 == 0 {
		return w.videoJobsSharingCPU()
	}
	if w == nil || w.cfg == nil {
		return 1
	}
	if w.x265InUseFn != nil {
		if n, ok := w.x265InUseFn(); ok && n > 0 {
			return n
		}
		return max(1, l.x265Pool())
	}
	c := &w.videoInUse
	c.mu.Lock()
	if time.Since(c.at) > videoInUseTTL || c.at.IsZero() {
		use, err := w.observeSlotUse(true)
		c.at, c.video, c.x265, c.ok = time.Now(), use.Video, use.X265, err == nil
	}
	n, ok := c.x265, c.ok
	c.mu.Unlock()
	if ok && n > 0 {
		return n
	}
	return max(1, l.x265Pool())
}

// slotChangeRecheck lets saving a split start the background self-check of its
// pools. A variable so tests, whose workers find a real ffmpeg on PATH, do not
// start real encodes in their state directories.
var slotChangeRecheck = true

// noteSlotSplitChanged drops caches that depend on the split and re-proves the
// x265 partitions the new split can produce.
func (w *Worker) noteSlotSplitChanged() {
	w.videoInUse.mu.Lock()
	w.videoInUse.at = time.Time{}
	w.videoInUse.mu.Unlock()
	w.slotSplit.mu.Lock()
	w.slotSplit.modTime = time.Time{}
	w.slotSplit.mu.Unlock()
	if slotChangeRecheck {
		w.recheckX265PartitionAfterSlotChange()
	}
}
