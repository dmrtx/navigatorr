package mediainspect

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// Heuristic mode (no ffprobe binary) still reports container, size,
// sidecars and dangerous names.
func TestHeuristicInspection(t *testing.T) {
	dir := t.TempDir()
	media := filepath.Join(dir, "Show.S01E01.1080p.mkv")
	os.WriteFile(media, []byte("fake-video-bytes"), 0o644)
	os.WriteFile(filepath.Join(dir, "Show.S01E01.1080p.eng.srt"), []byte("subs"), 0o644)

	rep, err := InspectFile(context.Background(), "/nonexistent/ffprobe", media)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if rep.Container != "mkv" {
		t.Errorf("container %q, want mkv", rep.Container)
	}
	if rep.SizeBytes == 0 {
		t.Error("size missing")
	}
	if len(rep.ExternalSubtitles) != 1 {
		t.Errorf("sidecars: %v", rep.ExternalSubtitles)
	}
	if rep.Probed {
		t.Error("probed should be false without ffprobe")
	}
}

func TestDangerousNameFlagged(t *testing.T) {
	dir := t.TempDir()
	evil := filepath.Join(dir, "Episode.1080p.mkv.exe")
	os.WriteFile(evil, []byte("x"), 0o644)
	rep, err := InspectFile(context.Background(), "/nonexistent/ffprobe", evil)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if len(rep.DangerousFiles) != 1 {
		t.Errorf("dangerous file not flagged: %+v", rep)
	}
}

func TestMissingFileErrors(t *testing.T) {
	if _, err := InspectFile(context.Background(), "/nonexistent/ffprobe",
		filepath.Join(t.TempDir(), "nope.mkv")); err == nil {
		t.Error("missing file should error")
	}
}
