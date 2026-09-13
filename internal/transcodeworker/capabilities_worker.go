package transcodeworker

import "context"

// VideoToolboxCapabilities probes the exact FFmpeg binary configured for this worker.
func (w *Worker) VideoToolboxCapabilities(ctx context.Context) (VideoToolboxCapabilities, error) {
	return ProbeVideoToolboxCapabilities(ctx, w.ffmpegPath)
}
