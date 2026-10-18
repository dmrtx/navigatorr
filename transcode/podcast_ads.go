package transcode

import (
	"context"

	"github.com/jakenesler/navigatorr/podcast"
)

// Optional capability keeps existing SSH/video executors compatible.
type PodcastAdExecutor interface {
	AdCatalog(context.Context, string) (podcast.AdCatalog, error)
	RevokeAd(context.Context, string, string) error
}
