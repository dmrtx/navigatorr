package transcodeworker

import (
	"context"

	"github.com/jakenesler/navigatorr/transcode"
)

// Capabilities probes full versioned WorkerCapabilities using the configured FFmpeg binary.
func (w *Worker) Capabilities(ctx context.Context) (transcode.WorkerCapabilities, error) {
	return ProbeWorkerCapabilities(ctx, w.ffmpegPath)
}

// VideoToolboxCapabilities probes the exact FFmpeg binary configured for this worker.
func (w *Worker) VideoToolboxCapabilities(ctx context.Context) (VideoToolboxCapabilities, error) {
	return ProbeVideoToolboxCapabilities(ctx, w.ffmpegPath)
}
