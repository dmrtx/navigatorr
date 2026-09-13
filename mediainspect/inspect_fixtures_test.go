package mediainspect

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// createMockFFprobe creates an executable mock script that outputs the given json string when called.
func createMockFFprobe(t *testing.T, dir, jsonOutput string) string {
	t.Helper()
	scriptPath := filepath.Join(dir, fmt.Sprintf("mock_ffprobe_%d.sh", os.Getpid()))
	content := "#!/bin/sh\ncat << 'EOF'\n" + jsonOutput + "\nEOF\n"
	if err := os.WriteFile(scriptPath, []byte(content), 0o755); err != nil {
		t.Fatalf("failed to create mock ffprobe: %v", err)
	}
	return scriptPath
}

func TestDetailedInspection_H264_8Bit(t *testing.T) {
	dir := t.TempDir()
	mediaFile := filepath.Join(dir, "Test.H264.8bit.mkv")
	_ = os.WriteFile(mediaFile, []byte("fake-payload"), 0o644)

	probeJSON := `{
		"streams": [
			{
				"index": 0,
				"codec_type": "video",
				"codec_name": "h264",
				"profile": "High",
				"pix_fmt": "yuv420p",
				"r_frame_rate": "24/1",
				"avg_frame_rate": "24/1",
				"width": 1920,
				"height": 1080,
				"bits_per_raw_sample": "8",
				"bit_rate": "8000000",
				"color_range": "tv",
				"color_space": "bt709",
				"color_primaries": "bt709",
				"color_transfer": "bt709"
			}
		],
		"format": {
			"format_name": "matroska,webm",
			"duration": "120.0"
		},
		"chapters": []
	}`

	mockFFprobe := createMockFFprobe(t, dir, probeJSON)
	rep, err := InspectDetailed(context.Background(), mockFFprobe, mediaFile)
	if err != nil {
		t.Fatalf("InspectDetailed failed: %v", err)
	}
	if !rep.Probed {
		t.Fatalf("expected probed=true")
	}
	if len(rep.Video) != 1 {
		t.Fatalf("expected 1 video stream, got %d", len(rep.Video))
	}
	v := rep.Video[0]
	if v.Codec != "h264" {
		t.Errorf("expected codec h264, got %s", v.Codec)
	}
	if v.Profile != "High" {
		t.Errorf("expected profile High, got %s", v.Profile)
	}
	if v.PixelFormat != "yuv420p" {
		t.Errorf("expected pixel_format yuv420p, got %s", v.PixelFormat)
	}
	if v.BitDepth != 8 {
		t.Errorf("expected bit_depth 8, got %d", v.BitDepth)
	}
	if v.FrameRate != "24/1" {
		t.Errorf("expected frame_rate 24/1, got %s", v.FrameRate)
	}
	if v.FPS != 24.0 {
		t.Errorf("expected fps 24.0, got %f", v.FPS)
	}
	if v.BitRate != 8000000 {
		t.Errorf("expected bit_rate 8000000, got %d", v.BitRate)
	}
	if v.ColorRange != "tv" || v.ColorSpace != "bt709" || v.ColorPrimaries != "bt709" || v.ColorTransfer != "bt709" {
		t.Errorf("unexpected color metadata: range=%s space=%s primaries=%s transfer=%s",
			v.ColorRange, v.ColorSpace, v.ColorPrimaries, v.ColorTransfer)
	}
	if rep.HDR != nil {
		t.Errorf("expected no HDR on standard 8-bit BT.709, got %+v", rep.HDR)
	}
}

