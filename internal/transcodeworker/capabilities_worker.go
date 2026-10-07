package transcodeworker

import (
	"context"
	"path/filepath"

	"github.com/jakenesler/navigatorr/transcode"
)

// Capabilities probes full versioned WorkerCapabilities using the configured FFmpeg binary.
func (w *Worker) Capabilities(ctx context.Context) (transcode.WorkerCapabilities, error) {
	caps, err := ProbeWorkerCapabilitiesWithScratch(ctx, w.ffmpegPath, filepath.Join(w.cfg.StateDir, "quality-probe-scratch"))
	if err != nil {
		return caps, err
	}
	caps.Podcast = w.podcastCapabilities(ctx)
	caps.CapabilityFingerprint, err = transcode.ComputeCapabilityFingerprint(caps)
	return caps, err
}

// VideoToolboxCapabilities probes the exact FFmpeg binary configured for this worker.
func (w *Worker) VideoToolboxCapabilities(ctx context.Context) (VideoToolboxCapabilities, error) {
	return ProbeVideoToolboxCapabilities(ctx, w.ffmpegPath)
}
