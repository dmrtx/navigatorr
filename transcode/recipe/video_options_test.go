package recipe

import (
	"fmt"
	"strings"
	"testing"
)

func testVideoRecipe(video string) []byte {
	return []byte(fmt.Sprintf(`schema_version: 1
bundle_version: "test-1"
containers:
  mkv:
    subtitle_copy: [ass, subrip]
    subtitle_conversions: {}
profiles:
  test:
    container: mkv
    video:
%s
    audio: {mode: copy}
    subtitles: {mode: preserve, convert_incompatible: true}
    preserve: {metadata: true, chapters: true, attachments: true}
    resilience: {max_attempts: 1, transient_retries: 0, retry_backoff_seconds: [], max_fallbacks: 0, fallbacks: []}
`, video))
}

func TestVideoProfileParsesTypedVideoToolboxControls(t *testing.T) {
	s, err := Parse(testVideoRecipe(`      codec: hevc_videotoolbox
      quality: 65
      profile: main10
      pixel_format: p010le
      prioritize_speed: false
      spatial_aq: true
      realtime: false`))
	if err != nil {
		t.Fatal(err)
	}
	p, err := Resolve(s, "test", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.VideoProfile != "main10" || p.PixelFormat != "p010le" || p.ExpectedBitDepth != 10 {
		t.Fatalf("unexpected resolved video controls: %+v", p)
	}
	if p.PrioritizeSpeed == nil || *p.PrioritizeSpeed {
		t.Fatalf("prioritize_speed=false was not preserved: %+v", p.PrioritizeSpeed)
	}
	if p.SpatialAQ == nil || !*p.SpatialAQ {
		t.Fatalf("spatial_aq=true was not preserved: %+v", p.SpatialAQ)
	}
	if p.Realtime == nil || *p.Realtime {
		t.Fatalf("realtime=false was not preserved: %+v", p.Realtime)
	}
}

func TestVideoProfileBackwardCompatibilityLeavesNewControlsUnset(t *testing.T) {
	s, err := Parse(testVideoRecipe(`      codec: hevc_videotoolbox
      quality: 65`))
	if err != nil {
		t.Fatal(err)
	}
	p, err := Resolve(s, "test", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.VideoProfile != "" || p.PixelFormat != "" || p.PrioritizeSpeed != nil || p.SpatialAQ != nil || p.Realtime != nil || p.ExpectedBitDepth != 0 {
		t.Fatalf("legacy profile acquired new behavior: %+v", p)
	}
}

func TestVideoProfileRejectsInvalidTypedControls(t *testing.T) {
	cases := []struct {
		name  string
		video string
		want  string
	}{
		{
			name: "main10 requires p010le",
			video: `      codec: hevc_videotoolbox
      quality: 65
      profile: main10
      pixel_format: yuv420p`,
			want: "main10 requires pixel_format p010le",
		},
		{
			name: "p010le requires main10",
			video: `      codec: hevc_videotoolbox
      quality: 65
      profile: main
      pixel_format: p010le`,
			want: "p010le requires HEVC profile main10",
		},
		{
			name: "unknown profile",
			video: `      codec: hevc_videotoolbox
      quality: 65
      profile: main12`,
			want: "unsupported HEVC profile",
		},
		{
			name: "unknown pixel format",
			video: `      codec: hevc_videotoolbox
      quality: 65
      pixel_format: yuv444p`,
			want: "unsupported pixel_format",
		},
		{
			name: "wrong boolean type",
			video: `      codec: hevc_videotoolbox
      quality: 65
      realtime: nope`,
			want: "cannot unmarshal",
		},
		{
			name: "arbitrary ffmpeg args rejected",
			video: `      codec: hevc_videotoolbox
      quality: 65
      ffmpeg_args: ["-foo", "bar"]`,
			want: "field ffmpeg_args",
		},
		{
			name: "unverified bitrate rejected",
			video: `      codec: hevc_videotoolbox
      quality: 65
      bitrate: 2500k`,
			want: "field bitrate",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(testVideoRecipe(tc.video))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestPlanDigestChangesWhenVideoKnobChanges(t *testing.T) {
	base, err := Parse(testVideoRecipe(`      codec: hevc_videotoolbox
      quality: 65
      profile: main
      pixel_format: yuv420p
      spatial_aq: false`))
	if err != nil {
		t.Fatal(err)
	}
	p1, err := Resolve(base, "test", nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	aq := true
	override := base.Bundle.Profiles["test"]
	override.Video.SpatialAQ = &aq
	p2, err := Resolve(base, "test", map[string]Profile{"test": override}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p1.PlanDigest == p2.PlanDigest {
		t.Fatalf("plan digest did not change when spatial_aq changed: %s", p1.PlanDigest)
	}
}
