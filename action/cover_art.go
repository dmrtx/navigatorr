package action

import (
	"context"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
)

// errCoverArtWorker is returned wherever a source with cover art would reach
// a worker that does not re-attach it.
const errCoverArtWorker = "worker does not keep cover art out of the video encode; upgrade the worker first"

// workerSupportsCoverArt reports whether the worker advertises the cover_art
// capability. Any lookup failure counts as unsupported (fail closed).
func workerSupportsCoverArt(ctx context.Context, worker transcode.Executor) bool {
	caps, err := worker.Capabilities(ctx)
	return err == nil && caps.CoverArt
}

// coverArtGateReport returns the report the source-class gates (the modes and the
// auto selector) should judge. Treating cover art as cover
// needs a worker with the cover_art capability; against an older worker the
// cover still counts as a second video stream, so those gates keep their
// previous reject/review behaviour. supported is consulted only when the
// source has cover art.
func coverArtGateReport(rep *mediainspect.DetailedReport, supported func() bool) *mediainspect.DetailedReport {
	if rep == nil || len(rep.CoverArt()) == 0 || supported() {
		return rep
	}
	legacy := *rep
	legacy.Video = make([]mediainspect.DetailedStream, len(rep.Video))
	for i, s := range rep.Video {
		if mediainspect.IsAttachedPicture(s) {
			disposition := make(map[string]int, len(s.Disposition))
			for k, v := range s.Disposition {
				if k != "attached_pic" {
					disposition[k] = v
				}
			}
			s.Disposition = disposition
		}
		legacy.Video[i] = s
	}
	return &legacy
}

// cachedCoverArtSupport asks the workers at most once, for loops over many
// sources before any of them is assigned a worker: every configured worker
// must support cover art, since any of them may be assigned the file.
func (e *Engine) cachedCoverArtSupport(ctx context.Context) func() bool {
	asked, supported := false, false
	return func() bool {
		if !asked {
			asked, supported = true, e.workersConfigured()
			for _, w := range e.deps.Workers.All() {
				supported = supported && workerSupportsCoverArt(ctx, w.Executor)
			}
		}
		return supported
	}
}
