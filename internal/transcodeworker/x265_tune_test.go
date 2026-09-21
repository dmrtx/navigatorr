package transcodeworker

import (
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/transcode"
)

func TestBuildLibX265ArgsIncludesBoundedTune(t *testing.T) {
	plan := &transcode.Plan{
		VideoCodec:       transcode.VideoCodecLibX265,
		Quality:          24,
		Preset:           "slow",
		Tune:             "animation",
		VideoProfile:     "main",
		PixelFormat:      "yuv420p",
		ExpectedBitDepth: 8,
	}
	args, err := BuildVideoEncoderArgs(plan)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-preset slow") || !strings.Contains(joined, "-tune animation") || !strings.Contains(joined, "-crf 24") {
		t.Fatalf("x265 args missing expected typed controls: %v", args)
	}
}

func TestBuildVideoEncoderArgsRejectsTuneForVideoToolbox(t *testing.T) {
	_, err := BuildVideoEncoderArgs(&transcode.Plan{
		VideoCodec:       transcode.VideoCodecHEVCVideoToolbox,
		Quality:          65,
		Tune:             "animation",
		VideoProfile:     "main",
		PixelFormat:      "yuv420p",
		ExpectedBitDepth: 8,
	})
	if err == nil || !strings.Contains(err.Error(), "tune is only supported for libx265") {
		t.Fatalf("expected fail-closed tune rejection, got %v", err)
	}
}

func TestBuildLibX265ArgsRejectsUnknownTune(t *testing.T) {
	_, err := BuildVideoEncoderArgs(&transcode.Plan{
		VideoCodec:       transcode.VideoCodecLibX265,
		Quality:          24,
		Preset:           "slow",
		Tune:             "not-a-real-tune",
		VideoProfile:     "main",
		PixelFormat:      "yuv420p",
		ExpectedBitDepth: 8,
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported libx265 tune") {
		t.Fatalf("expected invalid tune rejection, got %v", err)
	}
}
