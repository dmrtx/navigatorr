package mediainspect

import "testing"

func bt2020Display() *MasteringDisplayMetadata {
	return &MasteringDisplayMetadata{RedX: "34000/50000", RedY: "16000/50000", GreenX: "13250/50000", GreenY: "34500/50000", BlueX: "7500/50000", BlueY: "3000/50000", WhitePointX: "15635/50000", WhitePointY: "16450/50000", MinLuminance: "50/10000", MaxLuminance: "10000000/10000"}
}

func hdr10Stream() DetailedStream {
	return DetailedStream{Kind: "video", Codec: "hevc", Profile: "Main 10", PixelFormat: "yuv420p10le", BitDepth: 10, FPS: 23.976, ColorRange: "tv", ColorSpace: "bt2020nc", ColorPrimaries: "bt2020", ColorTransfer: "smpte2084", MasteringDisplay: bt2020Display(), ContentLightLevel: &ContentLightLevelMetadata{MaxCLL: 1000, MaxFALL: 400}}
}

func hdr10Frames() *FrameSignals {
	return &FrameSignals{Probed: true, Frames: 8, ColorTransfer: "smpte2084", ColorPrimaries: "bt2020", ColorSpace: "bt2020nc", ColorRange: "tv", MasteringDisplay: bt2020Display(), ContentLightLevel: &ContentLightLevelMetadata{MaxCLL: 1000, MaxFALL: 400}}
}

func dovi(profile, compat int) SideDataRecord {
	return SideDataRecord{SideDataType: "DOVI configuration record", Data: map[string]any{"dv_version_major": float64(1), "dv_profile": float64(profile), "dv_level": float64(6), "rpu_present_flag": float64(1), "el_present_flag": float64(0), "bl_present_flag": float64(1), "dv_bl_signal_compatibility_id": float64(compat)}}
}

