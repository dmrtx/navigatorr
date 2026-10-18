package selector

import (
	"slices"
	"testing"

	"github.com/jakenesler/navigatorr/mediainspect"
)

func TestSelectorIgnoresCoverArtWhenCountingVideo(t *testing.T) {
	cover := mediainspect.DetailedStream{Index: 0, Kind: "video", Codec: "mjpeg", Width: 600, Height: 900, Disposition: map[string]int{"attached_pic": 1}}
	main := mediainspect.DetailedStream{Index: 1, Kind: "video", Codec: "h264", Width: 1920, Height: 1080, BitDepth: 8}
	rep := mediainspect.DetailedReport{Probed: true, Container: "mkv", DurationSec: 1440, SizeBytes: 1600 * 1024 * 1024, Video: []mediainspect.DetailedStream{cover, main}, Audio: []mediainspect.DetailedStream{{Index: 2, Kind: "audio", Codec: "aac", Channels: 2}}}
	res := Select(Input{Report: rep, MediaType: "episode", MinSavingsPercent: 15})
	if res.Decision != DecisionTranscode || slices.Contains(res.Reasons, ReasonMultipleVideoStreams) {
		t.Fatalf("cover art blocked selection: %+v", res)
	}
	rep.Video = append(rep.Video, main)
	if res := Select(Input{Report: rep, MediaType: "episode", MinSavingsPercent: 15}); !slices.Contains(res.Reasons, ReasonMultipleVideoStreams) {
		t.Fatalf("two programme video streams accepted: %+v", res)
	}
}
