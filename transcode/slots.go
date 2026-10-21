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

// WorkerSlots is how a worker divides its max_parallel_jobs ceiling between
// video work (full conversions and final quality checks) and podcast work (transcription
// and ad detection). Without a split both kinds draw on one shared pool of
// Ceiling slots and Video.Total and Podcast.Total both report the ceiling.
type WorkerSlots struct {
	Ceiling  int      `json:"ceiling"`
	HostCPUs int      `json:"host_cpus,omitempty"`
	Split    bool     `json:"split"`
	Video    SlotPool `json:"video"`
	Podcast  SlotPool `json:"podcast"`
}

// SlotSplit is the per-kind slot assignment the server pushes to a worker.
// Zero values for both clear the split and return the worker to one shared
// pool.
type SlotSplit struct {
	VideoSlots   int `json:"video_slots"`
	PodcastSlots int `json:"podcast_slots"`
}

// Shared reports whether the assignment clears the split.
func (s SlotSplit) Shared() bool { return s.VideoSlots == 0 && s.PodcastSlots == 0 }

// ValidateSlotSplit checks a split against the worker's ceiling and CPU
// budget. The error is for a split that cannot work; the warnings describe
// one that works but gives video encodes less CPU than they would like.
// hostCPUs <= 0 means the CPU count is unknown and skips the CPU checks.
func ValidateSlotSplit(split SlotSplit, ceiling, hostCPUs int) (warnings []string, err error) {
	if split.Shared() {
		return nil, nil
	}
	switch {
	case split.VideoSlots < 1:
		return nil, fmt.Errorf("video_slots must be at least 1")
	case split.PodcastSlots < 1:
		return nil, fmt.Errorf("podcast_slots must be at least 1")
	case ceiling < 2:
		return nil, fmt.Errorf("the worker's max_parallel_jobs is %d, so its slots cannot be split between video and podcast", max(ceiling, 1))
	case split.VideoSlots+split.PodcastSlots > ceiling:
		return nil, fmt.Errorf("video_slots + podcast_slots is %d but the worker's max_parallel_jobs ceiling is %d", split.VideoSlots+split.PodcastSlots, ceiling)
	}
	if hostCPUs > 0 {
		if split.VideoSlots > hostCPUs {
			return nil, fmt.Errorf("video_slots %d exceeds the worker's %d CPUs", split.VideoSlots, hostCPUs)
		}
		if split.VideoSlots > 1 && hostCPUs/split.VideoSlots < MinX265PoolThreads {
			warnings = append(warnings, fmt.Sprintf("%d video slots on %d CPUs leave fewer than %d threads per encode, so x265 cannot partition its thread pools: encodes that run together each default to every CPU and compete", split.VideoSlots, hostCPUs, MinX265PoolThreads))
		}
	}
	return warnings, nil
}
