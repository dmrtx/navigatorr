package transcodeworker

import (
	"context"
	"path/filepath"

	"github.com/jakenesler/navigatorr/transcode"
)

// Capabilities probes full versioned WorkerCapabilities using the configured FFmpeg binary.
func (w *Worker) Capabilities(ctx context.Context) (transcode.WorkerCapabilities, error) {
	return ProbeWorkerCapabilitiesWithScratch(ctx, w.ffmpegPath, filepath.Join(w.cfg.StateDir, "quality-probe-scratch"))
}

// VideoToolboxCapabilities probes the exact FFmpeg binary configured for this worker.
func (w *Worker) VideoToolboxCapabilities(ctx context.Context) (VideoToolboxCapabilities, error) {
	return ProbeVideoToolboxCapabilities(ctx, w.ffmpegPath)
}
