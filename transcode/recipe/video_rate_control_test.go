package recipe

import (
	"fmt"
	"strings"
	"testing"
)

func testRateControlRecipe(video string) []byte {
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

func TestVideoToolboxBitrateModeResolves(t *testing.T) {
	s, err := Parse(testRateControlRecipe(`      codec: hevc_videotoolbox
      average_bitrate_kbps: 3500
      profile: main
      pixel_format: yuv420p
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
	if p.Quality != 0 || p.AverageBitrateKbps != 3500 {
		t.Fatalf("expected bitrate mode (quality 0, 3500k), got quality=%d bitrate=%d", p.Quality, p.AverageBitrateKbps)
	}
	if p.VideoCodec != "hevc_videotoolbox" || p.VideoProfile != "main" || p.PixelFormat != "yuv420p" {
		t.Fatalf("unexpected resolved shape: %+v", p)
	}
}

func TestVideoToolboxOfflineKnobsResolve(t *testing.T) {
	s, err := Parse(testRateControlRecipe(`      codec: hevc_videotoolbox
      quality: 65
      qmin: 0
      qmax: 51
      gop_size: 300
      b_frames: 0
      closed_gop: true
      power_efficient: false
      max_ref_frames: 4`))
	if err != nil {
		t.Fatal(err)
	}
	p, err := Resolve(s, "test", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.QMin == nil || *p.QMin != 0 || p.QMax == nil || *p.QMax != 51 {
		t.Fatalf("qmin/qmax not preserved: %+v", p)
	}
	if p.GOPSize == nil || *p.GOPSize != 300 || p.BFrames == nil || *p.BFrames != 0 {
		t.Fatalf("gop/bframes not preserved (explicit zero must survive): %+v", p)
	}
	if p.ClosedGOP == nil || !*p.ClosedGOP || p.PowerEfficient == nil || *p.PowerEfficient || p.MaxRefFrames == nil || *p.MaxRefFrames != 4 {
		t.Fatalf("closed_gop/power_efficient/max_ref_frames not preserved: %+v", p)
	}
}

func TestVideoRateControlFailClosed(t *testing.T) {
	cases := []struct {
		name  string
		video string
		want  string
	}{
		{
			name: "quality and bitrate together",
			video: `      codec: hevc_videotoolbox
      quality: 65
      average_bitrate_kbps: 3500`,
			want: "mutually exclusive",
		},
		{
			name: "neither quality nor bitrate",
			video: `      codec: hevc_videotoolbox
      quality: 0`,
			want: "must specify either quality",
		},
		{
			name: "constant bitrate without average",
			video: `      codec: hevc_videotoolbox
      quality: 65
      constant_bitrate: true`,
			want: "constant_bitrate requires average_bitrate_kbps",
		},
		{
			name: "maxrate without average",
			video: `      codec: hevc_videotoolbox
      quality: 65
      max_bitrate_kbps: 5000`,
			want: "max_bitrate_kbps requires average_bitrate_kbps",
		},
		{
			name: "maxrate below average",
			video: `      codec: hevc_videotoolbox
      average_bitrate_kbps: 3500
      max_bitrate_kbps: 3000`,
			want: "must be >= average_bitrate_kbps",
		},
		{
			name: "qmin above qmax",
			video: `      codec: hevc_videotoolbox
      quality: 65
      qmin: 40
      qmax: 30`,
			want: "qmin (40) must be <= qmax (30)",
		},
		{
			name: "preset for videotoolbox",
			video: `      codec: hevc_videotoolbox
      quality: 65
      preset: slow`,
			want: "preset is only supported for libx265",
		},
		{
			name: "average bitrate for libx265",
			video: `      codec: libx265
      quality: 24
      preset: slow
      average_bitrate_kbps: 3500`,
			want: "only supported for hevc_videotoolbox, not libx265",
		},
		{
			name: "max bitrate for libx265",
			video: `      codec: libx265
      quality: 24
      average_bitrate_kbps: 3000
      max_bitrate_kbps: 5000`,
			want: "only supported for hevc_videotoolbox, not libx265",
		},
		{
			name: "spatial aq for libx265",
			video: `      codec: libx265
      quality: 24
      spatial_aq: true`,
			want: "not supported for libx265",
		},
		{
			name: "gop size for libx265",
			video: `      codec: libx265
      quality: 24
      gop_size: 250`,
			want: "only supported for hevc_videotoolbox, not libx265",
		},
		{
			name: "bufsize still rejected",
			video: `      codec: hevc_videotoolbox
      quality: 65
      bufsize: 5000k`,
			want: "field bufsize",
		},
		{
			name: "bare bitrate still rejected",
			video: `      codec: hevc_videotoolbox
      quality: 65
      bitrate: 2500k`,
			want: "field bitrate",
		},
		{
			name: "software fallback knob rejected",
			video: `      codec: hevc_videotoolbox
      quality: 65
      require_sw: true`,
			want: "field require_sw",
		},
		{
			name: "low delay knob rejected",
			video: `      codec: hevc_videotoolbox
      quality: 65
      low_delay: true`,
			want: "field low_delay",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(testRateControlRecipe(tc.video))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestSearchBitrateValuesValidation(t *testing.T) {
	videoQuality := `      codec: hevc_videotoolbox
      quality: 65`
	videoBitrate := `      codec: hevc_videotoolbox
      average_bitrate_kbps: 3500`
	videoX265 := `      codec: libx265
      quality: 24
      preset: slow`

	withOpt := func(video, search string) []byte {
		return []byte(fmt.Sprintf(`schema_version: 2
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
    optimization:
      enabled: true
      sampling: {strategy: distributed, sample_count: 1, sample_seconds: 20, positions: [0.5]}
      quality: {preferred_metric: vmaf, vmaf: {target: 92, minimum: 88}}
      search:
%s
`, video, search))
	}

	// Valid VT bitrate sweep normalizes without backfilling quality values.
	s, err := Parse(withOpt(videoBitrate, "        max_candidates: 3\n        bitrate_values: [3200, 3500, 3800]"))
	if err != nil {
		t.Fatalf("valid bitrate sweep rejected: %v", err)
	}
	if len(s.Bundle.Profiles["test"].Optimization.Search.QualityValues) != 0 {
		t.Fatalf("quality values backfilled over bitrate sweep: %+v", s.Bundle.Profiles["test"].Optimization.Search)
	}

	cases := []struct {
		name   string
		video  string
		search string
		want   string
	}{
		{
			name:   "mixed sweep",
			video:  videoQuality,
			search: "        max_candidates: 4\n        quality_values: [60, 65]\n        bitrate_values: [3200, 3500]",
			want:   "mutually exclusive",
		},
		{
			name:   "bitrate base with quality search",
			video:  videoBitrate,
			search: "        max_candidates: 2\n        quality_values: [60, 65]",
			want:   "must match the search dimension",
		},
		{
			name:   "quality base with bitrate search",
			video:  videoQuality,
			search: "        max_candidates: 2\n        bitrate_values: [3200, 3500]",
			want:   "must match the search dimension",
		},
		{
			name:   "bitrate sweep for libx265",
			video:  videoX265,
			search: "        max_candidates: 2\n        bitrate_values: [3200, 3500]",
			want:   "only supported for hevc_videotoolbox, not libx265",
		},
		{
			name:   "unordered bitrates",
			video:  videoBitrate,
			search: "        max_candidates: 2\n        bitrate_values: [3500, 3200]",
			want:   "strictly ordered",
		},
		{
			name:   "duplicate bitrates",
			video:  videoBitrate,
			search: "        max_candidates: 2\n        bitrate_values: [3500, 3500]",
			want:   "strictly ordered",
		},
		{
			name:   "bitrates exceed max candidates",
			video:  videoBitrate,
			search: "        max_candidates: 1\n        bitrate_values: [3200, 3500]",
			want:   "exceeds max_candidates",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(withOpt(tc.video, tc.search))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}