func TestDetailedInspection_H264_10Bit(t *testing.T) {
	dir := t.TempDir()
	mediaFile := filepath.Join(dir, "Anime.Hi10P.H264.mkv")
	_ = os.WriteFile(mediaFile, []byte("fake-payload"), 0o644)

	probeJSON := `{
		"streams": [
			{
				"index": 0,
				"codec_type": "video",
				"codec_name": "h264",
				"profile": "High 10",
				"pix_fmt": "yuv420p10le",
				"r_frame_rate": "24000/1001",
				"avg_frame_rate": "24000/1001",
				"width": 1920,
				"height": 1080,
				"bits_per_raw_sample": "10",
				"bit_rate": "12500000"
			}
		],
		"format": {
			"format_name": "matroska,webm",
			"duration": "1420.0"
		},
		"chapters": []
	}`

	mockFFprobe := createMockFFprobe(t, dir, probeJSON)
	rep, err := InspectDetailed(context.Background(), mockFFprobe, mediaFile)
	if err != nil {
		t.Fatalf("InspectDetailed failed: %v", err)
	}
	if len(rep.Video) != 1 {
		t.Fatalf("expected 1 video stream, got %d", len(rep.Video))
	}
	v := rep.Video[0]
	if v.Profile != "High 10" {
		t.Errorf("expected High 10, got %s", v.Profile)
	}
	if v.PixelFormat != "yuv420p10le" {
		t.Errorf("expected yuv420p10le, got %s", v.PixelFormat)
	}
	if v.BitDepth != 10 {
		t.Errorf("expected bit_depth 10, got %d", v.BitDepth)
	}
	if v.FrameRate != "24000/1001" {
		t.Errorf("expected frame_rate 24000/1001, got %s", v.FrameRate)
	}
	expectedFPS := 24000.0 / 1001.0
	if math.Abs(v.FPS-expectedFPS) > 0.0001 {
		t.Errorf("expected fps ~%f, got %f", expectedFPS, v.FPS)
	}
	if v.BitRate != 12500000 {
		t.Errorf("expected bit_rate 12500000, got %d", v.BitRate)
	}
}

func TestDetailedInspection_HEVC_Main10(t *testing.T) {
	dir := t.TempDir()
	mediaFile := filepath.Join(dir, "Movie.HEVC.Main10.mkv")
	_ = os.WriteFile(mediaFile, []byte("fake-payload"), 0o644)

	probeJSON := `{
		"streams": [
			{
				"index": 0,
				"codec_type": "video",
				"codec_name": "hevc",
				"profile": "Main 10",
				"pix_fmt": "p010le",
				"r_frame_rate": "24000/1001",
				"avg_frame_rate": "24000/1001",
				"width": 3840,
				"height": 2160,
				"bits_per_raw_sample": "10",
				"tags": {
					"BPS": "18000000"
				}
			}
		],
		"format": {
			"format_name": "matroska,webm",
			"duration": "7200.0"
		},
		"chapters": []
	}`

	mockFFprobe := createMockFFprobe(t, dir, probeJSON)
	rep, err := InspectDetailed(context.Background(), mockFFprobe, mediaFile)
	if err != nil {
		t.Fatalf("InspectDetailed failed: %v", err)
	}
	if len(rep.Video) != 1 {
		t.Fatalf("expected 1 video stream, got %d", len(rep.Video))
	}
	v := rep.Video[0]
	if v.Codec != "hevc" {
		t.Errorf("expected codec hevc, got %s", v.Codec)
	}
	if v.Profile != "Main 10" {
		t.Errorf("expected profile Main 10, got %s", v.Profile)
	}
	if v.PixelFormat != "p010le" {
		t.Errorf("expected pixel_format p010le, got %s", v.PixelFormat)
	}
	if v.BitDepth != 10 {
		t.Errorf("expected bit_depth 10, got %d", v.BitDepth)
	}
	if v.BitRate != 18000000 {
		t.Errorf("expected bit_rate parsed from BPS tag 18000000, got %d", v.BitRate)
	}
}

