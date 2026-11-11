package transcodeworker

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/transcode"
)

// ffmpeg copies an attachment's name and MIME type as they are: an empty
// type stays empty (it is not rewritten as application/octet-stream) and a
// legacy font type is not renamed, so the exact comparison holds a copied
// font to its source's. A BMP cover re-attached as image/bmp reads back as
// an attachment, not a picture; the cover art count, checked first, refuses
// it before the attachment rule sees it.
func TestRealAttachmentsCopiedAsTheyAre(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe unavailable")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.Command(ffmpeg, append([]string{"-v", "error", "-y"}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("ffmpeg %v: %v\n%s", args, err, out)
		}
	}
	font := filepath.Join(dir, "a.ttf")
	if err := os.WriteFile(font, bytes.Repeat([]byte{7}, 3000), 0o600); err != nil {
		t.Fatal(err)
	}
	bmp := filepath.Join(dir, "cover.bmp")
	run("-f", "lavfi", "-i", "color=c=red:s=32x32", "-frames:v", "1", bmp)
	// ffmpeg will not write an empty MIME type, so the source gets a
	// placeholder that is then blanked: FileMimeType of size 0 followed by
	// an EBML Void element over the rest.
	marked := filepath.Join(dir, "marked.mkv")
	run("-f", "lavfi", "-i", "color=c=black:s=64x64:d=1", "-c:v", "ffv1",
		"-attach", font, "-metadata:s:t:0", "mimetype=x/abcde",
		"-attach", font, "-metadata:s:t:1", "mimetype=application/x-truetype-font", "-metadata:s:t:1", "filename=b.ttf",
		marked)
	data, err := os.ReadFile(marked)
	if err != nil {
		t.Fatal(err)
	}
	placeholder := []byte("\x46\x60\x87x/abcde")
	if bytes.Count(data, placeholder) != 1 {
		t.Fatal("placeholder MIME type not found once")
	}
	src := filepath.Join(dir, "source.mkv")
	if err := os.WriteFile(src, bytes.Replace(data, placeholder, []byte("\x46\x60\x80\xec\x85\x00\x00\x00\x00\x00"), 1), 0o600); err != nil {
		t.Fatal(err)
	}
	// The encode's attachment arguments: every source attachment mapped
	// and copied, the source's metadata kept.
	cand := filepath.Join(dir, "candidate.mkv")
	run("-i", src, "-map", "0:v:0", "-map", "0:t?", "-map_metadata", "0", "-c:v", "copy", "-c:t", "copy", cand)
	probe := func(path string) []SourceStream {
		t.Helper()
		streams, _, err := ProbeSourceStreams(context.Background(), ffprobe, path)
		if err != nil {
			t.Fatal(err)
		}
		return streams
	}
	srcStreams, candStreams := probe(src), probe(cand)
	var mimes []string
	for _, s := range streamsOfKind(srcStreams, "attachment") {
		mimes = append(mimes, s.MimeType)
	}
	if len(mimes) != 2 || mimes[0] != "" || mimes[1] != "application/x-truetype-font" {
		t.Fatalf("source attachment types %q", mimes)
	}
	if err := attachmentsPreserved(srcStreams, candStreams); err != nil {
		t.Fatalf("copied attachments refused: %v", err)
	}

	// A BMP cover, attached as the encode re-attaches covers.
	covered := filepath.Join(dir, "covered.mkv")
	run("-f", "lavfi", "-i", "color=c=black:s=64x64:d=1", "-c:v", "ffv1",
		"-attach", bmp, "-metadata:s:t:0", "mimetype=image/bmp", "-metadata:s:t:0", "filename=cover_0.bmp", covered)
	streams := probe(covered)
	if n := countCoverArt(streams); n != 0 {
		t.Fatalf("a BMP cover read back as %d pictures", n)
	}
	if n := len(streamsOfKind(streams, "attachment")); n != 1 {
		t.Fatalf("a BMP cover read back as %d attachments", n)
	}
}

// Matroska writes a language tag outside ISO 639-2 ("pob", "chs", a
// two-letter "en", a BCP 47 "pt-BR") and reads it back unchanged, so a
// copied track's language is compared exactly (transcode.SameLanguage).
func TestRealLanguageTagsCopiedAsTheyAre(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe unavailable")
	}
	dir := t.TempDir()
	tags := []string{"pob", "chs", "en", "pt-BR", "und"}
	args := []string{"-v", "error", "-y", "-f", "lavfi", "-i", "color=c=black:s=64x64:d=1"}
	for range tags {
		args = append(args, "-f", "lavfi", "-i", "anullsrc=d=1")
	}
	args = append(args, "-map", "0")
	for i, tag := range tags {
		args = append(args, "-map", strconv.Itoa(i+1), fmt.Sprintf("-metadata:s:a:%d", i), "language="+tag)
	}
	src := filepath.Join(dir, "source.mkv")
	args = append(args, "-c:v", "ffv1", "-c:a", "flac", src)
	if out, err := exec.Command(ffmpeg, args...).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v\n%s", err, out)
	}
	cand := filepath.Join(dir, "candidate.mkv")
	if out, err := exec.Command(ffmpeg, "-v", "error", "-y", "-i", src, "-map", "0", "-map_metadata", "0", "-c", "copy", cand).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v\n%s", err, out)
	}
	srcStreams, _, err := ProbeSourceStreams(context.Background(), ffprobe, src)
	if err != nil {
		t.Fatal(err)
	}
	candStreams, _, err := ProbeSourceStreams(context.Background(), ffprobe, cand)
	if err != nil {
		t.Fatal(err)
	}
	sa, ca := streamsOfKind(srcStreams, "audio"), streamsOfKind(candStreams, "audio")
	if len(sa) != len(tags) || len(ca) != len(tags) {
		t.Fatalf("%d and %d audio tracks, want %d", len(sa), len(ca), len(tags))
	}
	for i, tag := range tags {
		if !transcode.SameLanguage(sa[i].Language, ca[i].Language) {
			t.Errorf("track %d: source %q, copy %q", i, sa[i].Language, ca[i].Language)
		}
		if tag != "und" && !strings.EqualFold(sa[i].Language, tag) {
			t.Errorf("track %d: written %q, read back %q", i, tag, sa[i].Language)
		}
	}
}
