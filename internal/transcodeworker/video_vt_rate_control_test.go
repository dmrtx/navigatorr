package transcodeworker

import (
	"reflect"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/transcode"
)

func intPtr(v int) *int { return &v }

func TestBuildVideoEncoderArgsBitrateMode(t *testing.T) {
	plan := &transcode.Plan{
		VideoCodec:         "hevc_videotoolbox",
		AverageBitrateKbps: 3500,
		VideoProfile:       "main",
		PixelFormat:        "yuv420p",
		PrioritizeSpeed:    boolPtr(false),
		SpatialAQ:          boolPtr(true),
		Realtime:           boolPtr(false),
		ExpectedBitDepth:   8,
	}
	got, err := BuildVideoEncoderArgs(plan)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"-c:v", "hevc_videotoolbox", "-b:v", "3500k",
		"-profile:v", "main", "-pix_fmt", "yuv420p",
		"-prio_speed", "0", "-spatial_aq", "1", "-realtime", "0",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected args\nwant=%v\n got=%v", want, got)
	}
}

func TestBuildVideoEncoderArgsConstantBitrateAndMaxrate(t *testing.T) {
	plan := &transcode.Plan{
		VideoCodec:         "hevc_videotoolbox",
		AverageBitrateKbps: 3500,
		MaxBitrateKbps:     5000,
		ConstantBitrate:    boolPtr(true),
	}
	got, err := BuildVideoEncoderArgs(plan)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"-c:v", "hevc_videotoolbox", "-b:v", "3500k",
		"-constant_bit_rate", "1", "-maxrate", "5000k",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected args\nwant=%v\n got=%v", want, got)
	}
}

func TestBuildVideoEncoderArgsOfflineKnobs(t *testing.T) {
	plan := &transcode.Plan{
		VideoCodec:     "hevc_videotoolbox",
		Quality:        65,
		QMin:           intPtr(0),
		QMax:           intPtr(51),
		GOPSize:        intPtr(300),
		BFrames:        intPtr(0),
		ClosedGOP:      boolPtr(true),
		PowerEfficient: boolPtr(false),
		MaxRefFrames:   intPtr(4),
	}
	got, err := BuildVideoEncoderArgs(plan)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"-c:v", "hevc_videotoolbox", "-q:v", "65",
		"-qmin", "0", "-qmax", "51", "-g", "300", "-bf", "0",
		"-flags", "+cgop", "-power_efficient", "0", "-max_ref_frames", "4",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected args\nwant=%v\n got=%v", want, got)
	}
}

func TestBuildVideoEncoderArgsBFramesBooleanSemantics(t *testing.T) {
	// Upstream FFmpeg uses -bf only as an on/off switch (avctx->max_b_frames
	// > 0, reported as depth 2 for HEVC): 0 disables reordering, 1 enables
	// it, and anything above 1 fails closed instead of implying tunable depth.
	on := &transcode.Plan{VideoCodec: "hevc_videotoolbox", Quality: 65, BFrames: intPtr(1)}
	got, err := BuildVideoEncoderArgs(on)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"-c:v", "hevc_videotoolbox", "-q:v", "65", "-bf", "1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected args\nwant=%v\n got=%v", want, got)
	}
	for _, depth := range []int{2, 3, 16} {
		bad := &transcode.Plan{VideoCodec: "hevc_videotoolbox", Quality: 65, BFrames: intPtr(depth)}
		if _, err := BuildVideoEncoderArgs(bad); err == nil || !strings.Contains(err.Error(), "invalid b_frames") {
			t.Fatalf("b_frames %d did not fail closed: %v", depth, err)
		}
	}
}

func TestBuildVideoEncoderArgsClosedGOPExplicitFalse(t *testing.T) {
	plan := &transcode.Plan{VideoCodec: "hevc_videotoolbox", Quality: 65, ClosedGOP: boolPtr(false)}
	got, err := BuildVideoEncoderArgs(plan)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"-c:v", "hevc_videotoolbox", "-q:v", "65", "-flags", "-cgop"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected args\nwant=%v\n got=%v", want, got)
	}
}