func TestDetailedInspection_HDR_Metadata(t *testing.T) {
	dir := t.TempDir()
	mediaFile := filepath.Join(dir, "UHD.HDR10.mkv")
	_ = os.WriteFile(mediaFile, []byte("fake-payload"), 0o644)

	probeJSON := `{
		"streams": [
			{
				"index": 0,
				"codec_type": "video",
				"codec_name": "hevc",
				"profile": "Main 10",
				"pix_fmt": "yuv420p10le",
				"r_frame_rate": "24/1",
				"width": 3840,
				"height": 2160,
				"bits_per_raw_sample": "10",
				"color_range": "tv",
				"color_space": "bt2020nc",
				"color_primaries": "bt2020",
				"color_transfer": "smpte2084",
				"side_data_list": [
					{
						"side_data_type": "Mastering display metadata",
						"red_x": "34000/50000",
						"red_y": "16000/50000",
						"green_x": "13250/50000",
						"green_y": "34500/50000",
						"blue_x": "7500/50000",
						"blue_y": "3000/50000",
						"white_point_x": "15635/50000",
						"white_point_y": "16450/50000",
						"min_luminance": "50/10000",
						"max_luminance": "10000000/10000"
					},
					{
						"side_data_type": "Content light level metadata",
						"max_content": 1000,
						"max_average": 400
					},
					{
						"side_data_type": "DOVI configuration record",
						"dv_version_major": 1,
						"dv_version_minor": 0,
						"dv_profile": 8
					}
				]
			}
		],
		"format": {
			"format_name": "matroska,webm",
			"duration": "5400.0"
		},
		"chapters": []
	}`

	mockFFprobe := createMockFFprobe(t, dir, probeJSON)
	rep, err := InspectDetailed(context.Background(), mockFFprobe, mediaFile)
	if err != nil {
		t.Fatalf("InspectDetailed failed: %v", err)
	}
	if rep.HDR == nil {
		t.Fatalf("expected rep.HDR to be populated for HDR10 media")
	}
	if !rep.HDR.Present {
		t.Errorf("expected HDR.Present=true")
	}
	if rep.HDR.ColorPrimaries != "bt2020" || rep.HDR.ColorTransfer != "smpte2084" {
		t.Errorf("unexpected HDR colors: %+v", rep.HDR)
	}

	v := rep.Video[0]
	if len(v.SideData) != 3 {
		t.Fatalf("expected 3 side data records, got %d", len(v.SideData))
	}
	if v.MasteringDisplay == nil {
		t.Fatalf("expected MasteringDisplay to be parsed")
	}
	if v.MasteringDisplay.RedX != "34000/50000" || v.MasteringDisplay.MaxLuminance != "10000000/10000" {
		t.Errorf("unexpected mastering display values: %+v", v.MasteringDisplay)
	}
	if v.ContentLightLevel == nil {
		t.Fatalf("expected ContentLightLevel to be parsed")
	}
	if v.ContentLightLevel.MaxCLL != 1000 || v.ContentLightLevel.MaxFALL != 400 {
		t.Errorf("unexpected content light level: %+v", v.ContentLightLevel)
	}
}

func TestDetailedInspection_MultipleAudioStreams(t *testing.T) {
	dir := t.TempDir()
	mediaFile := filepath.Join(dir, "MultiAudio.mkv")
	_ = os.WriteFile(mediaFile, []byte("fake-payload"), 0o644)

	probeJSON := `{
		"streams": [
			{
				"index": 0,
				"codec_type": "video",
				"codec_name": "h264",
				"width": 1920,
				"height": 1080
			},
			{
				"index": 1,
				"codec_type": "audio",
				"codec_name": "aac",
				"channels": 6,
				"channel_layout": "5.1",
				"bit_rate": "384000",
				"tags": {
					"language": "jpn",
					"title": "Japanese 5.1"
				},
				"disposition": {
					"default": 1
				}
			},
			{
				"index": 2,
				"codec_type": "audio",
				"codec_name": "opus",
				"channels": 2,
				"channel_layout": "stereo",
				"bit_rate": "128000",
				"tags": {
					"language": "eng",
					"title": "English Commentary"
				},
				"disposition": {
					"comment": 1
				}
			}
		],
		"format": {
			"format_name": "matroska,webm",
			"duration": "1400.0"
		},
		"chapters": []
	}`

	mockFFprobe := createMockFFprobe(t, dir, probeJSON)
	rep, err := InspectDetailed(context.Background(), mockFFprobe, mediaFile)
	if err != nil {
		t.Fatalf("InspectDetailed failed: %v", err)
	}
	if len(rep.Audio) != 2 {
		t.Fatalf("expected 2 audio streams, got %d", len(rep.Audio))
	}
	a1 := rep.Audio[0]
	if a1.Codec != "aac" || a1.Channels != 6 || a1.ChannelLayout != "5.1" || a1.Language != "jpn" || a1.BitRate != 384000 {
		t.Errorf("unexpected audio 1: %+v", a1)
	}
	a2 := rep.Audio[1]
	if a2.Codec != "opus" || a2.Channels != 2 || a2.ChannelLayout != "stereo" || a2.Language != "eng" || a2.BitRate != 128000 {
		t.Errorf("unexpected audio 2: %+v", a2)
	}
}

