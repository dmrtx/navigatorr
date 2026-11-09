package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/arrservice"
	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/qbit"
	"github.com/jakenesler/navigatorr/store"
	"github.com/mark3labs/mcp-go/server"
)

func shaOf(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

type copyOut struct {
	Status            string `json:"status"`
	Bytes             int64  `json:"bytes"`
	SourceSHA256      string `json:"source_sha256"`
	DestinationSHA256 string `json:"destination_sha256"`
	ReplacedSHA256    string `json:"replaced_sha256"`
}

func TestFsCopyTool(t *testing.T) {
	s, root := fsManageServer(t, false, false)
	src := filepath.Join(root, "Movies", "Film (1997)", "Film (1997).mkv")
	dst := filepath.Join(root, "Backups", "Film (1997).mkv")

	res := callTool(t, s, "fs_copy", map[string]any{"source": src, "destination": dst})
	if res.IsError {
		t.Fatalf("copy failed: %s", resultText(t, res))
	}
	var out copyOut
	decode(t, resultText(t, res), &out)
	if out.Status != "copied" || out.SourceSHA256 != shaOf("data") || out.DestinationSHA256 != shaOf("data") || out.Bytes != 4 {
		t.Errorf("copy = %+v", out)
	}

	// A retry is safe and writes nothing.
	res = callTool(t, s, "fs_copy", map[string]any{"source": src, "destination": dst})
	decode(t, resultText(t, res), &out)
	if res.IsError || out.Status != "already_present" || out.DestinationSHA256 != shaOf("data") {
		t.Errorf("retry = %s", resultText(t, res))
	}

	// A different destination is refused with both hashes.
	os.WriteFile(dst, []byte("older"), 0o644)
	res = callTool(t, s, "fs_copy", map[string]any{"source": src, "destination": dst})
	var differs map[string]any
	decode(t, resultText(t, res), &differs)
	if !res.IsError || differs["status"] != "destination_differs" || differs["destination_sha256"] != shaOf("older") || differs["source_sha256"] != shaOf("data") {
		t.Errorf("differs = %s", resultText(t, res))
	}

	// Replacing needs confirm=true, then allow_destructive.
	res = callTool(t, s, "fs_copy", map[string]any{"source": src, "destination": dst, "replace_sha256": shaOf("older")})
	if !res.IsError || !strings.Contains(resultText(t, res), "confirm=true") {
		t.Errorf("replace without confirm: %s", resultText(t, res))
	}
	res = callTool(t, s, "fs_copy", map[string]any{"source": src, "destination": dst, "replace_sha256": shaOf("older"), "confirm": "true"})
	if !res.IsError || !strings.Contains(resultText(t, res), "allow_destructive") {
		t.Errorf("replace without allow_destructive: %s", resultText(t, res))
	}
	if b, _ := os.ReadFile(dst); string(b) != "older" {
		t.Errorf("destination overwritten: %q", b)
	}
}

func TestFsCopyToolReplacesWhenConfirmed(t *testing.T) {
	s, root := fsManageServer(t, true, false)
	src := filepath.Join(root, "Movies", "Film (1997)", "Film (1997).mkv")
	dst := filepath.Join(root, "Backups", "Film (1997).mkv")
	os.MkdirAll(filepath.Dir(dst), 0o755)
	os.WriteFile(dst, []byte("older"), 0o644)
	old := time.Now().Add(-time.Hour)
	os.Chtimes(dst, old, old)

	res := callTool(t, s, "fs_copy", map[string]any{"source": src, "destination": dst, "replace_sha256": strings.ToUpper(shaOf("older")), "confirm": "true"})
	if res.IsError {
		t.Fatalf("replace failed: %s", resultText(t, res))
	}
	var out copyOut
	decode(t, resultText(t, res), &out)
	if out.Status != "replaced" || out.ReplacedSHA256 != shaOf("older") || out.DestinationSHA256 != shaOf("data") {
		t.Errorf("replace = %+v", out)
	}
	if b, _ := os.ReadFile(dst); string(b) != "data" {
		t.Errorf("destination = %q", b)
	}
}

// inspectServer registers the media tools over root with a fake ffprobe that
// answers a stream probe with probeJSON and a packet count with countJSON.
func inspectServer(t *testing.T, probeJSON, countJSON string) (*server.MCPServer, string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	ffprobe := filepath.Join(bin, "ffprobe")
	script := "#!/bin/sh\ncase \"$*\" in\n*-count_packets*) cat <<'EOF'\n" + countJSON + "\nEOF\n;;\n*) cat <<'EOF'\n" + probeJSON + "\nEOF\n;;\nesac\n"
	if err := os.WriteFile(ffprobe, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	cfg.Media.AllowedReadRoots = []string{root}
	cfg.Media.FfprobePath = ffprobe
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	s := server.NewMCPServer("test", "0.0.0")
	RegisterMaintenance(s, cfg, arrservice.NewRegistry(cfg), (*qbit.Client)(nil), st)
	return s, root
}

const inspectProbe = `{"streams":[
 {"index":0,"codec_type":"video","codec_name":"hevc","profile":"Main 10","pix_fmt":"yuv420p10le","width":1920,"height":800,
  "color_transfer":"bt709","color_primaries":"bt709","color_space":"bt709"},
 {"index":1,"codec_type":"audio","codec_name":"eac3","channels":6,"channel_layout":"5.1(side)","bit_rate":"640000","tags":{"language":"jpn"},"disposition":{"default":1}},
 {"index":2,"codec_type":"audio","codec_name":"aac","channels":2,"channel_layout":"stereo","bit_rate":"128000","tags":{"language":"eng","title":"Commentary"}},
 {"index":3,"codec_type":"subtitle","codec_name":"ass","tags":{"language":"spa"}}],
 "format":{"format_name":"matroska,webm","duration":"1440.0","bit_rate":"5000000"}}`

func TestInspectMediaToolReportsTrackDetail(t *testing.T) {
	s, root := inspectServer(t, inspectProbe, `{"streams":[{"index":3,"nb_read_packets":"312"}]}`)
	media := filepath.Join(root, "Show - S01E01.mkv")
	os.WriteFile(media, []byte("x"), 0o644)

	res := callTool(t, s, "inspect_media", map[string]any{"path": media})
	if res.IsError {
		t.Fatal(resultText(t, res))
	}
	var rep struct {
		Resolution  string `json:"resolution"`
		Width       int    `json:"width"`
		Height      int    `json:"height"`
		BitRate     int64  `json:"bit_rate"`
		IsHDR       *bool  `json:"is_hdr"`
		AudioTracks []struct {
			Language   string `json:"language"`
			Layout     string `json:"layout"`
			BitRate    int64  `json:"bit_rate"`
			Default    bool   `json:"default"`
			Commentary bool   `json:"commentary"`
		} `json:"audio_tracks"`
		SubtitleTracks []struct {
			Events       *int64 `json:"events"`
			EventsSource string `json:"events_source"`
		} `json:"subtitle_tracks"`
	}
	decode(t, resultText(t, res), &rep)
	if rep.Resolution != "720p" || rep.Width != 1920 || rep.Height != 800 || rep.BitRate != 5000000 || rep.IsHDR == nil || *rep.IsHDR {
		t.Errorf("report = %s", resultText(t, res))
	}
	if len(rep.AudioTracks) != 2 || rep.AudioTracks[0].Layout != "5.1" || !rep.AudioTracks[0].Default || rep.AudioTracks[1].Layout != "2.0" || !rep.AudioTracks[1].Commentary || rep.AudioTracks[1].BitRate != 128000 {
		t.Errorf("audio = %+v", rep.AudioTracks)
	}
	// The container states no count, and counting is opt-in.
	if len(rep.SubtitleTracks) != 1 || rep.SubtitleTracks[0].Events != nil {
		t.Errorf("subtitles = %s", resultText(t, res))
	}

	res = callTool(t, s, "inspect_media", map[string]any{"path": media, "count_subtitle_events": "true"})
	decode(t, resultText(t, res), &rep)
	if rep.SubtitleTracks[0].Events == nil || *rep.SubtitleTracks[0].Events != 312 || rep.SubtitleTracks[0].EventsSource != "counted" {
		t.Errorf("counted subtitles = %s", resultText(t, res))
	}
}

func TestInspectMediaToolNotesAFailedCount(t *testing.T) {
	s, root := inspectServer(t, inspectProbe, `not json`)
	media := filepath.Join(root, "x.mkv")
	os.WriteFile(media, []byte("x"), 0o644)
	res := callTool(t, s, "inspect_media", map[string]any{"path": media, "count_subtitle_events": "true"})
	text := resultText(t, res)
	if res.IsError || !strings.Contains(text, "subtitle events were not counted") || !strings.Contains(text, `"audio_tracks"`) {
		t.Errorf("failed count = %s", text)
	}
}

// A replace destroys the old file, so it is refused wherever fs_remove would
// refuse to remove it.
func TestFsCopyToolReplaceHeldToWhatRemoveAllows(t *testing.T) {
	s, root := fsManageServer(t, true, false)
	other := filepath.Join(root, "Backups", "other.mkv")
	os.MkdirAll(filepath.Dir(other), 0o755)
	os.WriteFile(other, []byte("other bytes"), 0o644)
	fresh := filepath.Join(root, "Backups", "fresh.mkv")
	os.WriteFile(fresh, []byte("just written"), 0o644)

	for name, tc := range map[string]struct{ dst, content string }{
		"a file Radarr tracks":    {filepath.Join(root, "Movies", "Film (1997)", "Film (1997).mkv"), "data"},
		"a file qBittorrent owns": {filepath.Join(root, "Downloads", "Seeding", "seed.mkv"), "data"},
		"a file changed just now": {fresh, "just written"},
	} {
		t.Run(name, func(t *testing.T) {
			res := callTool(t, s, "fs_copy", map[string]any{"source": other, "destination": tc.dst, "replace_sha256": shaOf(tc.content), "confirm": "true"})
			if !res.IsError || !strings.Contains(resultText(t, res), "cannot be replaced") {
				t.Fatalf("replace of %s = %s", name, resultText(t, res))
			}
			if b, _ := os.ReadFile(tc.dst); string(b) != tc.content {
				t.Errorf("destination = %q", b)
			}
		})
	}

	// When Radarr cannot be read, nothing is guessed.
	s, root = fsManageServer(t, true, true)
	other = filepath.Join(root, "Movies", "Film (1997)", "Stray", "big.mkv")
	dst := filepath.Join(root, "Movies", "Film (1997)", "Stray", "big.en.srt")
	res := callTool(t, s, "fs_copy", map[string]any{"source": other, "destination": dst, "replace_sha256": shaOf("data"), "confirm": "true"})
	if !res.IsError || !strings.Contains(resultText(t, res), "cannot tell") {
		t.Errorf("replace with Radarr down = %s", resultText(t, res))
	}
}
