package transcodeworker

import (
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/transcode"
)

func signedPlan(t *testing.T, actions []transcode.SubtitleAction) *transcode.Plan {
	t.Helper()
	p := &transcode.Plan{
		Container: "mkv", VideoCodec: "hevc_videotoolbox", Quality: 65,
		AudioMode: "copy", SubtitleMode: "preserve", ConvertIncompatibleSubtitles: true,
		PreserveMetadata: true, PreserveChapters: true, PreserveAttachments: true,
		SubtitleActions: actions, RecipeVersion: "test-1.0.0",
		RecipeDigest: "sha256:1111111111111111111111111111111111111111111111111111111111111111",
		Resilience:   transcode.ResiliencePlan{MaxAttempts: 3, TransientRetries: 2, RetryBackoffSeconds: []int{1, 2}, MaxFallbacks: 1, RetryOn: []string{"worker_busy", "ssh_transient"}},
	}
	for _, a := range actions {
		if a.Operation == "transcode" {
			p.AppliedFallbacks = []string{"container_subtitle_incompatible:apply_container_conversion"}
			break
		}
	}
	d, err := transcode.DigestPlan(p)
	if err != nil {
		t.Fatal(err)
	}
	p.PlanDigest = d
	return p
}

func TestPlan_RecipeResolvedMP4MovTextToMKV(t *testing.T) {
	p := signedPlan(t, []transcode.SubtitleAction{
		{SourceStreamIndex: 2, TypeIndex: 0, SourceCodec: "mov_text", Operation: "transcode", Codec: "subrip", Reason: "matroska_compatibility"},
		{SourceStreamIndex: 3, TypeIndex: 1, SourceCodec: "ass", Operation: "copy", Codec: "copy"},
		{SourceStreamIndex: 4, TypeIndex: 2, SourceCodec: "ssa", Operation: "copy", Codec: "copy"},
	})
	streams := []SourceStream{{Index: 0, TypeIndex: 0, Kind: "video", Codec: "h264"}, {Index: 1, TypeIndex: 0, Kind: "audio", Codec: "ac3", Language: "eng", Channels: 6}, {Index: 2, TypeIndex: 0, Kind: "subtitle", Codec: "mov_text", Language: "eng"}, {Index: 3, TypeIndex: 1, Kind: "subtitle", Codec: "ass", Language: "jpn"}, {Index: 4, TypeIndex: 2, Kind: "subtitle", Codec: "ssa", Language: "spa"}}
	ep, err := BuildExecutionPlan(p, streams, 3600)
	if err != nil {
		t.Fatal(err)
	}
	if len(ep.Conversions) != 1 || ep.Conversions[0].FromCodec != "mov_text" || ep.Conversions[0].ToCodec != "subrip" {
		t.Fatalf("conversions=%+v", ep.Conversions)
	}
	args, err := BuildFFmpegArgs(ep, "in.mp4", "out.mkv", "progress.txt")
	if err != nil {
		t.Fatal(err)
	}
	s := strings.Join(args, " ")
	for _, want := range []string{"-c:v hevc_videotoolbox", "-c:a copy", "-c:s:0 subrip", "-c:s:1 copy", "-c:s:2 copy"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in %s", want, s)
		}
	}
	for i, a := range args {
		if a == "-c" && i+1 < len(args) && args[i+1] == "copy" {
			t.Fatalf("global -c copy forbidden: %s", s)
		}
	}
}

func TestPlan_WorkerRevalidatesCapabilityBoundary(t *testing.T) {
	base := signedPlan(t, nil)
	cases := []struct {
		name   string
		mutate func(*transcode.Plan)
	}{
		{"unknown container", func(p *transcode.Plan) { p.Container = "mp4" }},
		{"arbitrary codec", func(p *transcode.Plan) { p.VideoCodec = "; rm -rf /" }},
		{"arbitrary retry class", func(p *transcode.Plan) { p.Resilience.RetryOn = []string{"exec_shell"} }},
		{"missing recipe identity", func(p *transcode.Plan) { p.RecipeDigest = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cp := *base
			tc.mutate(&cp)
			d, _ := transcode.DigestPlan(&cp)
			cp.PlanDigest = d
			if tc.name == "missing recipe identity" {
				cp.RecipeDigest = ""
				d, _ = transcode.DigestPlan(&cp)
				cp.PlanDigest = d
			}
			if err := ValidatePlan(&cp); err == nil {
				t.Fatal("expected fail closed")
			}
		})
	}
}

func TestPlan_DigestDetectsMutation(t *testing.T) {
	p := signedPlan(t, nil)
	p.Quality = 90
	if err := ValidatePlan(p); err == nil || !strings.Contains(err.Error(), "plan digest") {
		t.Fatalf("expected digest rejection, got %v", err)
	}
}