func TestDetailedInspection_ASS_And_Fonts_And_Chapters(t *testing.T) {
	dir := t.TempDir()
	mediaFile := filepath.Join(dir, "AnimeWithFonts.mkv")
	_ = os.WriteFile(mediaFile, []byte("fake-payload"), 0o644)

	probeJSON := `{
		"streams": [
			{
				"index": 0,
				"codec_type": "video",
				"codec_name": "h264",
				"width": 1920,
				"height": 1080
			},
			{
				"index": 1,
				"codec_type": "subtitle",
				"codec_name": "ass",
				"tags": {
					"language": "eng",
					"title": "Full ASS Subtitles"
				}
			},
			{
				"index": 2,
				"codec_type": "attachment",
				"codec_name": "ttf",
				"tags": {
					"filename": "A-OTF-ShinGoPr6N-Regular.otf",
					"mimetype": "application/x-truetype-font"
				}
			},
			{
				"index": 3,
				"codec_type": "attachment",
				"codec_name": "ttf",
				"tags": {
					"filename": "Trebuchet-MS.ttf",
					"mimetype": "application/x-truetype-font"
				}
			}
		],
		"format": {
			"format_name": "matroska,webm",
			"duration": "1420.0"
		},
		"chapters": [
			{
				"id": 0,
				"start_time": "0.000000",
				"end_time": "90.000000",
				"tags": {"title": "Intro"}
			},
			{
				"id": 1,
				"start_time": "90.000000",
				"end_time": "1200.000000",
				"tags": {"title": "Episode Part A"}
			},
			{
				"id": 2,
				"start_time": "1200.000000",
				"end_time": "1420.000000",
				"tags": {"title": "Ending & Credits"}
			}
		]
	}`

	mockFFprobe := createMockFFprobe(t, dir, probeJSON)
	rep, err := InspectDetailed(context.Background(), mockFFprobe, mediaFile)
	if err != nil {
		t.Fatalf("InspectDetailed failed: %v", err)
	}

	if len(rep.Subtitles) != 1 {
		t.Fatalf("expected 1 subtitle stream, got %d", len(rep.Subtitles))
	}
	s := rep.Subtitles[0]
	if s.Codec != "ass" || s.Language != "eng" || s.Title != "Full ASS Subtitles" {
		t.Errorf("unexpected subtitle stream: %+v", s)
	}

	if len(rep.Attachments) != 2 {
		t.Fatalf("expected 2 attachments, got %d", len(rep.Attachments))
	}
	if rep.Attachments[0].Tags["filename"] != "A-OTF-ShinGoPr6N-Regular.otf" {
		t.Errorf("unexpected attachment 0: %+v", rep.Attachments[0])
	}
	if rep.Attachments[1].Tags["filename"] != "Trebuchet-MS.ttf" {
		t.Errorf("unexpected attachment 1: %+v", rep.Attachments[1])
	}

	if rep.Chapters != 3 {
		t.Errorf("expected 3 chapters, got %d", rep.Chapters)
	}
}

