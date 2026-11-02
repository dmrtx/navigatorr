package action

import (
	"testing"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
)

// Every source track says what became of it, also when the goal kept every
// track, and the video's after is the new file less what its audio takes.
func TestConversionStreamsSayWhatBecameOfEachTrack(t *testing.T) {
	rep := &mediainspect.DetailedReport{
		DurationSec: 1000, SizeBytes: 2_000_000_000,
		Video: []mediainspect.DetailedStream{{Index: 0, Kind: "video", Codec: "h264", Width: 1920, Height: 1080}},
		Audio: []mediainspect.DetailedStream{
			{Index: 1, Kind: "audio", Codec: "dts", Channels: 6, ChannelLayout: "5.1(side)", Language: "eng", BitRate: 1_536_000},
			{Index: 2, Kind: "audio", Codec: "ac3", Channels: 2, ChannelLayout: "stereo", Language: "spa", BitRate: 192_000},
			{Index: 3, Kind: "audio", Codec: "ac3", Channels: 2, ChannelLayout: "stereo", Language: "fra", BitRate: 192_000},
		},
		Subtitles: []mediainspect.DetailedStream{{Index: 4, Kind: "subtitle", Codec: "subrip", Language: "eng"}, {Index: 5, Kind: "subtitle", Codec: "subrip", Language: "fra"}},
	}
	plan := &transcode.Plan{VideoCodec: transcode.VideoCodecLibX265, AudioMode: transcode.AudioModeSizeAACBed, Tracks: &transcode.TrackSelection{KeepAudio: []int{1, 2}, KeepSubtitles: []int{4}}}
	got := conversionStreams(rep, plan, 600_000_000)
	if len(got.Audio) != 3 {
		t.Fatalf("audio %+v", got.Audio)
	}
	dts, spa, fra := got.Audio[0], got.Audio[1], got.Audio[2]
	if dts.Fate != "converted" || dts.AfterCodec != "aac" || dts.AfterKbps <= 0 || dts.AfterBytes >= dts.BeforeBytes || dts.BeforeBytes != 192_000_000 {
		t.Fatalf("dts %+v", dts)
	}
	if fra.Fate != "dropped" || fra.AfterBytes != 0 {
		t.Fatalf("fra %+v", fra)
	}
	if spa.Fate == "dropped" {
		t.Fatalf("spa %+v", spa)
	}
	if got.Subtitles.Total != 2 || got.Subtitles.Kept != 1 {
		t.Fatalf("subtitles %+v", got.Subtitles)
	}
	if want := 600_000_000 - dts.AfterBytes - spa.AfterBytes; got.Video.AfterBytes != want || got.Video.Codec != "h264" || got.Video.AfterCodec != transcode.VideoCodecLibX265 {
		t.Fatalf("video %+v want after %d", got.Video, want)
	}

	// No selection: every track is kept.
	all := conversionStreams(rep, &transcode.Plan{VideoCodec: transcode.VideoCodecLibX265, AudioMode: "copy"}, 600_000_000)
	for _, a := range all.Audio {
		if a.Fate != "copied" || a.AfterBytes != a.BeforeBytes {
			t.Fatalf("kept-all %+v", a)
		}
	}
	if ConversionStreamsOf(map[string]any{}, 1) != nil {
		t.Fatal("no probe, no streams")
	}
}
