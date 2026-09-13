package transcodeworker

import (
	"reflect"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/transcode"
)

const sampleVideoToolboxHelp = `Encoder hevc_videotoolbox [VideoToolbox H.265 Encoder]:
    General capabilities: dr1 delay hardware
    Threading capabilities: none
    Supported hardware devices: videotoolbox
    Supported pixel formats: videotoolbox_vld nv12 yuv420p bgra p010le
hevc_videotoolbox AVOptions:
  -profile           <int>        E..V....... Profile (from 0 to 3) (default 0)
     main            1            E..V....... Main Profile
     main10          2            E..V....... Main10 Profile
  -realtime          <boolean>    E..V....... Hint that encoding should happen in real-time (default false)
  -prio_speed        <boolean>    E..V....... prioritize encoding speed (default true)
  -spatial_aq        <boolean>    E..V....... spatial adaptive quantization (default false)
`

func boolPtr(v bool) *bool { return &v }

func TestParseVideoToolboxCapabilities(t *testing.T) {
	caps := ParseVideoToolboxCapabilities(sampleVideoToolboxHelp)
	if !caps.Available {
		t.Fatal("encoder should be available")
	}
	for _, want := range []string{"main", "main10"} {
		if !contains(caps.Profiles, want) {
			t.Fatalf("missing profile %s in %+v", want, caps)
		}
	}
	for _, want := range []string{"yuv420p", "p010le"} {
		if !contains(caps.PixelFormats, want) {
			t.Fatalf("missing pixel format %s in %+v", want, caps)
		}
	}
	for _, want := range []string{"profile", "prio_speed", "spatial_aq", "realtime"} {
		if !contains(caps.Options, want) {
			t.Fatalf("missing option %s in %+v", want, caps)
		}
	}
}

func TestBuildVideoEncoderArgsLegacyIsUnchanged(t *testing.T) {
	plan := &transcode.Plan{VideoCodec: "hevc_videotoolbox", Quality: 65}
	got, err := BuildVideoEncoderArgs(plan)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"-c:v", "hevc_videotoolbox", "-q:v", "65"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("legacy args changed\nwant=%v\n got=%v", want, got)
	}
}

func TestBuildVideoEncoderArgsBalancedMapping(t *testing.T) {
	plan := &transcode.Plan{
		VideoCodec:       "hevc_videotoolbox",
		Quality:          65,
		VideoProfile:     "main",
		PixelFormat:      "yuv420p",
		PrioritizeSpeed:  boolPtr(false),
		SpatialAQ:        boolPtr(true),
		Realtime:         boolPtr(false),
		ExpectedBitDepth: 8,
	}
	got, err := BuildVideoEncoderArgs(plan)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"-c:v", "hevc_videotoolbox", "-q:v", "65",
		"-profile:v", "main", "-pix_fmt", "yuv420p",
		"-prio_speed", "0", "-spatial_aq", "1", "-realtime", "0",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected args\nwant=%v\n got=%v", want, got)
	}
}

func TestBuildVideoEncoderArgsMain10Mapping(t *testing.T) {
	plan := &transcode.Plan{
		VideoCodec:       "hevc_videotoolbox",
		Quality:          65,
		VideoProfile:     "main10",
		PixelFormat:      "p010le",
		PrioritizeSpeed:  boolPtr(false),
		SpatialAQ:        boolPtr(false),
		Realtime:         boolPtr(false),
		ExpectedBitDepth: 10,
	}
	got, err := BuildVideoEncoderArgs(plan)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"-c:v", "hevc_videotoolbox", "-q:v", "65",
		"-profile:v", "main10", "-pix_fmt", "p010le",
		"-prio_speed", "0", "-spatial_aq", "0", "-realtime", "0",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected args\nwant=%v\n got=%v", want, got)
	}
}

func TestBuildVideoEncoderArgsRejectsInvalidMain10(t *testing.T) {
	plan := &transcode.Plan{VideoCodec: "hevc_videotoolbox", Quality: 65, VideoProfile: "main10", PixelFormat: "yuv420p", ExpectedBitDepth: 10}
	_, err := BuildVideoEncoderArgs(plan)
	if err == nil || !strings.Contains(err.Error(), "main10 requires pixel format p010le") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateVideoToolboxCapabilitiesFailsClosed(t *testing.T) {
	plan := &transcode.Plan{
		VideoCodec:       "hevc_videotoolbox",
		Quality:          65,
		VideoProfile:     "main10",
		PixelFormat:      "p010le",
		SpatialAQ:        boolPtr(true),
		ExpectedBitDepth: 10,
	}
	caps := ParseVideoToolboxCapabilities(sampleVideoToolboxHelp)
	caps.Options = []string{"profile", "prio_speed", "realtime"}
	if err := ValidateVideoToolboxCapabilities(plan, caps); err == nil || !strings.Contains(err.Error(), "encoder_capability_unsupported") || !strings.Contains(err.Error(), "spatial_aq") {
		t.Fatalf("missing capability did not fail closed: %v", err)
	}
}

func TestValidateVideoToolboxCapabilitiesRejectsMissingMain10(t *testing.T) {
	plan := &transcode.Plan{VideoCodec: "hevc_videotoolbox", Quality: 65, VideoProfile: "main10", PixelFormat: "p010le", ExpectedBitDepth: 10}
	caps := ParseVideoToolboxCapabilities(sampleVideoToolboxHelp)
	caps.Profiles = []string{"main"}
	if err := ValidateVideoToolboxCapabilities(plan, caps); err == nil || !strings.Contains(err.Error(), "main10") {
		t.Fatalf("missing main10 did not fail closed: %v", err)
	}
}

func TestBuildFFmpegArgsPlacesTypedVideoOptionsDeterministically(t *testing.T) {
	plan := &transcode.Plan{
		VideoCodec:          "hevc_videotoolbox",
		Quality:             65,
		VideoProfile:        "main",
		PixelFormat:         "yuv420p",
		PrioritizeSpeed:     boolPtr(false),
		SpatialAQ:           boolPtr(false),
		Realtime:            boolPtr(false),
		ExpectedBitDepth:    8,
		PreserveMetadata:    true,
		PreserveChapters:    true,
		PreserveAttachments: true,
	}
	execPlan := &ExecutionPlan{Plan: plan}
	got, err := BuildFFmpegArgs(execPlan, "/media/source.mkv", "/media/candidate.mkv", "/tmp/progress")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"-y", "-progress", "/tmp/progress", "-nostats", "-i", "/media/source.mkv",
		"-map", "0:v?", "-map", "0:a?", "-map", "0:s?", "-map", "0:t?",
		"-map_metadata", "0", "-map_chapters", "0",
		"-c:v", "hevc_videotoolbox", "-q:v", "65", "-profile:v", "main", "-pix_fmt", "yuv420p", "-prio_speed", "0", "-spatial_aq", "0", "-realtime", "0",
		"-c:a", "copy", "-c:t", "copy", "/media/candidate.mkv",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected full args\nwant=%v\n got=%v", want, got)
	}
}