func TestDetailedReport_JSONRoundTripPreservation(t *testing.T) {
	orig := DetailedReport{
		Path:        "/media/test.mkv",
		Container:   "mkv",
		DurationSec: 1420.5,
		SizeBytes:   1048576,
		Chapters:    4,
		Probed:      true,
		HDR: &HDRReport{
			Present:        true,
			ColorPrimaries: "bt2020",
			ColorTransfer:  "smpte2084",
			ColorSpace:     "bt2020nc",
			MasteringDisplay: &MasteringDisplayMetadata{
				RedX:         "34000/50000",
				MaxLuminance: "10000000/10000",
			},
			ContentLightLevel: &ContentLightLevelMetadata{
				MaxCLL:  1000,
				MaxFALL: 400,
			},
		},
		Video: []DetailedStream{
			{
				Index:          0,
				Kind:           "video",
				Codec:          "hevc",
				Profile:        "Main 10",
				PixelFormat:    "p010le",
				Width:          3840,
				Height:         2160,
				BitDepth:       10,
				FrameRate:      "24000/1001",
				FPS:            23.976023976023978,
				BitRate:        18500000,
				ColorRange:     "tv",
				ColorSpace:     "bt2020nc",
				ColorPrimaries: "bt2020",
				ColorTransfer:  "smpte2084",
				MasteringDisplay: &MasteringDisplayMetadata{
					RedX: "34000/50000",
				},
				ContentLightLevel: &ContentLightLevelMetadata{
					MaxCLL: 1000,
				},
				SideData: []SideDataRecord{
					{
						SideDataType: "Mastering display metadata",
						Data:         map[string]any{"red_x": "34000/50000"},
					},
				},
			},
		},
		Audio: []DetailedStream{
			{
				Index:         1,
				Kind:          "audio",
				Codec:         "aac",
				Language:      "jpn",
				Channels:      6,
				ChannelLayout: "5.1",
				BitRate:       384000,
			},
		},
		Subtitles: []DetailedStream{
			{
				Index:    2,
				Kind:     "subtitle",
				Codec:    "ass",
				Language: "eng",
			},
		},
		Attachments: []DetailedStream{
			{
				Index: 3,
				Kind:  "attachment",
				Codec: "ttf",
				Tags:  map[string]string{"filename": "font.ttf"},
			},
		},
	}

	data, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}

	var restored DetailedReport
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("json.Unmarshal failed: %v", err)
	}

	if !reflect.DeepEqual(orig, restored) {
		t.Errorf("Round trip mismatch:\norig:     %+v\nrestored: %+v", orig, restored)
	}
}

func TestDetailedInspection_FormatBitRateAndFrameRates(t *testing.T) {
	dir := t.TempDir()
	mediaFile := filepath.Join(dir, "rates.mkv")
	_ = os.WriteFile(mediaFile, []byte("fake-video"), 0o644)

	probeJSON := `{
		"streams": [
			{
				"index": 0,
				"codec_type": "video",
				"codec_name": "hevc",
				"r_frame_rate": "24/1",
				"avg_frame_rate": "24000/1001",
				"bit_rate": "8000000"
			}
		],
		"format": {
			"format_name": "matroska",
			"duration": "100.0",
			"bit_rate": "8500000"
		}
	}`

	mockFFprobe := createMockFFprobe(t, dir, probeJSON)
	rep, err := InspectDetailed(context.Background(), mockFFprobe, mediaFile)
	if err != nil {
		t.Fatalf("InspectDetailed failed: %v", err)
	}

	if rep.BitRate != 8500000 {
		t.Errorf("expected format bit_rate=8500000, got %d", rep.BitRate)
	}

	if len(rep.Video) != 1 {
		t.Fatalf("expected 1 video stream, got %d", len(rep.Video))
	}
	v := rep.Video[0]
	if v.RFrameRate != "24/1" {
		t.Errorf("expected r_frame_rate='24/1', got %q", v.RFrameRate)
	}
	if v.AvgFrameRate != "24000/1001" {
		t.Errorf("expected avg_frame_rate='24000/1001', got %q", v.AvgFrameRate)
	}
	// FPS should be computed from avg_frame_rate first (23.976), not r_frame_rate (24.0)
	if v.FPS < 23.97 || v.FPS > 23.98 {
		t.Errorf("expected FPS computed from avg_frame_rate (~23.976), got %v", v.FPS)
	}
}

