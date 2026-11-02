package maintenanceui

import (
	"encoding/json"
	"testing"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
)

// The page lists a trimmed snapshot of each job; the opened job's streams and
// outcome come from its full record, where the plan, the source probe and the
// quality ladder are.
func TestOpenedConversionCarriesStreamsAndOutcome(t *testing.T) {
	s, h := testUI(t)
	st := s.engine.Deps().Store
	asMap := func(v any) map[string]any {
		raw, _ := json.Marshal(v)
		out := map[string]any{}
		_ = json.Unmarshal(raw, &out)
		return out
	}
	rep := mediainspect.DetailedReport{
		DurationSec: 1000, SizeBytes: 2_000_000_000,
		Video: []mediainspect.DetailedStream{{Index: 0, Kind: "video", Codec: "h264", Width: 1920, Height: 1080}},
		Audio: []mediainspect.DetailedStream{
			{Index: 1, Kind: "audio", Codec: "ac3", Channels: 2, Language: "eng", BitRate: 192_000},
			{Index: 2, Kind: "audio", Codec: "ac3", Channels: 6, Language: "fra", BitRate: 448_000},
		},
	}
	plan := transcode.Plan{VideoCodec: transcode.VideoCodecLibX265, Tracks: &transcode.TrackSelection{KeepAudio: []int{1}}}
	state := map[string]any{
		"resolved_path":        "/media/movies/Sample/Sample.mkv",
		"original_intact":      true,
		"original":             map[string]any{"size_bytes": 2_000_000_000},
		"candidate_size_bytes": 700_000_000,
		"plan":                 asMap(plan),
		"source_report":        asMap(rep),
		"quality_ladder": map[string]any{"video_codec": "libx265", "preset": "medium", "quality_values": []any{22, 19}, "rung": 0, "outcome": "passed",
			"final": map[string]any{"verdict": "pass", "measured": map[string]any{"vmaf_mean": 94.0}, "limits": map[string]any{"vmaf_mean_minimum": 93.0}}},
	}
	seedOperation(t, st, "conv", "transcode_media", "completed", map[string]any{"path": "/media/movies/Sample/Sample.mkv"}, nil, state)

	w := request(h, "GET", "/api/maintenance/operations?id=conv", "", true)
	var page struct {
		Jobs []map[string]any `json:"jobs"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page.Jobs) != 1 {
		t.Fatal(w.Code, w.Body.String())
	}
	job := page.Jobs[0]
	streams := operationMap(job["streams"])
	audio, _ := streams["audio"].([]any)
	if len(audio) != 2 || operationMap(audio[1])["fate"] != "dropped" {
		t.Fatalf("streams %v", job["streams"])
	}
	outcome := operationMap(job["outcome"])
	if outcome["setting"] != "CRF 22 · medium" || outcome["vmaf_target"] != 93.0 {
		t.Fatalf("outcome %v", job["outcome"])
	}
}
