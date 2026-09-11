package transcodeworker

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/transcode"
)

func TestPlan_SubtitleStreamPlanner(t *testing.T) {
	// Exact test scenario from specifications:
	// input subtitle streams:
	//   0: mov_text
	//   1: ass
	//   2: subrip
	// output subtitle codec decisions:
	//   0: subrip
	//   1: copy
	//   2: copy
	plan := &transcode.Plan{
		Container:                    "mkv",
		VideoCodec:                   "hevc_videotoolbox",
		Quality:                      65,
		AudioMode:                    "copy",
		SubtitleMode:                 "preserve",
		ConvertIncompatibleSubtitles: true,
		PreserveMetadata:             true,
		PreserveChapters:             true,
		PreserveAttachments:          true,
	}

	inputStreams := []SourceStream{
		{Index: 0, TypeIndex: 0, Kind: "video", Codec: "h264"},
		{Index: 1, TypeIndex: 0, Kind: "audio", Codec: "ac3", Language: "eng"},
		{Index: 2, TypeIndex: 0, Kind: "subtitle", Codec: "mov_text", Language: "eng"},
		{Index: 3, TypeIndex: 1, Kind: "subtitle", Codec: "ass", Language: "jpn"},
		{Index: 4, TypeIndex: 2, Kind: "subtitle", Codec: "subrip", Language: "spa"},
	}

	execPlan, err := BuildExecutionPlan(plan, inputStreams, 120.0)
	if err != nil {
		t.Fatalf("BuildExecutionPlan failed: %v", err)
	}

	// Verify subtitle stream decisions
	subDecisions := make(map[int]string)
	for _, s := range execPlan.Streams {
		if s.Kind == "subtitle" {
			subDecisions[s.TypeIndex] = s.TargetCodec
		}
	}

	if subDecisions[0] != "subrip" {
		t.Errorf("expected subtitle 0 targetCodec 'subrip', got %q", subDecisions[0])
	}
	if subDecisions[1] != "copy" {
		t.Errorf("expected subtitle 1 targetCodec 'copy', got %q", subDecisions[1])
	}
	if subDecisions[2] != "copy" {
		t.Errorf("expected subtitle 2 targetCodec 'copy', got %q", subDecisions[2])
	}

	// Verify conversion recording
	if len(execPlan.Conversions) != 1 {
		t.Fatalf("expected 1 conversion, got %d", len(execPlan.Conversions))
	}
	conv := execPlan.Conversions[0]
	if conv.StreamType != "subtitle" || conv.StreamIndex != 0 || conv.FromCodec != "mov_text" || conv.ToCodec != "subrip" {
		t.Errorf("unexpected conversion: %+v", conv)
	}
	if conv.Reason != "matroska_compatibility" {
		t.Errorf("expected reason 'matroska_compatibility', got %q", conv.Reason)
	}

	// Verify FFmpeg argument generation
	args, err := BuildFFmpegArgs(execPlan, "/path/to/source.mp4", "/path/to/candidate.mkv", "/path/to/progress.txt")
	if err != nil {
		t.Fatalf("BuildFFmpegArgs failed: %v", err)
	}

	argsStr := strings.Join(args, " ")
	if !strings.Contains(argsStr, "-c:s:0 subrip") {
		t.Errorf("expected '-c:s:0 subrip' in args, got: %s", argsStr)
	}
	if !strings.Contains(argsStr, "-c:s:1 copy") {
		t.Errorf("expected '-c:s:1 copy' in args, got: %s", argsStr)
	}
	if !strings.Contains(argsStr, "-c:s:2 copy") {
		t.Errorf("expected '-c:s:2 copy' in args, got: %s", argsStr)
	}
	// Verify global -c copy or global -c:s srt is NOT present
	for i, a := range args {
		if a == "-c" && i+1 < len(args) && args[i+1] == "copy" {
			t.Errorf("dangerous global '-c copy' found in args: %s", argsStr)
		}
		if a == "-c:s" && i+1 < len(args) && (args[i+1] == "srt" || args[i+1] == "subrip") {
			t.Errorf("destructive global '-c:s srt/subrip' found in args: %s", argsStr)
		}
	}
}

