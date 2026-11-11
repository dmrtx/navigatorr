package transcodeworker

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"
)

// decodedSamplePTS is what probeSamplePTS read before it read packets: the
// best-effort timestamp of every decoded picture.
func decodedSamplePTS(t *testing.T, ctx context.Context, ffprobe, path string) []float64 {
	t.Helper()
	out, err := exec.CommandContext(ctx, ffprobe, "-v", "error", "-select_streams", "v:0", "-show_entries", "frame=best_effort_timestamp_time", "-of", "json", path).Output()
	if err != nil {
		t.Fatal(err)
	}
	var log struct {
		Frames []struct {
			PTS string `json:"best_effort_timestamp_time"`
		} `json:"frames"`
	}
	if err := json.Unmarshal(out, &log); err != nil {
		t.Fatal(err)
	}
	pts := make([]float64, len(log.Frames))
	for i, f := range log.Frames {
		if pts[i], err = strconv.ParseFloat(f.PTS, 64); err != nil {
			t.Fatal(err)
		}
	}
	return pts
}

// The packets of a real FFV1 extract carry exactly the timestamps its decoded
// pictures do, one per picture, in order: from a window at the start and one
// past a seek, from a constant-rate source, from an AVI whose 1/fps time base
// rounds differently, and from a variable-rate source.
func TestSamplePacketTimestampsMatchDecodedFrames(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	dir := t.TempDir()
	sources := []struct {
		name string
		fps  float64
		args []string
	}{
		{"cfr.mkv", 25, []string{"-f", "lavfi", "-i", "testsrc2=size=160x90:rate=25:duration=4", "-c:v", "libx264", "-preset", "ultrafast", "-bf", "2"}},
		{"ntsc.avi", 24000.0 / 1001, []string{"-f", "lavfi", "-i", "testsrc2=size=160x90:rate=24000/1001:duration=4", "-c:v", "mpeg4", "-q:v", "5"}},
		{"vfr.mkv", 25, []string{"-f", "lavfi", "-i", "testsrc2=size=160x90:rate=25:duration=4", "-vf", "setpts='PTS+if(gte(N,30),0.013/TB,0)'", "-fps_mode", "passthrough", "-c:v", "libx264", "-preset", "ultrafast"}},
	}
	for _, s := range sources {
		in := filepath.Join(dir, s.name)
		runFFmpeg(t, ctx, ffmpeg, append(append([]string{"-y", "-nostdin", "-v", "error"}, s.args...), in)...)
		for _, start := range []float64{0, 1.3} {
			out := filepath.Join(dir, s.name+"-"+strconv.FormatFloat(start, 'f', 1, 64)+".mkv")
			extractSampleWindow(t, ctx, ffmpeg, ffprobe, in, out, start, 1.5, s.fps)
			packets, err := probeSamplePTS(ctx, ffprobe, out)
			if err != nil {
				t.Fatalf("%s at %.1f s: %v", s.name, start, err)
			}
			if frames := decodedSamplePTS(t, ctx, ffprobe, out); len(frames) == 0 || !slices.Equal(packets, frames) {
				t.Fatalf("%s at %.1f s: packet timestamps %v, decoded %v", s.name, start, packets, frames)
			}
		}
	}
}

// The packet reader refuses what it cannot vouch for: anything but one FFV1
// stream (whose packets need not be pictures in order), no packets, a
// timestamp it cannot read, or timestamps out of order. A packet without a
// presentation timestamp takes its decode timestamp.
func TestParseSamplePacketPTS(t *testing.T) {
	for _, c := range []struct {
		name, json string
		want       []float64
	}{
		{"ffv1", `{"streams":[{"codec_name":"ffv1"}],"packets":[{"pts_time":"0.000000","dts_time":"0.000000"},{"pts_time":"0.040000","dts_time":"0.040000"}]}`, []float64{0, 0.04}},
		{"missing pts", `{"streams":[{"codec_name":"ffv1"}],"packets":[{"pts_time":"0.000000"},{"pts_time":"N/A","dts_time":"0.040000"}]}`, []float64{0, 0.04}},
		{"not ffv1", `{"streams":[{"codec_name":"h264"}],"packets":[{"pts_time":"0.000000"}]}`, nil},
		{"no stream", `{"packets":[{"pts_time":"0.000000"}]}`, nil},
		{"no packets", `{"streams":[{"codec_name":"ffv1"}],"packets":[]}`, nil},
		{"unreadable", `{"streams":[{"codec_name":"ffv1"}],"packets":[{"pts_time":"N/A","dts_time":"N/A"}]}`, nil},
		{"out of order", `{"streams":[{"codec_name":"ffv1"}],"packets":[{"pts_time":"0.040000"},{"pts_time":"0.000000"}]}`, nil},
	} {
		got, err := parseSamplePacketPTS([]byte(c.json))
		if c.want == nil {
			if err == nil {
				t.Errorf("%s: accepted %v", c.name, got)
			}
			continue
		}
		if err != nil || !slices.Equal(got, c.want) {
			t.Errorf("%s: %v %v, want %v", c.name, got, err, c.want)
		}
	}
}
