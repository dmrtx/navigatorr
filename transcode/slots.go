package transcode

import (
	"fmt"
)

// MinX265PoolThreads is the smallest x265 thread pool the worker hands one
// encode. Smaller pools change x265's lookahead scheduling and with it the
// bitstream, so a video job is never given fewer threads than this.
const MinX265PoolThreads = 4

// SlotPool is one kind of work's share of a worker's slots.
type SlotPool struct {
	Total int `json:"total"`
	Used  int `json:"used"`
}

// WorkerSlots is how a worker divides its ceiling (the jobs it runs at once)
// between video work (full conversions and final quality checks) and podcast
// work (transcription and ad detection). Without a split both kinds draw on
// one shared pool of Ceiling slots and Video.Total and Podcast.Total both
// report the ceiling.
//
// Ceiling is the effective value: max_parallel_jobs from the worker config, or
// the operator's override of it. CeilingLimit and CeilingSource are empty
// from a worker that cannot take an override.
type WorkerSlots struct {
	Ceiling       int      `json:"ceiling"`
	CeilingLimit  int      `json:"ceiling_limit,omitempty"`
	CeilingSource string   `json:"ceiling_source,omitempty"`
	HostCPUs      int      `json:"host_cpus,omitempty"`
	Split         bool     `json:"split"`
	Video         SlotPool `json:"video"`
	Podcast       SlotPool `json:"podcast"`
	// X265 is the cap on x265 encodes inside the video pool (Total 0: no
	// cap). Nil from a worker that predates the cap.
	X265 *SlotPool `json:"x265,omitempty"`
}

// Ceiling sources a worker reports.
const (
	SlotCeilingFromConfig   = "config"
	SlotCeilingFromOverride = "override"
)

// CeilingAdjustable reports whether the worker accepts a ceiling override.
func (s WorkerSlots) CeilingAdjustable() bool { return s.CeilingLimit > 0 }

// X265Capable reports whether the worker accepts an x265 cap.
func (s WorkerSlots) X265Capable() bool { return s.X265 != nil }

// SlotSplit is the per-kind slot assignment the server pushes to a worker.
// Zero values for both clear the split and return the worker to one shared
// pool. Ceiling, when positive, also sets how many jobs the worker runs at
// once; zero leaves it as it is. A worker that predates it refuses the field,
// so it is only sent to one that reports CeilingLimit.
//
// X265Slots, when positive, caps how many x265 (libx265) encodes run at once
// inside the video pool, so a VideoToolbox encode can run beside one; zero is
// no cap. A worker that predates it refuses the field, so it is only sent to
// one that reports X265.
type SlotSplit struct {
	VideoSlots   int `json:"video_slots"`
	PodcastSlots int `json:"podcast_slots"`
	Ceiling      int `json:"ceiling,omitempty"`
	X265Slots    int `json:"x265_slots,omitempty"`
}

// Shared reports whether the assignment clears the video/podcast split (the
// x265 cap is independent of it).
func (s SlotSplit) Shared() bool { return s.VideoSlots == 0 && s.PodcastSlots == 0 }

// VideoPool is the video slots of the assignment given the ceiling: the
// ceiling itself when the split is cleared.
func (s SlotSplit) VideoPool(ceiling int) int {
	if s.Shared() {
		return ceiling
	}
	return s.VideoSlots
}

// ValidateSlotCeiling checks a ceiling against the worker's hard maximum.
func ValidateSlotCeiling(ceiling, limit int) error {
	if ceiling < 1 || ceiling > limit {
		return fmt.Errorf("parallel jobs must be between 1 and %d on this worker", max(limit, 1))
	}
	return nil
}

// FitSlotSplit applies a new ceiling to a split: a split whose pools add up to
// more than the ceiling returns to one shared pool rather than being scaled,
// so the worker only ever runs pools the operator chose. An x265 cap larger
// than the video pool that is left is cleared the same way. It returns notes
// describing what it reset.
func FitSlotSplit(split SlotSplit, ceiling int) (SlotSplit, []string) {
	var notes []string
	if !split.Shared() && split.VideoSlots+split.PodcastSlots > ceiling {
		split = SlotSplit{Ceiling: split.Ceiling, X265Slots: split.X265Slots}
		notes = append(notes, SlotSplitResetNote(ceiling))
	}
	if split.X265Slots > split.VideoPool(ceiling) {
		split.X265Slots = 0
		notes = append(notes, "The x265 limit does not fit the video slots, so x265 encodes are no longer limited.")
	}
	return split, notes
}

// SlotSplitResetNote explains a split FitSlotSplit reset.
func SlotSplitResetNote(ceiling int) string {
	return fmt.Sprintf("The video and podcast split does not fit %d parallel jobs, so all slots are shared again.", ceiling)
}

// ValidateSlotSplit checks a split against the worker's ceiling and CPU
// budget. The error is for a split that cannot work; the warnings describe
// one that works but gives video encodes less CPU than they would like.
// hostCPUs <= 0 means the CPU count is unknown and skips the CPU checks.
func ValidateSlotSplit(split SlotSplit, ceiling, hostCPUs int) (warnings []string, err error) {
	if pool := split.VideoPool(ceiling); split.X265Slots < 0 || split.X265Slots > pool {
		return nil, fmt.Errorf("x265_slots must be between 0 and the %d video slots", pool)
	}
	if split.Shared() {
		return nil, nil
	}
	switch {
	case split.VideoSlots < 1:
		return nil, fmt.Errorf("video_slots must be at least 1")
	case split.PodcastSlots < 1:
		return nil, fmt.Errorf("podcast_slots must be at least 1")
	case ceiling < 2:
		return nil, fmt.Errorf("the worker runs %d parallel job, so its slots cannot be split between video and podcast", max(ceiling, 1))
	case split.VideoSlots+split.PodcastSlots > ceiling:
		return nil, fmt.Errorf("video_slots + podcast_slots is %d but the worker runs at most %d parallel jobs", split.VideoSlots+split.PodcastSlots, ceiling)
	}
	if hostCPUs > 0 {
		if split.VideoSlots > hostCPUs {
			return nil, fmt.Errorf("video_slots %d exceeds the worker's %d CPUs", split.VideoSlots, hostCPUs)
		}
		// With x265 capped at one the other video jobs are not x265 encodes,
		// which never partition, so there is no pool split to warn about.
		if split.VideoSlots > 1 && split.X265Slots != 1 && hostCPUs/split.VideoSlots < MinX265PoolThreads {
			warnings = append(warnings, fmt.Sprintf("%d video slots on %d CPUs leave fewer than %d threads per encode, so x265 cannot partition its thread pools: encodes that run together each default to every CPU and compete", split.VideoSlots, hostCPUs, MinX265PoolThreads))
		}
	}
	return warnings, nil
}
