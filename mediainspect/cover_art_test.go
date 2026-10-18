package mediainspect

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

const coverArtProbe = `{
  "streams": [
    {"index": 0, "codec_type": "video", "codec_name": "h264", "pix_fmt": "yuv420p", "width": 1920, "height": 1080, "bits_per_raw_sample": "8", "avg_frame_rate": "24000/1001", "disposition": {"default": 1, "attached_pic": 0}},
    {"index": 1, "codec_type": "audio", "codec_name": "aac", "channels": 2, "tags": {"language": "eng"}},
    {"index": 2, "codec_type": "attachment", "codec_name": "ttf", "extradata_size": 1024, "tags": {"filename": "font.ttf", "mimetype": "application/x-truetype-font"}},
    {"index": 3, "codec_type": "video", "codec_name": "mjpeg", "pix_fmt": "yuvj420p", "width": 600, "height": 900, "avg_frame_rate": "0/0", "disposition": {"default": 0, "attached_pic": 1}, "tags": {"filename": "cover.jpg", "mimetype": "image/jpeg"}}
  ],
  "format": {"format_name": "matroska,webm", "duration": "1420.5", "size": "1000000"}
}`

func TestMainVideoExcludesAttachedPictures(t *testing.T) {
	dir := t.TempDir()
	media := filepath.Join(dir, "S01E01.mkv")
	if err := os.WriteFile(media, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	rep, err := InspectDetailed(context.Background(), createMockFFprobe(t, dir, coverArtProbe), media)
	if err != nil || !rep.Probed {
		t.Fatalf("inspect: %v", err)
	}
	if len(rep.Video) != 2 {
		t.Fatalf("Video must keep every video stream, got %d", len(rep.Video))
	}
	main, covers := rep.MainVideo(), rep.CoverArt()
	if len(main) != 1 || main[0].Index != 0 || main[0].Codec != "h264" {
		t.Fatalf("main video: %+v", main)
	}
	if len(covers) != 1 || covers[0].Index != 3 || !IsAttachedPicture(covers[0]) {
		t.Fatalf("cover art: %+v", covers)
	}
	if len(rep.Attachments) != 1 {
		t.Fatalf("attachments: %+v", rep.Attachments)
	}
	if IsAttachedPicture(rep.Audio[0]) || IsAttachedPicture(DetailedStream{Kind: "audio", Disposition: map[string]int{"attached_pic": 1}}) {
		t.Fatal("non-video stream classified as cover art")
	}
	var nilReport *DetailedReport
	if nilReport.MainVideo() != nil || nilReport.CoverArt() != nil {
		t.Fatal("nil report must have no streams")
	}
}

func TestInspectDetailedRecordsFieldOrder(t *testing.T) {
	dir := t.TempDir()
	media := filepath.Join(dir, "field.mkv")
	if err := os.WriteFile(media, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	probe := `{"streams":[{"index":0,"codec_type":"video","codec_name":"mpeg2video","pix_fmt":"yuv420p","width":1920,"height":1080,"avg_frame_rate":"50/1","field_order":"tt"}],"format":{"format_name":"matroska","duration":"10","size":"100"}}`
	rep, err := InspectDetailed(context.Background(), createMockFFprobe(t, dir, probe), media)
	if err != nil || len(rep.MainVideo()) != 1 {
		t.Fatalf("inspect: %v", err)
	}
	if got := rep.MainVideo()[0]; got.FieldOrder != "tt" || got.FPS != 50 {
		t.Fatalf("field order not recorded: %+v", got)
	}
}