func TestDetailedInspection_MasteringDisplayNoNilStringsAndBoundedSideData(t *testing.T) {
	dir := t.TempDir()
	mediaFile := filepath.Join(dir, "hdr_partial.mkv")
	_ = os.WriteFile(mediaFile, []byte("fake-video"), 0o644)

	// Create side data with partial mastering display (omitted fields) and more than 10 side-data entries
	probeJSON := `{
		"streams": [
			{
				"index": 0,
				"codec_type": "video",
				"codec_name": "hevc",
				"side_data_list": [
					{
						"side_data_type": "Mastering display metadata",
						"red_x": "34000/50000",
						"max_luminance": "10000000/10000"
					},
					{"side_data_type": "entry2"},
					{"side_data_type": "entry3"},
					{"side_data_type": "entry4"},
					{"side_data_type": "entry5"},
					{"side_data_type": "entry6"},
					{"side_data_type": "entry7"},
					{"side_data_type": "entry8"},
					{"side_data_type": "entry9"},
					{"side_data_type": "entry10"},
					{"side_data_type": "entry11"},
					{"side_data_type": "entry12"}
				]
			}
		],
		"format": {
			"format_name": "matroska",
			"duration": "50.0"
		}
	}`

	mockFFprobe := createMockFFprobe(t, dir, probeJSON)
	rep, err := InspectDetailed(context.Background(), mockFFprobe, mediaFile)
	if err != nil {
		t.Fatalf("InspectDetailed failed: %v", err)
	}

	if len(rep.Video) != 1 {
		t.Fatalf("expected 1 video stream, got %d", len(rep.Video))
	}
	v := rep.Video[0]

	// 1. Verify side data list is bounded to maxSideDataEntries (10)
	if len(v.SideData) > 10 {
		t.Errorf("expected side data entries to be bounded to 10, got %d", len(v.SideData))
	}

	// 2. Verify partial mastering display does not contain "<nil>" strings
	if v.MasteringDisplay == nil {
		t.Fatalf("expected mastering display to be non-nil")
	}
	if v.MasteringDisplay.RedX != "34000/50000" {
		t.Errorf("expected red_x='34000/50000', got %q", v.MasteringDisplay.RedX)
	}
	if v.MasteringDisplay.MinLuminance != "" {
		t.Errorf("expected unpopulated min_luminance to be empty string, got %q", v.MasteringDisplay.MinLuminance)
	}

	// 3. Verify JSON serialization never includes "<nil>"
	data, err := json.Marshal(v.MasteringDisplay)
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}
	jsonStr := string(data)
	if strings.Contains(jsonStr, "<nil>") {
		t.Errorf("JSON output contains '<nil>': %s", jsonStr)
	}
	if strings.Contains(jsonStr, "min_luminance") {
		t.Errorf("JSON output should omit empty min_luminance: %s", jsonStr)
	}
}

func TestDetailedInspection_HDRColorSpaceBT2020NC_and_BT2020C(t *testing.T) {
	cases := []struct {
		name       string
		colorSpace string
	}{
		{"bt2020nc", "bt2020nc"},
		{"bt2020c", "bt2020c"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := DetailedStream{
				Kind:       "video",
				Codec:      "hevc",
				ColorSpace: tc.colorSpace,
			}
			if !isHDRStream(st) {
				t.Errorf("expected isHDRStream=true for ColorSpace=%q", tc.colorSpace)
			}
		})
	}
}