func TestPlan_RecipeCannotExpandSubtitleCapabilities(t *testing.T) {
	cases := [][]transcode.SubtitleAction{
		{{SourceStreamIndex: 1, TypeIndex: 0, SourceCodec: "made_up", Operation: "copy", Codec: "copy"}},
		{{SourceStreamIndex: 1, TypeIndex: 0, SourceCodec: "ass", Operation: "transcode", Codec: "subrip"}},
		{{SourceStreamIndex: 1, TypeIndex: 0, SourceCodec: "mov_text", Operation: "transcode", Codec: "libx264"}},
		{{SourceStreamIndex: 1, TypeIndex: 0, SourceCodec: "mov_text", Operation: "exec", Codec: "subrip"}},
	}
	for i, actions := range cases {
		p := signedPlan(t, actions)
		if err := ValidatePlan(p); err == nil {
			t.Fatalf("case %d should fail", i)
		}
	}
}

func TestPlan_SourceChangeFailsClosed(t *testing.T) {
	p := signedPlan(t, []transcode.SubtitleAction{{SourceStreamIndex: 2, TypeIndex: 0, SourceCodec: "mov_text", Operation: "transcode", Codec: "subrip", Reason: "matroska_compatibility"}})
	_, err := BuildExecutionPlan(p, []SourceStream{{Index: 0, TypeIndex: 0, Kind: "video", Codec: "h264"}, {Index: 2, TypeIndex: 0, Kind: "subtitle", Codec: "ass"}}, 60)
	if err == nil || !strings.Contains(err.Error(), "source codec changed") {
		t.Fatalf("unexpected err %v", err)
	}
}

func TestPlan_MissingOrExtraSubtitleActionFailsClosed(t *testing.T) {
	p := signedPlan(t, nil)
	_, err := BuildExecutionPlan(p, []SourceStream{{Index: 0, TypeIndex: 0, Kind: "video", Codec: "h264"}, {Index: 1, TypeIndex: 0, Kind: "subtitle", Codec: "ass"}}, 60)
	if err == nil {
		t.Fatal("missing subtitle action accepted")
	}
	p = signedPlan(t, []transcode.SubtitleAction{{SourceStreamIndex: 1, TypeIndex: 0, SourceCodec: "ass", Operation: "copy", Codec: "copy"}})
	_, err = BuildExecutionPlan(p, []SourceStream{{Index: 0, TypeIndex: 0, Kind: "video", Codec: "h264"}}, 60)
	if err == nil || !strings.Contains(err.Error(), "missing subtitle") {
		t.Fatalf("extra action not rejected: %v", err)
	}
}

func TestPlan_LegacyHevcVTShimIsBounded(t *testing.T) {
	p, err := ResolveWorkerPlan("hevc-vt", nil)
	if err != nil {
		t.Fatal(err)
	}
	streams := []SourceStream{{Index: 0, TypeIndex: 0, Kind: "video", Codec: "h264"}, {Index: 1, TypeIndex: 0, Kind: "subtitle", Codec: "mov_text"}, {Index: 2, TypeIndex: 1, Kind: "subtitle", Codec: "ass"}}
	ep, err := BuildExecutionPlan(p, streams, 60)
	if err != nil {
		t.Fatal(err)
	}
	if len(ep.Conversions) != 1 || ep.Conversions[0].ToCodec != "subrip" {
		t.Fatalf("legacy shim conversion=%+v", ep.Conversions)
	}
	if _, err := ResolveWorkerPlan("hevc-vt-quality", nil); err == nil {
		t.Fatal("only hevc-vt legacy shim should remain")
	}
}

func TestPlan_ConversionsPreserveIndividualStreams(t *testing.T) {
	p := signedPlan(t, []transcode.SubtitleAction{{SourceStreamIndex: 2, TypeIndex: 0, SourceCodec: "ass", Operation: "copy", Codec: "copy"}, {SourceStreamIndex: 3, TypeIndex: 1, SourceCodec: "mov_text", Operation: "transcode", Codec: "subrip", Reason: "matroska_compatibility"}, {SourceStreamIndex: 4, TypeIndex: 2, SourceCodec: "subrip", Operation: "copy", Codec: "copy"}})
	streams := []SourceStream{{Index: 0, TypeIndex: 0, Kind: "video", Codec: "h264"}, {Index: 2, TypeIndex: 0, Kind: "subtitle", Codec: "ass"}, {Index: 3, TypeIndex: 1, Kind: "subtitle", Codec: "mov_text"}, {Index: 4, TypeIndex: 2, Kind: "subtitle", Codec: "subrip"}}
	ep, err := BuildExecutionPlan(p, streams, 60)
	if err != nil {
		t.Fatal(err)
	}
	got := map[int]string{}
	for _, s := range ep.Streams {
		if s.Kind == "subtitle" {
			got[s.TypeIndex] = s.TargetCodec
		}
	}
	if got[0] != "copy" || got[1] != "subrip" || got[2] != "copy" {
		t.Fatalf("got %+v", got)
	}
}
