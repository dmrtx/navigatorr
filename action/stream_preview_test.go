package action

import (
	"testing"

	"github.com/jakenesler/navigatorr/mediainspect"
)

func TestPreviewStreamsSizesTracksAndPlansEachGoal(t *testing.T) {
	rep := &mediainspect.DetailedReport{
		DurationSec: 1000, SizeBytes: 1_000_000_000,
		Video: []mediainspect.DetailedStream{{Index: 0, Kind: "video", Codec: "h264"}},
		Audio: []mediainspect.DetailedStream{
			// Lossless with only its statistics: converted under every goal.
			{Index: 1, Codec: "truehd", Channels: 6, ChannelLayout: "5.1(side)", Tags: map[string]string{"NUMBER_OF_BYTES": "400000000"}},
			// AC-3 448 kbps is within 25% of AAC-LC 5.1's 384: copied at level 1.
			{Index: 2, Codec: "ac3", Channels: 6, ChannelLayout: "5.1(side)", BitRate: 448_000},
			{Index: 3, Codec: "aac", Channels: 2, ChannelLayout: "stereo", BitRate: 128_000},
		},
		Subtitles: []mediainspect.DetailedStream{{Index: 4, Codec: "subrip", Tags: map[string]string{"NUMBER_OF_BYTES": "50000"}}},
	}
	p := PreviewStreams(rep)
	if p.SourceBytes != 1_000_000_000 || p.DurationSec != 1000 {
		t.Fatalf("source: %+v", p)
	}
	lossless, ac3, aac := p.Audio[1], p.Audio[2], p.Audio[3]
	if lossless.Bytes != 400_000_000 || lossless.Kbps != 3200 || ac3.Bytes != 56_000_000 || ac3.Kbps != 448 || aac.Bytes != 16_000_000 {
		t.Fatalf("track sizes: %+v %+v %+v", lossless, ac3, aac)
	}
	for _, goal := range []string{"size", "compact"} {
		if o := lossless.Outputs[goal]; o.Output != "aac" || o.Kbps != 384 || o.Bytes != 48_000_000 {
			t.Errorf("%s lossless: %+v", goal, o)
		}
		if o := ac3.Outputs[goal]; o.Output != "copy" || o.Bytes != 56_000_000 {
			t.Errorf("%s ac3 is copied at its own size: %+v", goal, o)
		}
		if o := aac.Outputs[goal]; o.Output != "copy" || o.Bytes != 16_000_000 {
			t.Errorf("%s aac: %+v", goal, o)
		}
	}
	if o := aac.Outputs["size_he"]; o.Output != "he_aac" || o.Kbps != 64 || o.Bytes != 8_000_000 {
		t.Errorf("level 2 makes stereo AAC 128 HE-AAC 64: %+v", o)
	}
	if p.Subtitles[4] != 50_000 {
		t.Fatalf("subtitle: %d", p.Subtitles[4])
	}
	if want := int64(1_000_000_000 - 400_000_000 - 56_000_000 - 16_000_000 - 50_000); p.VideoBytes != want || !p.VideoDerived {
		t.Fatalf("video is the rest of the file: %d derived=%v, want %d", p.VideoBytes, p.VideoDerived, want)
	}

	// The video's own statistics win over the rest of the file.
	rep.Video[0].Tags = map[string]string{"NUMBER_OF_BYTES": "500000000"}
	if p := PreviewStreams(rep); p.VideoBytes != 500_000_000 || p.VideoDerived {
		t.Fatalf("video statistics: %+v", p)
	}

	// An audio track of unknown size leaves the video unknown, not inflated.
	rep.Video[0].Tags = nil
	rep.Audio = append(rep.Audio, mediainspect.DetailedStream{Index: 5, Codec: "eac3", Channels: 6, ChannelLayout: "5.1(side)"})
	p = PreviewStreams(rep)
	if p.VideoBytes != 0 || p.Audio[5].Bytes != 0 || p.Audio[5].Outputs["size"].Output != "copy" {
		t.Fatalf("unknown track: %+v", p)
	}

	// No duration or size: nothing is sized.
	if p := PreviewStreams(&mediainspect.DetailedReport{Audio: rep.Audio}); len(p.Audio) != 0 || p.VideoBytes != 0 {
		t.Fatalf("unsized source: %+v", p)
	}
}
