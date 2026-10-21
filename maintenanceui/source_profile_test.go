package maintenanceui

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/jakenesler/navigatorr/action"
	"github.com/jakenesler/navigatorr/fsop"
	"github.com/jakenesler/navigatorr/mediainspect"
)

func hdr10Mastering() *mediainspect.MasteringDisplayMetadata {
	return &mediainspect.MasteringDisplayMetadata{RedX: "34000/50000", RedY: "16000/50000", GreenX: "13250/50000", GreenY: "34500/50000", BlueX: "7500/50000", BlueY: "3000/50000", WhitePointX: "15635/50000", WhitePointY: "16450/50000", MinLuminance: "50/10000", MaxLuminance: "10000000/10000"}
}

type profileResponse struct {
	Profile     sourceProfile                 `json:"profile"`
	Goals       map[string]action.GoalVerdict `json:"goals"`
	Recommended string                        `json:"recommended"`
	Error       string                        `json:"error"`
}

// profileFixture serves one fake video from an allow-listed temp root with a
// fake probe, so no ffprobe binary is involved.
func profileFixture(t *testing.T, stream mediainspect.DetailedStream, probeErr error) (func(path string) (int, profileResponse), string) {
	t.Helper()
	s, h := testUI(t)
	root := t.TempDir()
	video := filepath.Join(root, "movie.mkv")
	if err := os.WriteFile(video, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	deps := s.engine.Deps()
	deps.Fs, _ = fsop.NewResolver([]string{root}, nil)
	s.engine = action.NewEngine(deps)
	s.probe = func(_ context.Context, _, path string) (mediainspect.DetailedReport, error) {
		if probeErr != nil {
			return mediainspect.DetailedReport{Path: path}, probeErr
		}
		return mediainspect.DetailedReport{Path: path, Probed: true, SizeBytes: 63_000_000_000, Video: []mediainspect.DetailedStream{stream}}, nil
	}
	s.classify = func(_ context.Context, rep *mediainspect.DetailedReport) (mediainspect.DynamicRange, error) {
		// Frames carry what the stream signals, as a consistent file does.
		v := rep.MainVideo()[0]
		return mediainspect.ClassifyDynamicRange(rep, &mediainspect.FrameSignals{Probed: true, Frames: 8, ColorTransfer: v.ColorTransfer, ColorPrimaries: v.ColorPrimaries, ColorSpace: v.ColorSpace}), nil
	}
	return func(path string) (int, profileResponse) {
		w := request(h, "GET", "/api/maintenance/source-profile?path="+url.QueryEscape(path), "", true)
		var out profileResponse
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(w.Code, w.Body.String())
		}
		return w.Code, out
	}, video
}

func TestSourceProfileHDRSkipsSizeAndRecommendsCompact(t *testing.T) {
	get, video := profileFixture(t, mediainspect.DetailedStream{Kind: "video", Codec: "hevc", Width: 3840, Height: 2160, BitDepth: 10, FPS: 23.976, ColorTransfer: "smpte2084", ColorPrimaries: "bt2020", ColorSpace: "bt2020nc", ColorRange: "tv", PixelFormat: "yuv420p10le", MasteringDisplay: hdr10Mastering()}, nil)
	code, out := get(video)
	if code != 200 {
		t.Fatal(code, out.Error)
	}
	p := out.Profile
	if p.Resolution != "4K" || p.DynamicRange != "HDR10" || p.VideoCodec != "HEVC" || p.BitDepth != 10 || p.SizeBytes != 63_000_000_000 {
		t.Fatalf("%+v", p)
	}
	for _, mode := range []string{"size", "quality", "x265_preserve"} {
		if v := out.Goals[mode]; v.Status != "skip" || v.ReasonCode != "hdr_requires_compact" {
			t.Fatalf("%s: %+v", mode, v)
		}
	}
	if v := out.Goals["compact"]; v.Status != "ok" {
		t.Fatalf("compact: %+v", v)
	}
	if out.Recommended != "compact" {
		t.Fatalf("recommended %q", out.Recommended)
	}
}

