package transcodeworker

import (
	"reflect"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/transcode"
)

// The statistics a source stream carries are found under every name ffprobe
// gives them: exact, or with the tag language mkvmerge often writes.
func TestStatisticsTagKeysIncludeLanguageVariants(t *testing.T) {
	tags := map[string]string{
		"language": "spa", "title": "Main", "BPS-eng": "640000", "NUMBER_OF_BYTES-eng": "1", "NUMBER_OF_FRAMES-eng": "1",
		"DURATION-eng": "00:00:01", "_STATISTICS_WRITING_APP-eng": "mkvmerge", "_STATISTICS_TAGS-eng": "BPS DURATION", "bps": "1", "BPSX": "1", "ENCODER": "x",
		// "-metadata:s:a:0 BPS-x=y=" would set BPS-x to "y=", not clear it.
		"BPS-x=y": "1",
	}
	want := []string{"BPS-eng", "DURATION-eng", "NUMBER_OF_BYTES-eng", "NUMBER_OF_FRAMES-eng", "_STATISTICS_TAGS-eng", "_STATISTICS_WRITING_APP-eng", "bps"}
	if got := statisticsTagKeys(tags); !reflect.DeepEqual(got, want) {
		t.Fatalf("keys %v, want %v", got, want)
	}
}

// Every re-encoded stream (video, converted audio, converted subtitle) loses
// the source's statistics under the names it really has them; a copied
// stream keeps them, for they still describe it.
func TestReencodedStreamsClearTheSourceStatisticsTheyCarry(t *testing.T) {
	stats := []string{"BPS-eng", "NUMBER_OF_BYTES-eng", "DURATION"}
	streams := []StreamAction{
		{Kind: "video", TypeIndex: 0, TargetCodec: "libx265", StatisticsTags: stats},
		{Kind: "audio", TypeIndex: 0, TargetCodec: "aac", BitrateKbps: 160, HEAAC: true, AudioToolbox: true, StatisticsTags: stats},
		{Kind: "audio", TypeIndex: 1, TargetCodec: "copy", StatisticsTags: stats},
		{Kind: "subtitle", TypeIndex: 0, TargetCodec: "subrip", StatisticsTags: stats},
		{Kind: "subtitle", TypeIndex: 1, TargetCodec: "copy", StatisticsTags: stats},
	}
	plan := &transcode.Plan{VideoCodec: "libx265", Quality: 24, Preset: "slow", VideoProfile: "main", PixelFormat: "yuv420p", ExpectedBitDepth: 8, AudioMode: transcode.AudioModeSizeHEAAC, PreserveMetadata: true}
	args, err := BuildFFmpegArgs(&ExecutionPlan{Plan: plan, Streams: streams}, "source.mkv", "candidate.mkv", "progress")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, spec := range []string{"v:0", "a:0", "s:0"} {
		for _, tag := range []string{"BPS", "BPS-eng", "NUMBER_OF_BYTES-eng", "NUMBER_OF_FRAMES", "DURATION", "_STATISTICS_TAGS"} {
			if !strings.Contains(joined, "-metadata:s:"+spec+" "+tag+"= ") {
				t.Errorf("%s keeps %s: %s", spec, tag, joined)
			}
		}
	}
	for _, spec := range []string{"a:1", "s:1"} {
		if strings.Contains(joined, "-metadata:s:"+spec+" ") {
			t.Errorf("copied %s loses its metadata: %s", spec, joined)
		}
	}
	if strings.Count(joined, "-metadata:s:v:0 DURATION= ") != 1 {
		t.Errorf("an exact name is cleared twice: %s", joined)
	}

	// A copied video stream (the size goal's audio-only plan) keeps its own
	// statistics; the converted audio still loses the source's.
	copyPlan := sizeAudioOnlyTestPlan(t, 8)
	streams[0].TargetCodec = "copy"
	args, err = BuildFFmpegArgs(&ExecutionPlan{Plan: copyPlan, Streams: streams[:3]}, "source.mkv", "candidate.mkv", "progress")
	if err != nil {
		t.Fatal(err)
	}
	joined = strings.Join(args, " ")
	if strings.Contains(joined, "-metadata:s:v:0") || !strings.Contains(joined, "-metadata:s:a:0 BPS-eng= ") {
		t.Errorf("audio-only plan: %s", joined)
	}
}