func TestBuildVideoEncoderArgsRateControlFailClosed(t *testing.T) {
	cases := []struct {
		name string
		plan *transcode.Plan
		want string
	}{
		{
			name: "quality and bitrate together",
			plan: &transcode.Plan{VideoCodec: "hevc_videotoolbox", Quality: 65, AverageBitrateKbps: 3500},
			want: "mutually exclusive",
		},
		{
			name: "constant bitrate without average",
			plan: &transcode.Plan{VideoCodec: "hevc_videotoolbox", Quality: 65, ConstantBitrate: boolPtr(true)},
			want: "constant_bitrate requires average_bitrate_kbps",
		},
		{
			name: "maxrate without average",
			plan: &transcode.Plan{VideoCodec: "hevc_videotoolbox", Quality: 65, MaxBitrateKbps: 5000},
			want: "max_bitrate_kbps requires average_bitrate_kbps",
		},
		{
			name: "maxrate below average",
			plan: &transcode.Plan{VideoCodec: "hevc_videotoolbox", AverageBitrateKbps: 3500, MaxBitrateKbps: 3000},
			want: "must be >= average_bitrate_kbps",
		},
		{
			name: "qmin above qmax",
			plan: &transcode.Plan{VideoCodec: "hevc_videotoolbox", Quality: 65, QMin: intPtr(40), QMax: intPtr(30)},
			want: "qmin (40) must be <= qmax (30)",
		},
		{
			name: "negative qmin",
			plan: &transcode.Plan{VideoCodec: "hevc_videotoolbox", Quality: 65, QMin: intPtr(-1)},
			want: "invalid qmin",
		},
		{
			name: "zero gop",
			plan: &transcode.Plan{VideoCodec: "hevc_videotoolbox", Quality: 65, GOPSize: intPtr(0)},
			want: "invalid gop_size",
		},
		{
			name: "negative bframes",
			plan: &transcode.Plan{VideoCodec: "hevc_videotoolbox", Quality: 65, BFrames: intPtr(-1)},
			want: "invalid b_frames",
		},
		{
			name: "zero max ref frames",
			plan: &transcode.Plan{VideoCodec: "hevc_videotoolbox", Quality: 65, MaxRefFrames: intPtr(0)},
			want: "invalid max_ref_frames",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := BuildVideoEncoderArgs(tc.plan); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestBuildLibX265ArgsRejectsVideoToolboxKnobs(t *testing.T) {
	cases := []struct {
		name string
		plan *transcode.Plan
		want string
	}{
		{
			name: "average bitrate",
			plan: &transcode.Plan{VideoCodec: "libx265", Quality: 24, Preset: "slow", AverageBitrateKbps: 3500},
			want: "not supported for libx265",
		},
		{
			name: "max bitrate",
			plan: &transcode.Plan{VideoCodec: "libx265", Quality: 24, Preset: "slow", AverageBitrateKbps: 3500, MaxBitrateKbps: 5000},
			want: "not supported for libx265",
		},
		{
			name: "constant bitrate",
			plan: &transcode.Plan{VideoCodec: "libx265", Quality: 24, Preset: "slow", AverageBitrateKbps: 3500, ConstantBitrate: boolPtr(true)},
			want: "not supported for libx265",
		},
		{
			name: "qmin",
			plan: &transcode.Plan{VideoCodec: "libx265", Quality: 24, Preset: "slow", QMin: intPtr(0)},
			want: "not supported for libx265",
		},
		{
			name: "gop size",
			plan: &transcode.Plan{VideoCodec: "libx265", Quality: 24, Preset: "slow", GOPSize: intPtr(250)},
			want: "not supported for libx265",
		},
		{
			name: "bframes",
			plan: &transcode.Plan{VideoCodec: "libx265", Quality: 24, Preset: "slow", BFrames: intPtr(3)},
			want: "not supported for libx265",
		},
		{
			name: "closed gop",
			plan: &transcode.Plan{VideoCodec: "libx265", Quality: 24, Preset: "slow", ClosedGOP: boolPtr(true)},
			want: "not supported for libx265",
		},
		{
			name: "power efficient",
			plan: &transcode.Plan{VideoCodec: "libx265", Quality: 24, Preset: "slow", PowerEfficient: boolPtr(true)},
			want: "not supported for libx265",
		},
		{
			name: "max ref frames",
			plan: &transcode.Plan{VideoCodec: "libx265", Quality: 24, Preset: "slow", MaxRefFrames: intPtr(4)},
			want: "not supported for libx265",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := BuildVideoEncoderArgs(tc.plan); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestValidateVideoToolboxCapabilitiesNewOptions(t *testing.T) {
	plan := &transcode.Plan{
		VideoCodec:         "hevc_videotoolbox",
		AverageBitrateKbps: 3500,
		ConstantBitrate:    boolPtr(true),
		PowerEfficient:     boolPtr(false),
		MaxRefFrames:       intPtr(3),
	}
	caps := ParseVideoToolboxCapabilities(sampleVideoToolboxHelp)
	// The shared sample help output predates the new options: requesting them
	// must fail closed, not silently drop them.
	if err := ValidateVideoToolboxCapabilities(plan, caps); err == nil || !strings.Contains(err.Error(), "encoder_capability_unsupported") {
		t.Fatalf("new options without capability did not fail closed: %v", err)
	}
	caps.Options = append(caps.Options, "constant_bit_rate", "power_efficient", "max_ref_frames")
	if err := ValidateVideoToolboxCapabilities(plan, caps); err != nil {
		t.Fatalf("new options with capability should validate: %v", err)
	}
}

func TestParseVideoToolboxCapabilitiesNewOptions(t *testing.T) {
	raw := sampleVideoToolboxHelp + "  -constant_bit_rate <boolean>  E..V....... Constant bit rate (default false)\n" +
		"  -power_efficient       <boolean>  E..V....... Prefer power efficiency (default false)\n" +
		"  -max_ref_frames        <int>      E..V....... Maximum reference frames (default 0)\n"
	caps := ParseVideoToolboxCapabilities(raw)
	for _, want := range []string{"constant_bit_rate", "power_efficient", "max_ref_frames"} {
		if !contains(caps.Options, want) {
			t.Fatalf("missing option %s in %+v", want, caps)
		}
	}
}