func TestSourceProfileSDR1080pSkipsCompactAndRecommendsSize(t *testing.T) {
	get, video := profileFixture(t, mediainspect.DetailedStream{Kind: "video", Codec: "h264", Width: 1920, Height: 1080, BitDepth: 8, FPS: 24, ColorTransfer: "bt709", ColorPrimaries: "bt709", ColorSpace: "bt709"}, nil)
	code, out := get(video)
	if code != 200 {
		t.Fatal(code, out.Error)
	}
	if out.Profile.Resolution != "1080p" || out.Profile.DynamicRange != "SDR" || out.Profile.VideoCodec != "H.264" {
		t.Fatalf("%+v", out.Profile)
	}
	if v := out.Goals["compact"]; v.Status != "skip" || v.ReasonCode != "compact_not_needed" {
		t.Fatalf("compact: %+v", v)
	}
	for _, mode := range []string{"size", "quality", "x265_preserve"} {
		if v := out.Goals[mode]; v.Status != "ok" {
			t.Fatalf("%s: %+v", mode, v)
		}
	}
	if out.Recommended != "size" {
		t.Fatalf("recommended %q", out.Recommended)
	}
}

func TestSourceProfileHEVCSkipsConvertToHEVC(t *testing.T) {
	get, video := profileFixture(t, mediainspect.DetailedStream{Kind: "video", Codec: "hevc", Width: 1920, Height: 1080, BitDepth: 10, FPS: 24, ColorTransfer: "bt709", ColorPrimaries: "bt709", ColorSpace: "bt709"}, nil)
	_, out := get(video)
	if v := out.Goals["x265_preserve"]; v.Status != "skip" || v.ReasonCode != "already_target_codec" {
		t.Fatalf("%+v", v)
	}
}

func TestSourceProfileHDRWithoutFrameEvidenceLeavesCompactUnknown(t *testing.T) {
	s, h := testUI(t)
	root := t.TempDir()
	video := filepath.Join(root, "movie.mkv")
	os.WriteFile(video, []byte("fixture"), 0600)
	deps := s.engine.Deps()
	deps.Fs, _ = fsop.NewResolver([]string{root}, nil)
	s.engine = action.NewEngine(deps)
	s.probe = func(_ context.Context, _, path string) (mediainspect.DetailedReport, error) {
		return mediainspect.DetailedReport{Path: path, Probed: true, Video: []mediainspect.DetailedStream{{Kind: "video", Codec: "hevc", Width: 3840, Height: 2160, BitDepth: 10, FPS: 24, ColorTransfer: "smpte2084"}}}, nil
	}
	s.classify = func(context.Context, *mediainspect.DetailedReport) (mediainspect.DynamicRange, error) {
		return mediainspect.DynamicRange{}, errors.New("frames unreadable")
	}
	w := request(h, "GET", "/api/maintenance/source-profile?path="+url.QueryEscape(video), "", true)
	var out profileResponse
	json.Unmarshal(w.Body.Bytes(), &out)
	if w.Code != 200 || out.Goals["compact"].Status != "unknown" || out.Goals["size"].Status != "skip" || out.Recommended != "" {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestSourceProfileRefusesPathsOutsideTheReadAllowList(t *testing.T) {
	get, _ := profileFixture(t, mediainspect.DetailedStream{Kind: "video", Codec: "h264", Width: 1920, Height: 1080, BitDepth: 8, FPS: 24}, nil)
	outside := filepath.Join(t.TempDir(), "outside.mkv")
	os.WriteFile(outside, []byte("fixture"), 0600)
	for _, path := range []string{outside, "/etc/passwd", "../../etc/passwd", ""} {
		if code, _ := get(path); code != 400 {
			t.Fatalf("%q: %d", path, code)
		}
	}
}

func TestSourceProfileRefusesFoldersAndNonVideoFiles(t *testing.T) {
	get, video := profileFixture(t, mediainspect.DetailedStream{Kind: "video", Codec: "h264", Width: 1920, Height: 1080, BitDepth: 8, FPS: 24}, nil)
	notes := filepath.Join(filepath.Dir(video), "notes.txt")
	os.WriteFile(notes, []byte("x"), 0600)
	for _, path := range []string{filepath.Dir(video), notes} {
		if code, _ := get(path); code != 400 {
			t.Fatalf("%q: %d", path, code)
		}
	}
}

func TestSourceProfileProbeFailureIsAHandledStatus(t *testing.T) {
	get, video := profileFixture(t, mediainspect.DetailedStream{}, errors.New("ffprobe exploded"))
	code, out := get(video)
	// Not a 502/503/504: the console reads those as "server unreachable".
	if code != 422 || out.Error != "could not read this file" {
		t.Fatal(code, out.Error)
	}
}

func TestSourceProfileRequiresSignIn(t *testing.T) {
	_, h := testUI(t)
	if w := request(h, "GET", "/api/maintenance/source-profile?path=%2Fmedia%2Fmovie.mkv", "", false); w.Code != 401 {
		t.Fatal(w.Code)
	}
}