func TestClassifyDynamicRange(t *testing.T) {
	sdr := DetailedStream{Kind: "video", Codec: "h264", PixelFormat: "yuv420p", BitDepth: 8, ColorPrimaries: "bt709", ColorTransfer: "bt709", ColorSpace: "bt709"}
	cases := []struct {
		name        string
		stream      func() DetailedStream
		frames      func() *FrameSignals
		class       string
		reason      string
		convertible bool
		dvApproval  bool
		baseLayer   string
	}{
		{name: "sdr without frame probe", stream: func() DetailedStream { return sdr }, frames: func() *FrameSignals { return nil }, class: DynamicRangeSDR},
		{name: "hdr10", stream: hdr10Stream, frames: hdr10Frames, class: DynamicRangeHDR10, convertible: true},
		{name: "hdr10 metadata only in frames", stream: func() DetailedStream {
			s := hdr10Stream()
			s.ColorTransfer, s.ColorPrimaries, s.MasteringDisplay, s.ContentLightLevel = "", "", nil, nil
			return s
		}, frames: hdr10Frames, class: DynamicRangeHDR10, convertible: true},
		{name: "hdr requires frame evidence", stream: hdr10Stream, frames: func() *FrameSignals { return nil }, class: DynamicRangeUnknownHDR, reason: "hdr_frame_evidence_missing"},
		{name: "pq without mastering display", stream: func() DetailedStream {
			s := hdr10Stream()
			s.MasteringDisplay = nil
			return s
		}, frames: func() *FrameSignals { f := hdr10Frames(); f.MasteringDisplay = nil; return f }, class: DynamicRangePQ10, reason: "hdr10_static_metadata_missing"},
		{name: "hlg", stream: func() DetailedStream {
			s := hdr10Stream()
			s.ColorTransfer, s.MasteringDisplay, s.ContentLightLevel = "arib-std-b67", nil, nil
			return s
		}, frames: func() *FrameSignals {
			return &FrameSignals{Probed: true, Frames: 8, ColorTransfer: "arib-std-b67", ColorPrimaries: "bt2020", ColorSpace: "bt2020nc"}
		}, class: DynamicRangeHLG, reason: "hlg_not_supported"},
		{name: "hdr10plus", stream: hdr10Stream, frames: func() *FrameSignals { f := hdr10Frames(); f.HDR10Plus = true; return f }, class: DynamicRangeHDR10Plus, reason: "hdr10plus_dynamic_metadata_not_preserved"},
		{name: "other dynamic metadata", stream: hdr10Stream, frames: func() *FrameSignals {
			f := hdr10Frames()
			f.DynamicMetadata = []string{"HDR Vivid dynamic metadata"}
			return f
		}, class: DynamicRangeUnknownHDR, reason: "hdr_dynamic_metadata_unsupported"},
		{name: "dolby vision profile 5", stream: func() DetailedStream {
			s := hdr10Stream()
			s.ColorTransfer, s.ColorPrimaries, s.ColorSpace, s.MasteringDisplay, s.ContentLightLevel = "", "", "", nil, nil
			s.SideData = []SideDataRecord{dovi(5, 0)}
			return s
		}, frames: func() *FrameSignals { return &FrameSignals{Probed: true, Frames: 8, DolbyVisionRPU: true} }, class: DynamicRangeDolbyVision, reason: "dolby_vision_profile_5_no_compatible_base_layer", baseLayer: "none"},
		{name: "dolby vision profile 8.1", stream: func() DetailedStream {
			s := hdr10Stream()
			s.SideData = []SideDataRecord{dovi(8, 1)}
			return s
		}, frames: func() *FrameSignals { f := hdr10Frames(); f.DolbyVisionRPU = true; return f }, class: DynamicRangeDolbyVision, convertible: true, dvApproval: true, baseLayer: DynamicRangeHDR10},
		{name: "dolby vision profile 7 dual layer", stream: func() DetailedStream {
			s := hdr10Stream()
			r := dovi(7, 6)
			r.Data["el_present_flag"] = float64(1)
			s.SideData = []SideDataRecord{r}
			return s
		}, frames: hdr10Frames, class: DynamicRangeDolbyVision, convertible: true, dvApproval: true, baseLayer: DynamicRangeHDR10},
		{name: "dolby vision profile 8.2 sdr base", stream: func() DetailedStream {
			s := sdr
			s.PixelFormat, s.BitDepth = "yuv420p10le", 10
			s.SideData = []SideDataRecord{dovi(8, 2)}
			return s
		}, frames: func() *FrameSignals {
			return &FrameSignals{Probed: true, Frames: 8, ColorTransfer: "bt709", ColorPrimaries: "bt709", ColorSpace: "bt709", DolbyVisionRPU: true}
		}, class: DynamicRangeDolbyVision, reason: "dolby_vision_sdr_base_layer_unsupported", baseLayer: DynamicRangeSDR},
		{name: "dolby vision profile 8.4 hlg base", stream: func() DetailedStream {
			s := hdr10Stream()
			s.ColorTransfer, s.MasteringDisplay = "arib-std-b67", nil
			s.SideData = []SideDataRecord{dovi(8, 4)}
			return s
		}, frames: func() *FrameSignals {
			return &FrameSignals{Probed: true, Frames: 8, ColorTransfer: "arib-std-b67", ColorPrimaries: "bt2020", ColorSpace: "bt2020nc"}
		}, class: DynamicRangeDolbyVision, reason: "dolby_vision_hlg_base_layer_unsupported", baseLayer: DynamicRangeHLG},
		{name: "dolby vision without configuration record", stream: func() DetailedStream {
			s := hdr10Stream()
			s.Tags = map[string]string{"codec_tag_string": "dvh1"}
			return s
		}, frames: hdr10Frames, class: DynamicRangeDolbyVision, reason: "dolby_vision_configuration_missing", baseLayer: "unknown"},
		{name: "dolby vision with hdr10plus", stream: func() DetailedStream {
			s := hdr10Stream()
			s.SideData = []SideDataRecord{dovi(8, 1)}
			return s
		}, frames: func() *FrameSignals { f := hdr10Frames(); f.HDR10Plus = true; return f }, class: DynamicRangeDolbyVision, reason: "hdr10plus_dynamic_metadata_not_preserved", baseLayer: DynamicRangeHDR10},
		{name: "inconsistent mastering display", stream: hdr10Stream, frames: func() *FrameSignals {
			f := hdr10Frames()
			md := *bt2020Display()
			md.MaxLuminance = "40000000/10000"
			f.MasteringDisplay = &md
			return f
		}, class: DynamicRangeHDR10, reason: "hdr_metadata_inconsistent"},
		{name: "inconsistent frames", stream: hdr10Stream, frames: func() *FrameSignals { f := hdr10Frames(); f.Inconsistent = []string{"mastering_display"}; return f }, class: DynamicRangeHDR10, reason: "hdr_metadata_inconsistent"},
		{name: "twelve bit hdr10", stream: func() DetailedStream {
			s := hdr10Stream()
			s.PixelFormat, s.BitDepth = "yuv420p12le", 12
			return s
		}, frames: hdr10Frames, class: DynamicRangeHDR10, reason: "hdr10_requires_10bit_420"},
		{name: "full range hdr10", stream: func() DetailedStream { s := hdr10Stream(); s.ColorRange = "pc"; return s }, frames: func() *FrameSignals { f := hdr10Frames(); f.ColorRange = "pc"; return f }, class: DynamicRangeHDR10, reason: "hdr10_requires_limited_range"},
		{name: "implausible content light", stream: func() DetailedStream { s := hdr10Stream(); s.ContentLightLevel = nil; return s }, frames: func() *FrameSignals {
			f := hdr10Frames()
			f.ContentLightLevel = &ContentLightLevelMetadata{MaxCLL: 100, MaxFALL: 400}
			return f
		}, class: DynamicRangeHDR10, reason: "hdr10_content_light_level_invalid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rep := &DetailedReport{Video: []DetailedStream{tc.stream()}}
			d := ClassifyDynamicRange(rep, tc.frames())
			if d.Class != tc.class || d.Reason != tc.reason {
				t.Fatalf("class/reason = %q/%q, want %q/%q (%+v)", d.Class, d.Reason, tc.class, tc.reason, d)
			}
			if d.HDR10Convertible() != tc.convertible || d.RequiresDolbyVisionRemoval() != tc.dvApproval {
				t.Fatalf("convertible/dv approval = %v/%v, want %v/%v", d.HDR10Convertible(), d.RequiresDolbyVisionRemoval(), tc.convertible, tc.dvApproval)
			}
			if tc.baseLayer != "" && d.BaseLayer != tc.baseLayer {
				t.Fatalf("base layer = %q, want %q", d.BaseLayer, tc.baseLayer)
			}
			if d.Label == "" {
				t.Fatal("label missing")
			}
		})
	}
}