func TestDetailedInspection_ParseFrameRateRational_RejectsNaNAndInf(t *testing.T) {
	invalidRates := []string{
		"NaN",
		"NaN/1",
		"1/NaN",
		"Inf",
		"-Inf",
		"Inf/1",
		"1/Inf",
		"+Inf",
		"0/0",
		"-24/1",
		"0/1",
		"",
	}

	for _, raw := range invalidRates {
		fps, ok := ParseFrameRateRational(raw)
		if ok || fps != 0 {
			t.Errorf("ParseFrameRateRational(%q) expected false, 0; got %v, %v", raw, ok, fps)
		}
	}

	// Valid rational rates
	fps, ok := ParseFrameRateRational("24000/1001")
	if !ok || math.Abs(fps-23.976023976) > 0.001 {
		t.Errorf("ParseFrameRateRational(24000/1001) expected ~23.976, got %v", fps)
	}
}

func TestDetailedInspection_ParseBitRateAndCLL_RejectsNonFiniteAndOverflow(t *testing.T) {
	// ParseBitRate tests
	invalidBitrates := []any{
		"NaN",
		"Inf",
		"-Inf",
		math.NaN(),
		math.Inf(1),
		math.Inf(-1),
		1e25, // overflows int64
		-500,
	}
	for _, raw := range invalidBitrates {
		br := ParseBitRate(raw, nil)
		if br != 0 {
			t.Errorf("ParseBitRate(%v) expected 0, got %d", raw, br)
		}
	}

	// Valid bitrate
	if br := ParseBitRate("8000000", nil); br != 8000000 {
		t.Errorf("expected 8000000, got %d", br)
	}
	if br := ParseBitRate(float64(8000000), nil); br != 8000000 {
		t.Errorf("expected 8000000, got %d", br)
	}

	// CLL conversions
	invalidCLLMap := map[string]any{
		"max_content": math.NaN(),
		"max_average": math.Inf(1),
	}
	if cll := parseContentLightLevelMetadata(invalidCLLMap); cll != nil {
		t.Errorf("expected nil CLL for NaN/Inf, got %+v", cll)
	}

	overflowCLLMap := map[string]any{
		"max_content": 1e20, // exceeds MaxInt32
	}
	if cll := parseContentLightLevelMetadata(overflowCLLMap); cll != nil {
		t.Errorf("expected nil CLL for overflow > MaxInt32, got %+v", cll)
	}

	validCLLMap := map[string]any{
		"max_content": 1000,
		"max_average": 400,
	}
	cll := parseContentLightLevelMetadata(validCLLMap)
	if cll == nil || cll.MaxCLL != 1000 || cll.MaxFALL != 400 {
		t.Errorf("unexpected valid CLL: %+v", cll)
	}
}

func TestDetailedInspection_SanitizeSideDataMap_DeterministicKeyOrder(t *testing.T) {
	// Map with more keys than maxSideDataKeys (15)
	raw := map[string]any{
		"z_key": "last",
		"a_key": "first",
		"m_key": "middle",
		"k01":   1,
		"k02":   2,
		"k03":   3,
		"k04":   4,
		"k05":   5,
		"k06":   6,
		"k07":   7,
		"k08":   8,
		"k09":   9,
		"k10":   10,
		"k11":   11,
		"k12":   12,
		"k13":   13,
		"k14":   14,
		"k15":   15,
	}

	// Repeated runs must produce identical keys in identical order
	sanitized1 := sanitizeSideDataMap(raw)
	data1, _ := json.Marshal(sanitized1)

	for i := 0; i < 20; i++ {
		sanitized2 := sanitizeSideDataMap(raw)
		data2, _ := json.Marshal(sanitized2)
		if string(data1) != string(data2) {
			t.Fatalf("non-deterministic output in sanitizeSideDataMap:\nrun1: %s\nrun2: %s", string(data1), string(data2))
		}
	}
}

