package mediainspect

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// previewTrackProbe is an MP4 whose muxer wrote its chapter thumbnails as a
// second mjpeg video track (one picture per chapter, so under 0.01 fps) beside
// a flagged cover.
const previewTrackProbe = `{
  "streams": [
    {"index": 0, "codec_type": "video", "codec_name": "h264", "pix_fmt": "yuv420p", "width": 1920, "height": 804, "bits_per_raw_sample": "8", "avg_frame_rate": "2071444319/86396490", "r_frame_rate": "24000/1001", "disposition": {"default": 1, "attached_pic": 0}},
    {"index": 1, "codec_type": "audio", "codec_name": "aac", "channels": 6, "tags": {"language": "eng"}},
    {"index": 6, "codec_type": "video", "codec_name": "mjpeg", "pix_fmt": "yuvj444p", "width": 1493, "height": 1748, "avg_frame_rate": "0/0", "r_frame_rate": "90000/1", "disposition": {"default": 0, "attached_pic": 1}},
    {"index": 11, "codec_type": "video", "codec_name": "mjpeg", "pix_fmt": "yuvj420p", "width": 640, "height": 268, "avg_frame_rate": "24500/4338459", "r_frame_rate": "24000/1001", "disposition": {"default": 0, "attached_pic": 0, "timed_thumbnails": 0}}
  ],
  "format": {"format_name": "mov,mp4,m4a,3gp,3g2,mj2", "duration": "8682.6", "size": "12522200530"}
}`

func TestMainVideoLeavesPreviewTrackOut(t *testing.T) {
	dir := t.TempDir()
	media := filepath.Join(dir, "Movie.mp4")
	if err := os.WriteFile(media, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	rep, err := InspectDetailed(context.Background(), createMockFFprobe(t, dir, previewTrackProbe), media)
	if err != nil || !rep.Probed {
		t.Fatalf("inspect: %v", err)
	}
	if len(rep.Video) != 3 {
		t.Fatalf("Video must keep every video stream, got %d", len(rep.Video))
	}
	main, covers, previews := rep.MainVideo(), rep.CoverArt(), rep.ThumbnailTracks()
	if len(main) != 1 || main[0].Index != 0 || main[0].Codec != "h264" {
		t.Fatalf("main video: %+v", main)
	}
	if len(covers) != 1 || covers[0].Index != 6 {
		t.Fatalf("cover art: %+v", covers)
	}
	if len(previews) != 1 || previews[0].Index != 11 || previews[0].FPS >= 0.01 {
		t.Fatalf("preview tracks: %+v", previews)
	}
	var nilReport *DetailedReport
	if nilReport.ThumbnailTracks() != nil {
		t.Fatal("nil report must have no preview tracks")
	}
}

func TestIsThumbnailTrack(t *testing.T) {
	still := map[string]int{"default": 0}
	for _, tc := range []struct {
		name        string
		kind, codec string
		fps         float64
		disposition map[string]int
		want        bool
	}{
		{"chapter thumbnails", "video", "mjpeg", 0.0056, still, true},
		{"png slideshow", "video", "png", 0.5, still, true},
		{"kind not stated", "", "MJPEG", 0.25, still, true},
		{"motion jpeg video", "video", "mjpeg", 23.976, still, false},
		{"one frame per second", "video", "mjpeg", 1, still, false},
		{"slow h264", "video", "h264", 0.5, still, false},
		{"no frame rate", "video", "mjpeg", 0, still, false},
		{"cover art", "video", "mjpeg", 0.0056, map[string]int{"attached_pic": 1}, false},
		{"audio", "audio", "mjpeg", 0.5, still, false},
	} {
		if got := IsThumbnailTrack(tc.kind, tc.codec, tc.fps, tc.disposition); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A file whose only video is a preview-like track keeps it as its programme:
// there is nothing else to encode, so it is not dropped.
func TestPreviewTrackAloneStaysProgrammeVideo(t *testing.T) {
	alone := []DetailedStream{{Index: 0, Kind: "video", Codec: "mjpeg", FPS: 0.25}}
	if main := MainVideoStreams(alone); len(main) != 1 || main[0].Index != 0 {
		t.Fatalf("main video: %+v", main)
	}
	if previews := ThumbnailTrackStreams(alone); len(previews) != 0 {
		t.Fatalf("preview tracks: %+v", previews)
	}
	beside := append([]DetailedStream{{Index: 0, Kind: "video", Codec: "mjpeg", FPS: 0.25}}, DetailedStream{Index: 1, Kind: "video", Codec: "h264", FPS: 24})
	if main := MainVideoStreams(beside); len(main) != 1 || main[0].Index != 1 {
		t.Fatalf("a preview track listed first was taken as the programme: %+v", main)
	}
}