func TestPlan_MultipleProfilesDifferentArgv(t *testing.T) {
	planQuality := &transcode.Plan{
		Container:  "mkv",
		VideoCodec: "hevc_videotoolbox",
		Quality:    55,
		AudioMode:  "copy",
	}
	planSpace := &transcode.Plan{
		Container:  "mkv",
		VideoCodec: "hevc_videotoolbox",
		Quality:    75,
		AudioMode:  "copy",
	}

	streams := []SourceStream{
		{Index: 0, TypeIndex: 0, Kind: "video", Codec: "h264"},
	}

	execQuality, _ := BuildExecutionPlan(planQuality, streams, 60.0)
	execSpace, _ := BuildExecutionPlan(planSpace, streams, 60.0)

	argsQ, _ := BuildFFmpegArgs(execQuality, "in.mp4", "out.mkv", "prog.txt")
	argsS, _ := BuildFFmpegArgs(execSpace, "in.mp4", "out.mkv", "prog.txt")

	strQ := strings.Join(argsQ, " ")
	strS := strings.Join(argsS, " ")

	if !strings.Contains(strQ, "-q:v 55") {
		t.Errorf("expected -q:v 55 in args, got: %s", strQ)
	}
	if !strings.Contains(strS, "-q:v 75") {
		t.Errorf("expected -q:v 75 in args, got: %s", strS)
	}
	if strQ == strS {
		t.Errorf("expected different arguments for different profile qualities")
	}
}

func TestPlan_IncompatibleSubtitleFailClosed(t *testing.T) {
	plan := &transcode.Plan{
		Container:                    "mkv",
		VideoCodec:                   "hevc_videotoolbox",
		Quality:                      65,
		AudioMode:                    "copy",
		SubtitleMode:                 "preserve",
		ConvertIncompatibleSubtitles: true,
	}

	// Subtitle codec that is neither supported in Matroska nor convertible
	streams := []SourceStream{
		{Index: 0, TypeIndex: 0, Kind: "video", Codec: "h264"},
		{Index: 1, TypeIndex: 0, Kind: "subtitle", Codec: "unknown_incompatible_format"},
	}

	_, err := BuildExecutionPlan(plan, streams, 100.0)
	if err == nil {
		t.Fatal("expected error on unknown incompatible subtitle codec, got nil")
	}
	if !strings.Contains(err.Error(), "unsupported subtitle codec") || !strings.Contains(err.Error(), "fail closed") {
		t.Errorf("expected fail closed error message, got: %v", err)
	}
}

func TestPlan_DisabledConvertIncompatibleFailClosed(t *testing.T) {
	plan := &transcode.Plan{
		Container:                    "mkv",
		VideoCodec:                   "hevc_videotoolbox",
		Quality:                      65,
		AudioMode:                    "copy",
		SubtitleMode:                 "preserve",
		ConvertIncompatibleSubtitles: false, // Disabled conversion
	}

	streams := []SourceStream{
		{Index: 0, TypeIndex: 0, Kind: "video", Codec: "h264"},
		{Index: 1, TypeIndex: 0, Kind: "subtitle", Codec: "mov_text"},
	}

	_, err := BuildExecutionPlan(plan, streams, 100.0)
	if err == nil {
		t.Fatal("expected error when convert_incompatible_subtitles is false, got nil")
	}
	if !strings.Contains(err.Error(), "incompatible subtitle codec") {
		t.Errorf("expected incompatible error, got: %v", err)
	}
}

func TestPlan_SummarizeFFmpegError(t *testing.T) {
	tempDir := t.TempDir()
	logPath := filepath.Join(tempDir, "ffmpeg.log")

	content := `ffmpeg version 7.0 Copyright (c) 2000-2024 the FFmpeg developers
[matroska @ 0x123456] Subtitle codec mov_text (94213) is not supported to that coin: matroska
[matroska @ 0x123456] Could not write header for output file #0 (incorrect codec parameters ?): Function not implemented
Error initializing output stream 0:1 --
Conversion failed!
`
	if err := os.WriteFile(logPath, []byte(content), 0644); err != nil {
		t.Fatalf("writing log: %v", err)
	}

	summary := SummarizeFFmpegError(logPath, errors.New("exit status 178"))
	if !strings.Contains(summary, "mov_text") || !strings.Contains(summary, "not supported") {
		t.Errorf("expected log summary to capture mov_text error, got: %s", summary)
	}
	if !strings.Contains(summary, "exit status 178") {
		t.Errorf("expected log summary to mention exit status, got: %s", summary)
	}
}

func TestPlan_WorkerValidationFailClosed(t *testing.T) {
	// Plan with invalid values directly sent to worker
	badPlan := &transcode.Plan{
		Container:    "mp4", // not yet enabled on worker
		VideoCodec:   "hevc_videotoolbox",
		Quality:      65,
		AudioMode:    "copy",
		SubtitleMode: "preserve",
	}
	err := ValidatePlan(badPlan)
	if err == nil {
		t.Fatal("expected error on unsupported container, got nil")
	}

	badCodecPlan := &transcode.Plan{
		Container:    "mkv",
		VideoCodec:   "hevc_nvenc",
		Quality:      65,
		AudioMode:    "copy",
		SubtitleMode: "preserve",
	}
	err = ValidatePlan(badCodecPlan)
	if err == nil {
		t.Fatal("expected error on unsupported video codec, got nil")
	}
}