func TestDetailedInspection_NumericBoundaries(t *testing.T) {
	// 1. ParseBitRate with float64(math.MaxInt64) and string 2^63
	maxInt64Float := float64(math.MaxInt64)
	if br := ParseBitRate(maxInt64Float, nil); br != 0 {
		t.Errorf("expected 0 for float64(math.MaxInt64), got %d", br)
	}
	if br := ParseBitRate("9223372036854775808", nil); br != 0 {
		t.Errorf("expected 0 for 2^63 string, got %d", br)
	}

	// 2. ParseBitRate tag ordering: exact BPS preferred over BPS-*
	tagsPreferred := map[string]string{
		"BPS-eng": "1000000",
		"BPS":     "5000000",
		"BPS-fra": "2000000",
	}
	if br := ParseBitRate(nil, tagsPreferred); br != 5000000 {
		t.Errorf("expected exact BPS tag (5000000), got %d", br)
	}

	// Deterministic sorting when multiple BPS-* tags exist and no exact BPS
	tagsSorted := map[string]string{
		"BPS-rus": "3000000",
		"BPS-eng": "1000000",
		"BPS-fra": "2000000",
	}
	for i := 0; i < 20; i++ {
		if br := ParseBitRate(nil, tagsSorted); br != 1000000 {
			t.Fatalf("expected deterministic first BPS tag BPS-eng (1000000), got %d", br)
		}
	}

	// 3. ParseDuration
	durations := []struct {
		raw      string
		expected float64
	}{
		{"NaN", 0},
		{"+Inf", 0},
		{"-Inf", 0},
		{"-12.5", 0},
		{"0", 0},
		{"0.0", 0},
		{"", 0},
		{"   ", 0},
		{"120.5", 120.5},
	}
	for _, tc := range durations {
		if got := ParseDuration(tc.raw); got != tc.expected {
			t.Errorf("ParseDuration(%q) = %v, expected %v", tc.raw, got, tc.expected)
		}
	}

	// 4. Content light level int exceeding MaxInt32
	cllOverflowInt := map[string]any{
		"max_content": int(math.MaxInt32 + 1000),
	}
	if cll := parseContentLightLevelMetadata(cllOverflowInt); cll != nil {
		t.Errorf("expected nil CLL for int > MaxInt32, got %+v", cll)
	}

	cllOverflowString := map[string]any{
		"max_content": "3000000000", // > MaxInt32 (2147483647)
	}
	if cll := parseContentLightLevelMetadata(cllOverflowString); cll != nil {
		t.Errorf("expected nil CLL for string > MaxInt32, got %+v", cll)
	}

	cllMixed := map[string]any{
		"max_content": int(math.MaxInt32 + 1000),
		"max_average": 400,
	}
	if cll := parseContentLightLevelMetadata(cllMixed); cll == nil || cll.MaxCLL != 0 || cll.MaxFALL != 400 {
		t.Errorf("expected MaxCLL=0, MaxFALL=400 for overflow max_content, got %+v", cll)
	}
}

func TestDetailedInspection_FrameRateFallbackAssignsValidRational(t *testing.T) {
	dir := t.TempDir()
	mediaFile := filepath.Join(dir, "Test.FrameRateFallback.mkv")
	_ = os.WriteFile(mediaFile, []byte("fake-payload"), 0o644)

	probeJSON := `{
		"streams": [
			{
				"index": 0,
				"codec_type": "video",
				"codec_name": "h264",
				"r_frame_rate": "24/1",
				"avg_frame_rate": "0/0",
				"width": 1920,
				"height": 1080
			}
		],
		"format": {
			"format_name": "matroska",
			"duration": "60.0"
		}
	}`

	mockFFprobe := createMockFFprobe(t, dir, probeJSON)
	rep, err := InspectDetailed(context.Background(), mockFFprobe, mediaFile)
	if err != nil {
		t.Fatalf("InspectDetailed failed: %v", err)
	}
	if len(rep.Video) != 1 {
		t.Fatalf("expected 1 video stream, got %d", len(rep.Video))
	}
	v := rep.Video[0]
	if v.FrameRate != "24/1" {
		t.Errorf("expected FrameRate '24/1', got %q", v.FrameRate)
	}
	if v.FPS != 24.0 {
		t.Errorf("expected FPS 24.0, got %f", v.FPS)
	}
}