func TestClassifyDynamicRangeRejectsMultipleVideoStreams(t *testing.T) {
	d := ClassifyDynamicRange(&DetailedReport{Video: []DetailedStream{hdr10Stream(), hdr10Stream()}}, hdr10Frames())
	if d.HDR10Convertible() || d.Reason != "hdr_single_video_stream_required" {
		t.Fatalf("got %+v", d)
	}
}

func TestDolbyVisionLabel(t *testing.T) {
	s := hdr10Stream()
	s.SideData = []SideDataRecord{dovi(8, 1)}
	d := ClassifyDynamicRange(&DetailedReport{Video: []DetailedStream{s}}, hdr10Frames())
	if d.Label != "Dolby Vision profile 8.1 (HDR10 base layer)" {
		t.Fatalf("label = %q", d.Label)
	}
}

func TestX265MasterDisplay(t *testing.T) {
	got, err := X265MasterDisplay(bt2020Display())
	if err != nil {
		t.Fatal(err)
	}
	if want := "G(13250,34500)B(7500,3000)R(34000,16000)WP(15635,16450)L(10000000,50)"; got != want {
		t.Fatalf("got %s want %s", got, want)
	}
	// Equivalent rationals produce the same signalled values.
	md := bt2020Display()
	md.RedX, md.MaxLuminance = "0.68", "1000"
	if again, err := X265MasterDisplay(md); err != nil || again != got {
		t.Fatalf("decimal form = %q, %v", again, err)
	}
	if peak, err := MasteringDisplayPeakNits(md); err != nil || peak != 1000 {
		t.Fatalf("peak = %d, %v", peak, err)
	}
	for name, mutate := range map[string]func(*MasteringDisplayMetadata){
		"missing chroma":    func(m *MasteringDisplayMetadata) { m.GreenX = "" },
		"chroma over 1":     func(m *MasteringDisplayMetadata) { m.BlueY = "60000/50000" },
		"negative":          func(m *MasteringDisplayMetadata) { m.RedY = "-1/50000" },
		"peak too low":      func(m *MasteringDisplayMetadata) { m.MaxLuminance = "10/1" },
		"peak too high":     func(m *MasteringDisplayMetadata) { m.MaxLuminance = "20000/1" },
		"min above max":     func(m *MasteringDisplayMetadata) { m.MinLuminance = "2000/1" },
		"garbage luminance": func(m *MasteringDisplayMetadata) { m.MinLuminance = "abc" },
	} {
		m := bt2020Display()
		mutate(m)
		if _, err := X265MasterDisplay(m); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	if _, err := X265MasterDisplay(nil); err == nil {
		t.Fatal("nil accepted")
	}
}

func TestParseFrameSignals(t *testing.T) {
	data := []byte(`{"frames":[
		{"color_transfer":"smpte2084","color_primaries":"bt2020","color_space":"bt2020nc","color_range":"tv","side_data_list":[
			{"side_data_type":"H.26[45] User Data Unregistered SEI message"},
			{"side_data_type":"Mastering display metadata","red_x":"34000/50000","red_y":"16000/50000","green_x":"13250/50000","green_y":"34500/50000","blue_x":"7500/50000","blue_y":"3000/50000","white_point_x":"15635/50000","white_point_y":"16450/50000","min_luminance":"50/10000","max_luminance":"10000000/10000"},
			{"side_data_type":"Content light level metadata","max_content":1000,"max_average":400}]},
		{"color_transfer":"smpte2084","color_primaries":"bt2020","color_space":"bt2020nc","color_range":"tv","side_data_list":[
			{"side_data_type":"HDR Dynamic Metadata SMPTE2094-40 (HDR10+)"},
			{"side_data_type":"Dolby Vision RPU Data"}]}]}`)
	fs, err := ParseFrameSignals(data)
	if err != nil {
		t.Fatal(err)
	}
	if !fs.Probed || fs.Frames != 2 || fs.ColorTransfer != "smpte2084" || fs.MasteringDisplay == nil || fs.ContentLightLevel == nil || fs.ContentLightLevel.MaxCLL != 1000 || !fs.HDR10Plus || !fs.DolbyVisionRPU || len(fs.Inconsistent) != 0 {
		t.Fatalf("unexpected signals %+v", fs)
	}
	mixed := []byte(`{"frames":[{"color_transfer":"smpte2084"},{"color_transfer":"bt709"}]}`)
	if fs, err := ParseFrameSignals(mixed); err != nil || len(fs.Inconsistent) != 1 {
		t.Fatalf("mixed transfer = %+v, %v", fs, err)
	}
	if _, err := ParseFrameSignals([]byte(`{"frames":[]}`)); err == nil {
		t.Fatal("empty frames accepted")
	}
}
