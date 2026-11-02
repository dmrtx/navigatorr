package maintenanceui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/arrservice"
	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
)

// These go through the operations endpoint: the list is built from a trimmed
// snapshot and the opened job from its full record, so a field must be
// checked where a client reads it.

type operationsAnswer struct {
	Jobs     []map[string]any `json:"jobs"`
	ReadyIDs []string         `json:"ready_ids"`
	Ready    []map[string]any `json:"ready"`
}

func getOperations(t *testing.T, h http.Handler, query string) operationsAnswer {
	t.Helper()
	w := request(h, "GET", "/api/maintenance/operations?"+query, "", true)
	var page operationsAnswer
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	return page
}

func jobByID(t *testing.T, page operationsAnswer, id string) map[string]any {
	t.Helper()
	for _, job := range page.Jobs {
		if job["id"] == id {
			return job
		}
	}
	t.Fatalf("job %s not listed in %d jobs", id, len(page.Jobs))
	return nil
}

func asJSONMap(v any) map[string]any {
	raw, _ := json.Marshal(v)
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	return out
}

func seedFailed(t *testing.T, st *store.Store, id, name string, inputs, state map[string]any, errText string) {
	t.Helper()
	in, _ := json.Marshal(inputs)
	sj, _ := json.Marshal(state)
	ej, _ := json.Marshal(map[string]string{"error": errText})
	if err := st.CreateActionInstance(store.ActionInstance{ID: id, ActionName: name, Status: store.ActionStatusFailed, InputsJSON: string(in), StateJSON: string(sj), ErrorJSON: string(ej)}); err != nil {
		t.Fatal(err)
	}
}

func ladderRung(q int, vmaf float64, bytes int64) map[string]any {
	return map[string]any{"quality": q, "reason_code": "quality_not_met", "candidate_bytes": bytes, "job_id": fmt.Sprintf("job-%d", q),
		"final": map[string]any{"verdict": "fail", "failed_checks": []any{"below_minimum_quality"}, "failures": []any{"VMAF below"},
			"measured": map[string]any{"vmaf_mean": vmaf, "vmaf_worst_window": vmaf - 5, "cambi_mean": 1.0}, "limits": map[string]any{"vmaf_mean_minimum": 93.0, "vmaf_worst_window_minimum": 80.0}}}
}

// A ladder whose every setting missed the target says, on the list and on
// the opened job, the score of the last one against the target and what each
// setting measured.
func TestCauseOfAQualityMissOnListAndDetail(t *testing.T) {
	s, h := testUI(t)
	st := s.engine.Deps().Store
	seedOperation(t, st, "miss", "transcode_media", "completed", map[string]any{"path": "/media/movies/Sample/Sample.mkv", "mode": "size"}, nil, map[string]any{
		"resolved_path": "/media/movies/Sample/Sample.mkv", "skip_transcode": true, "reason_code": "quality_not_met", "original": map[string]any{"size_bytes": 4_000_000_000},
		"quality_ladder": map[string]any{"video_codec": "libx265", "preset": "medium", "quality_values": []any{22, 19, 16}, "rung": 2, "outcome": "quality_not_met",
			"message": "long text that stays in the history",
			"tried":   []any{ladderRung(22, 89.5, 1_000_000_000), ladderRung(19, 90.8, 1_300_000_000), ladderRung(16, 91.2, 1_700_000_000)}},
	})
	for _, query := range []string{"group=workflow", "id=miss"} {
		job := jobByID(t, getOperations(t, h, query), "miss")
		cause := operationMap(job["cause"])
		if cause["kind"] != "quality_below_target" || cause["summary"] != "Quality 91.2 VMAF at CRF 16 · medium, the last of 3 settings tried, below the 93.0 target" || cause["measured"] != 91.2 || cause["limit"] != 93.0 || cause["unit"] != "vmaf" {
			t.Fatalf("%s: cause %v", query, job["cause"])
		}
		attempts, _ := cause["attempts"].([]any)
		if len(attempts) != 3 {
			t.Fatalf("%s: attempts %v", query, cause["attempts"])
		}
		first := operationMap(attempts[0])
		if first["setting"] != "CRF 22 · medium" || first["vmaf_mean"] != 89.5 || first["vmaf_worst"] != 84.5 || first["bytes"] != 1e9 || first["passed"] != false {
			t.Fatalf("%s: first attempt %v", query, first)
		}
		if query == "id=miss" {
			if e := operationMap(job["explanation"]); !strings.HasPrefix(operationString(e["happened"]), "Quality 91.2 VMAF") {
				t.Fatalf("explanation %v", job["explanation"])
			}
		}
	}
}

// A file that missed its target size because of its audio says so on the
// opened job, where the probe and the plan are; the list says the size miss.
func TestCauseBlamesTheAudioForASizeMiss(t *testing.T) {
	s, h := testUI(t)
	st := s.engine.Deps().Store
	rep := mediainspect.DetailedReport{
		DurationSec: 1000, SizeBytes: 4_000_000_000,
		Video: []mediainspect.DetailedStream{{Index: 0, Kind: "video", Codec: "h264", Width: 1920, Height: 1080}},
		Audio: []mediainspect.DetailedStream{{Index: 1, Kind: "audio", Codec: "truehd", Channels: 8, Language: "eng", BitRate: 16_000_000}},
	}
	plan := transcode.Plan{VideoCodec: transcode.VideoCodecLibX265, AudioMode: "copy"}
	seedOperation(t, st, "big", "transcode_media", "completed", map[string]any{"path": "/media/movies/Sample/Sample.mkv", "mode": "size"}, nil, map[string]any{
		"resolved_path": "/media/movies/Sample/Sample.mkv", "skip_transcode": true, "reason_code": "size_budget_exceeded",
		"original": map[string]any{"size_bytes": 4_000_000_000}, "candidate_size_bytes": 3_000_000_000,
		"size_budget":   map[string]any{"budget_bytes": 2_500_000_000, "video_bitrate_kbps": 4000, "candidate_bytes": 3_000_000_000},
		"plan":          asJSONMap(plan),
		"source_report": asJSONMap(rep),
	})
	list := operationMap(jobByID(t, getOperations(t, h, "group=workflow"), "big")["cause"])
	if list["kind"] != "over_size_target" || list["measured"] != 3e9 || list["limit"] != 2.5e9 || list["unit"] != "bytes" {
		t.Fatalf("list cause %v", list)
	}
	detail := jobByID(t, getOperations(t, h, "id=big"), "big")
	cause := operationMap(detail["cause"])
	if cause["kind"] != "audio_too_big" || cause["stream"] != "audio" || !strings.HasPrefix(operationString(cause["summary"]), "Audio is 1.9 GB of the 2.8 GB result (67%); the video alone") {
		t.Fatalf("detail cause %v", cause)
	}
	attempts, _ := cause["attempts"].([]any)
	if len(attempts) != 1 || operationMap(attempts[0])["setting"] != "4000 kbps" {
		t.Fatalf("budget attempt %v", cause["attempts"])
	}
	if e := operationMap(detail["explanation"]); operationString(e["happened"]) != operationString(cause["summary"])+"." {
		t.Fatalf("explanation %v vs cause %v", detail["explanation"], cause["summary"])
	}
}

// A failed encode and a failed replacement say what stopped them.
func TestCauseOfFailuresAndFailedReplacements(t *testing.T) {
	s, h := testUI(t)
	st := s.engine.Deps().Store
	seedFailed(t, st, "changed", "transcode_media", map[string]any{"path": "/media/a.mkv"}, map[string]any{"resolved_path": "/media/a.mkv"}, "transcode failed (source_changed): size moved")
	seedOperation(t, st, "ready", "transcode_media", "completed", map[string]any{"path": "/media/b.mkv"}, map[string]any{"original_intact": true}, map[string]any{"resolved_path": "/media/b.mkv", "original": map[string]any{"size_bytes": 1000}, "candidate_size_bytes": 400})
	seedFailed(t, st, "promote", "promote_transcode_candidate", map[string]any{"transcode_action_id": "ready"}, nil, "transcode failed (worker_unreachable): dial timeout")
	page := getOperations(t, h, "status=all&limit=100")
	if c := operationMap(jobByID(t, page, "changed")["cause"]); c["kind"] != "file_changed" || c["summary"] != "The file changed while the job ran" {
		t.Fatalf("failed cause %v", c)
	}
	detail := jobByID(t, getOperations(t, h, "id=ready"), "ready")
	if detail["conversion_status"] != "replace_failed" {
		t.Fatalf("status %v", detail["conversion_status"])
	}
	if c := operationMap(detail["cause"]); c["kind"] != "worker_unreachable" || !strings.HasPrefix(operationString(c["summary"]), "Replacement failed: ") {
		t.Fatalf("replacement cause %v", c)
	}
}

// The opened job lists each subtitle with its fate and the totals of each
// kind of stream.
func TestStreamsCarrySubtitleTracksAndTotals(t *testing.T) {
	s, h := testUI(t)
	st := s.engine.Deps().Store
	rep := mediainspect.DetailedReport{
		DurationSec: 1000, SizeBytes: 2_000_000_000,
		Video: []mediainspect.DetailedStream{{Index: 0, Kind: "video", Codec: "h264", Width: 1920, Height: 1080}},
		Audio: []mediainspect.DetailedStream{
			{Index: 1, Kind: "audio", Codec: "ac3", Channels: 2, Language: "eng", BitRate: 192_000},
			{Index: 2, Kind: "audio", Codec: "ac3", Channels: 6, Language: "fra", BitRate: 448_000},
		},
		Subtitles: []mediainspect.DetailedStream{
			{Index: 3, Kind: "subtitle", Codec: "subrip", Language: "eng", Title: "Full", BitRate: 800},
			{Index: 4, Kind: "subtitle", Codec: "hdmv_pgs_subtitle", Language: "spa", BitRate: 40_000},
		},
	}
	plan := transcode.Plan{VideoCodec: transcode.VideoCodecLibX265, Tracks: &transcode.TrackSelection{KeepAudio: []int{1}, KeepSubtitles: []int{3}}}
	seedOperation(t, st, "conv", "transcode_media", "completed", map[string]any{"path": "/media/movies/Sample/Sample.mkv"}, nil, map[string]any{
		"resolved_path": "/media/movies/Sample/Sample.mkv", "original_intact": true, "original": map[string]any{"size_bytes": 2_000_000_000}, "candidate_size_bytes": 700_000_000,
		"plan": asJSONMap(plan), "source_report": asJSONMap(rep),
	})
	streams := operationMap(jobByID(t, getOperations(t, h, "id=conv"), "conv")["streams"])
	subs := operationMap(streams["subtitles"])
	tracks, _ := subs["tracks"].([]any)
	if len(tracks) != 2 {
		t.Fatalf("subtitles %v", subs)
	}
	kept, dropped := operationMap(tracks[0]), operationMap(tracks[1])
	if kept["fate"] != "kept" || kept["codec"] != "subrip" || kept["title"] != "Full" || operationString(kept["language"]) == "" || kept["index"] != 3.0 {
		t.Fatalf("kept subtitle %v", kept)
	}
	if dropped["fate"] != "dropped" || operationNumber(dropped["bytes"]) <= 0 {
		t.Fatalf("dropped subtitle %v", dropped)
	}
	totals := operationMap(streams["totals"])
	for _, key := range []string{"video_before", "video_after", "audio_before", "audio_after", "subtitles_before", "subtitles_after", "removed_bytes"} {
		if _, ok := totals[key]; !ok {
			t.Fatalf("totals miss %s: %v", key, totals)
		}
	}
	audio, _ := streams["audio"].([]any)
	removed := operationNumber(operationMap(audio[1])["before_bytes"]) + operationNumber(dropped["bytes"])
	if totals["removed_bytes"] != removed || totals["subtitles_after"] != kept["bytes"] || operationNumber(totals["audio_before"]) <= operationNumber(totals["audio_after"]) {
		t.Fatalf("totals %v, removed %v", totals, removed)
	}
}

// Every run of a file is an attempt of one conversion; an older one names
// the latest. A file of a batch counts its runs in that batch and its own
// runs, not another batch's.
func TestAttemptsListEveryRunOfTheFile(t *testing.T) {
	s, h := testUI(t)
	st := s.engine.Deps().Store
	path := "/media/shows/Sample/S01E01.mkv"
	seedOperation(t, st, "first", "transcode_media", "completed", map[string]any{"path": path, "mode": "size"}, nil, map[string]any{"resolved_path": path, "skip_transcode": true, "reason_code": "size_growth", "original": map[string]any{"size_bytes": 1000}, "candidate_size_bytes": 1200})
	seedOperation(t, st, "batch-a", "transcode_batch", "completed", map[string]any{"paths": []any{path}}, nil, nil)
	seedOperation(t, st, "child-a", "transcode_media", "completed", map[string]any{"path": path, "parent_action_id": "batch-a"}, map[string]any{"original_intact": true}, map[string]any{"resolved_path": path, "original": map[string]any{"size_bytes": 1000}, "candidate_size_bytes": 400})
	seedOperation(t, st, "batch-b", "transcode_batch", "completed", map[string]any{"paths": []any{path}}, nil, nil)
	seedOperation(t, st, "child-b", "transcode_media", "completed", map[string]any{"path": path, "parent_action_id": "batch-b"}, nil, map[string]any{"resolved_path": path, "skip_transcode": true, "reason_code": "compact_not_needed"})
	seedOperation(t, st, "second", "transcode_media", "completed", map[string]any{"path": path, "mode": "size"}, nil, map[string]any{"resolved_path": path, "skip_transcode": true, "reason_code": "compact_not_needed"})

	ids := func(job map[string]any) []string {
		var out []string
		for _, a := range job["attempts"].([]any) {
			out = append(out, operationString(operationMap(a)["id"]))
		}
		return out
	}
	first := jobByID(t, getOperations(t, h, "id=first"), "first")
	if got := strings.Join(ids(first), ","); got != "first,child-a,child-b,second" {
		t.Fatalf("standalone attempts %s", got)
	}
	attempts := first["attempts"].([]any)
	one := operationMap(attempts[0])
	if one["current"] != true || operationMap(attempts[3])["current"] != false || one["conversion_status"] != "no_result" || operationMap(one["cause"])["kind"] != "not_smaller" || one["created_at"] == "" || operationNumber(one["number"]) <= 0 {
		t.Fatalf("first attempt %v", one)
	}
	if by := operationMap(first["superseded_by"]); by["id"] != "second" || operationNumber(by["number"]) <= 0 {
		t.Fatalf("superseded_by %v", first["superseded_by"])
	}
	child := jobByID(t, getOperations(t, h, "id=child-a"), "child-a")
	if got := strings.Join(ids(child), ","); got != "first,child-a,second" {
		t.Fatalf("batch file attempts %s", got)
	}
	if operationMap(child["attempts"].([]any)[1])["candidate_bytes"] != 400.0 {
		t.Fatalf("candidate bytes %v", child["attempts"])
	}
	latest := jobByID(t, getOperations(t, h, "id=second"), "second")
	if _, ok := latest["superseded_by"]; ok {
		t.Fatalf("the latest attempt is superseded: %v", latest["superseded_by"])
	}
}

// The replacement of a file of a batch opens that file's conversion.
func TestBatchFileReplacementNamesItsConversion(t *testing.T) {
	s, h := testUI(t)
	st := s.engine.Deps().Store
	path := "/media/shows/Sample/S01E02.mkv"
	seedOperation(t, st, "batch", "transcode_batch", "completed", map[string]any{"paths": []any{path}}, nil, nil)
	seedOperation(t, st, "child", "transcode_media", "completed", map[string]any{"path": path, "parent_action_id": "batch"}, map[string]any{"original_intact": true}, map[string]any{"resolved_path": path, "original": map[string]any{"size_bytes": 1000}, "candidate_size_bytes": 400})
	seedOperation(t, st, "promote", "promote_transcode_candidate", "running", map[string]any{"transcode_action_id": "child", "batch_promote_parent_id": "batch"}, nil, nil)
	job := jobByID(t, getOperations(t, h, "id=promote"), "promote")
	if job["conversion_id"] != "child" || job["workflow_id"] != "batch" {
		t.Fatalf("conversion %v, workflow %v", job["conversion_id"], job["workflow_id"])
	}
	// The batch still groups its file and the file's replacement.
	if row := jobByID(t, getOperations(t, h, "group=workflow&status=all"), "batch"); row["workflow_id"] != "batch" {
		t.Fatalf("batch row %v", row["workflow_id"])
	}
}

// The Stats list sends its largest rows and the ids of every ready file; an
// opened batch names every ready file across its pages.
func TestReadyIDsCoverEveryReadyFile(t *testing.T) {
	s, h := testUI(t)
	st := s.engine.Deps().Store
	const files = savingsReadyListed + 3
	for i := range files {
		path := fmt.Sprintf("/media/movies/Sample %d/Sample.mkv", i)
		seedOperation(t, st, fmt.Sprintf("ready-%03d", i), "transcode_media", "completed", map[string]any{"path": path}, map[string]any{"original_intact": true}, map[string]any{"resolved_path": path, "original": map[string]any{"size_bytes": 10_000}, "candidate_size_bytes": 1000 + i})
	}
	page := getOperations(t, h, "limit=1")
	if len(page.Ready) != savingsReadyListed || len(page.ReadyIDs) != files {
		t.Fatalf("ready rows %d, ids %d", len(page.Ready), len(page.ReadyIDs))
	}
	if page.ReadyIDs[0] != "ready-000" {
		t.Fatalf("largest saving first: %v", page.ReadyIDs[:3])
	}

	seedOperation(t, st, "batch", "transcode_batch", "completed", map[string]any{"paths": []any{}}, nil, nil)
	for i, status := range []string{"done", "done2", "skipped", "replaced"} {
		path := fmt.Sprintf("/media/shows/Sample/S01E%02d.mkv", i+1)
		outputs := map[string]any{"original_intact": true}
		state := map[string]any{"resolved_path": path, "original": map[string]any{"size_bytes": 1000}, "candidate_size_bytes": 400}
		if status == "skipped" {
			state["skip_transcode"] = true
		}
		seedOperation(t, st, "child-"+status, "transcode_media", "completed", map[string]any{"path": path, "parent_action_id": "batch"}, outputs, state)
		if err := st.CreateTranscodeBatchItem(store.TranscodeBatchItem{BatchID: "batch", ItemKey: status, FilePath: path, DisplayLabel: status, ChildActionID: "child-" + status, Status: "completed", Decision: "transcode", CandidatePath: path + ".new"}); err != nil {
			t.Fatal(err)
		}
	}
	seedOperation(t, st, "promote", "promote_transcode_candidate", "running", map[string]any{"transcode_action_id": "child-replaced"}, nil, nil)
	batch := jobByID(t, getOperations(t, h, "id=batch"), "batch")
	got, _ := json.Marshal(batch["ready_item_ids"])
	if string(got) != `["child-done","child-done2"]` {
		t.Fatalf("ready items %s", got)
	}
}

// The review queue searches title, year, folder and note without regard to
// case or accents, sorts, pages, and says how many entries match in all.
func TestReviewQueueSearchesSortsAndPages(t *testing.T) {
	s, h := testUI(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]map[string]any{
			{"id": 1, "title": "Café Story", "year": 2001, "path": "/media/movies/Cafe Story (2001)"},
			{"id": 2, "title": "Zebra Days", "year": 1999, "path": "/media/movies/Zebra Days (1999)"},
			{"id": 3, "title": "Another Film", "year": 2015, "path": "/media/movies/Another Film (2015)"},
		})
	}))
	defer upstream.Close()
	s.registry = arrservice.NewRegistry(&config.Config{Services: map[string]config.ServiceConfig{"radarr": {URL: upstream.URL}}})
	st := s.engine.Deps().Store
	add := func(id, title, note string, priority int, size int64) {
		if _, err := st.AddItem(store.MaintenanceItem{MediaType: "movie", Service: "radarr", MediaID: id, Title: title, IssueType: "oversized", Priority: priority, Notes: note, CurrentSize: &size}); err != nil {
			t.Fatal(err)
		}
	}
	add("1", "Café Story", "Remux grande", 5, 300)
	add("2", "Zebra Days", "pista en ESPAÑOL", 9, 100)
	add("3", "Another Film", "", 1, 900)
	read := func(query string) queuePage {
		t.Helper()
		w := request(h, "GET", "/api/maintenance/queue?"+query, "", true)
		var page queuePage
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil {
			t.Fatal(w.Code, w.Body.String())
		}
		return page
	}
	titles := func(p queuePage) string {
		var out []string
		for _, it := range p.Items {
			out = append(out, it.Title)
		}
		return strings.Join(out, "|")
	}
	for query, want := range map[string]string{
		"q=CAFE":         "Café Story",
		"q=espanol":      "Zebra Days",
		"q=1999":         "Zebra Days",
		"q=another+2015": "Another Film",
		"q=movies/zebra": "Zebra Days",
		"q=nothing":      "",
	} {
		if got := titles(read(query)); got != want {
			t.Fatalf("%s: %q, want %q", query, got, want)
		}
	}
	for order, want := range map[string]string{
		"":         "Zebra Days|Café Story|Another Film",
		"priority": "Zebra Days|Café Story|Another Film",
		"size":     "Another Film|Café Story|Zebra Days",
		"newest":   "Another Film|Zebra Days|Café Story",
		"title":    "Another Film|Café Story|Zebra Days",
	} {
		if got := titles(read("sort=" + order)); got != want {
			t.Fatalf("sort %q: %q, want %q", order, got, want)
		}
	}
	page := read("sort=title&offset=1&limit=1")
	if titles(page) != "Café Story" || page.Total != 3 || !page.Truncated {
		t.Fatalf("page %+v", page)
	}
	if page := read("q=a&offset=2&limit=5"); page.Total != 3 || len(page.Items) != 1 || page.Truncated {
		t.Fatalf("last page %+v", page)
	}
	for _, bad := range []string{"sort=random", "limit=1001", "limit=0", "offset=-1"} {
		if w := request(h, "GET", "/api/maintenance/queue?"+bad, "", true); w.Code != 400 {
			t.Fatalf("%s: %d", bad, w.Code)
		}
	}
	// The old path answers the same.
	if w := request(h, "GET", "/api/maintenance/maintenance-queue?q=cafe", "", true); w.Code != 200 || !strings.Contains(w.Body.String(), `"total":1`) {
		t.Fatal(w.Code, w.Body.String())
	}
}

// The goal form's preview: source-tracks with the settings being chosen
// returns each stream's before and an estimated after, the video included.
func TestSourceTracksEstimatesTheChosenGoal(t *testing.T) {
	audio := []mediainspect.DetailedStream{
		{Index: 1, Kind: "audio", Codec: "flac", Language: "eng", Channels: 2, ChannelLayout: "stereo", BitRate: 900_000},
		{Index: 2, Kind: "audio", Codec: "aac", Language: "spa", Channels: 2, ChannelLayout: "stereo", BitRate: 128_000},
	}
	subs := []mediainspect.DetailedStream{{Index: 3, Kind: "subtitle", Codec: "subrip", Language: "eng", Tags: map[string]string{"NUMBER_OF_BYTES": "40000"}}}
	_, video, s := tracksFixture(t, audio, subs)
	s.probe = func(_ context.Context, _, path string) (mediainspect.DetailedReport, error) {
		return mediainspect.DetailedReport{Path: path, Probed: true, DurationSec: 100, SizeBytes: 100_000_000,
			Video: []mediainspect.DetailedStream{{Index: 0, Kind: "video", Codec: "h264", Width: 3840, Height: 2160}}, Audio: audio, Subtitles: subs}, nil
	}
	h := s.Handler()
	estimate := func(query string) (int, map[string]any) {
		t.Helper()
		w := request(h, "GET", "/api/maintenance/source-tracks?path="+url.QueryEscape(video)+"&"+query, "", true)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, operationMap(out["estimate"])
	}
	if code, e := estimate(""); code != 200 || e != nil {
		t.Fatalf("no mode, no estimate: %d %v", code, e)
	}
	code, e := estimate("mode=size&keep_audio=1&keep_subtitles=")
	streams := operationMap(e["streams"])
	totals := operationMap(streams["totals"])
	tracks, _ := streams["audio"].([]any)
	if code != 200 || e["estimate"] != true || e["video_basis"] != "heuristic" || len(tracks) != 2 {
		t.Fatal(code, e)
	}
	flac, spa := operationMap(tracks[0]), operationMap(tracks[1])
	if flac["fate"] != "converted" || flac["before_kbps"] != 900.0 || flac["after_kbps"] != 192.0 || flac["after_bytes"] != 2_400_000.0 || flac["before_bytes"] != 11_250_000.0 || spa["fate"] != "dropped" {
		t.Fatalf("audio %v %v", flac, spa)
	}
	if operationNumber(totals["video_after"]) <= 0 || operationNumber(totals["video_after"]) >= operationNumber(totals["video_before"]) || totals["subtitles_after"] != 0.0 || totals["audio_after"] != 2_400_000.0 {
		t.Fatalf("totals %v", totals)
	}
	if operationNumber(e["total_after"]) != operationNumber(totals["video_after"])+2_400_000 {
		t.Fatalf("total %v", e)
	}
	// Audio only copies the video; a target size gives the video the rest.
	_, only := estimate("mode=size&audio_only=true")
	if v := operationMap(operationMap(only["streams"])["totals"]); only["video_basis"] != "copied" || v["video_after"] != v["video_before"] {
		t.Fatalf("audio only %v", only)
	}
	_, budget := estimate("mode=size&size_budget_percent=50&audio_mode=copy")
	if v := operationMap(operationMap(budget["streams"])["totals"]); budget["video_basis"] != "size_budget" || budget["total_after"] != 50_000_000.0 || budget["audio_mode"] != "copy" || operationNumber(v["video_after"]) != 50_000_000-12_850_000-40_000 {
		t.Fatalf("budget %v", budget)
	}
	_, compact := estimate("mode=compact&max_height=1080")
	_, size := estimate("mode=size")
	if operationNumber(operationMap(operationMap(compact["streams"])["totals"])["video_after"]) >= operationNumber(operationMap(operationMap(size["streams"])["totals"])["video_after"]) {
		t.Fatalf("1080p estimate not below 2160p: %v vs %v", compact, size)
	}
	for _, bad := range []string{"mode=fast", "mode=size&audio_mode=loud", "mode=size&keep_audio=x", "mode=size&size_budget_percent=150", "mode=compact&max_height=-1"} {
		if code, _ := estimate(bad); code != 400 {
			t.Fatalf("%s: %d", bad, code)
		}
	}
}

// Every cause and explanation the server sends is one line of at most
// summaryLimit characters, and a quality or size miss states its numbers.
// Each case goes through the endpoint: the list (snapshot) and the opened job
// (full record).
func TestEveryCauseIsOneLineWithItsNumbers(t *testing.T) {
	s, h := testUI(t)
	st := s.engine.Deps().Store
	heavy := mediainspect.DetailedReport{DurationSec: 1000, SizeBytes: 4_000_000_000,
		Video: []mediainspect.DetailedStream{{Index: 0, Kind: "video", Codec: "h264", Width: 1920, Height: 1080}},
		Audio: []mediainspect.DetailedStream{{Index: 1, Kind: "audio", Codec: "truehd", Channels: 8, Language: "eng", BitRate: 16_000_000}}}
	light := heavy
	light.Audio = []mediainspect.DetailedStream{{Index: 1, Kind: "audio", Codec: "aac", Channels: 2, Language: "eng", BitRate: 128_000}}
	copyPlan := asJSONMap(transcode.Plan{VideoCodec: transcode.VideoCodecLibX265, AudioMode: "copy"})
	base := func(reason string, extra map[string]any) map[string]any {
		state := map[string]any{"skip_transcode": true, "reason_code": reason, "original": map[string]any{"size_bytes": 4_000_000_000}, "mode_policy": map[string]any{"min_savings_percent": 15}}
		for k, v := range extra {
			state[k] = v
		}
		return state
	}
	ladder := func(checks []any, measured, limits map[string]any) map[string]any {
		return map[string]any{"video_codec": "hevc_videotoolbox", "quality_values": []any{60, 66, 72}, "rung": 2, "outcome": "quality_not_met",
			"tried": []any{map[string]any{"quality": 72, "reason_code": "quality_not_met", "final": map[string]any{"verdict": "fail", "failed_checks": checks, "measured": measured, "limits": limits}}}}
	}
	numbered := map[string]bool{"quality_below_target": true, "worst_window_below_floor": true, "not_smaller": true, "below_min_savings": true, "over_size_target": true, "audio_too_big": true, "video_too_big": true}
	results := map[string]struct {
		state map[string]any
		kind  string
	}{
		"saving":        {base("insufficient_savings", map[string]any{"candidate_size_bytes": 3_600_000_000}), "below_min_savings"},
		"estimate":      {base("insufficient_savings", map[string]any{"benchmark_decision": map[string]any{"winner": map[string]any{"estimated_bytes": 3_700_000_000}}}), "below_min_savings"},
		"growth":        {base("size_growth", map[string]any{"candidate_size_bytes": 4_100_000_000}), "not_smaller"},
		"budget":        {base("size_budget_exceeded", map[string]any{"candidate_size_bytes": 3_000_000_000, "size_budget": map[string]any{"budget_bytes": 2_500_000_000}}), "over_size_target"},
		"audio blame":   {base("size_budget_exceeded", map[string]any{"candidate_size_bytes": 3_000_000_000, "size_budget": map[string]any{"budget_bytes": 2_500_000_000}, "plan": copyPlan, "source_report": asJSONMap(heavy)}), "audio_too_big"},
		"video blame":   {base("size_budget_exceeded", map[string]any{"candidate_size_bytes": 3_000_000_000, "size_budget": map[string]any{"budget_bytes": 2_500_000_000}, "plan": copyPlan, "source_report": asJSONMap(light)}), "video_too_big"},
		"growth blame":  {base("size_growth", map[string]any{"candidate_size_bytes": 4_100_000_000, "plan": copyPlan, "source_report": asJSONMap(light)}), "not_smaller"},
		"mean":          {base("quality_not_met", map[string]any{"quality_ladder": ladder([]any{"below_minimum_quality"}, map[string]any{"vmaf_mean": 91.2}, map[string]any{"vmaf_mean_minimum": 93.0})}), "quality_below_target"},
		"worst":         {base("quality_not_met", map[string]any{"quality_ladder": ladder([]any{"quality_vmaf_worst_window_below_minimum"}, map[string]any{"vmaf_mean": 95.1, "vmaf_worst_window": 78.4}, map[string]any{"vmaf_mean_minimum": 93.0, "vmaf_worst_window_minimum": 80.0, "worst_window_seconds": 10})}), "worst_window_below_floor"},
		"p5":            {base("quality_not_met", map[string]any{"quality_ladder": ladder([]any{"quality_vmaf_p5_below_minimum"}, map[string]any{"vmaf_p5": 82.0}, map[string]any{"vmaf_p5_minimum": 84.0})}), "quality_below_target"},
		"budget vmaf":   {base("quality_not_met", map[string]any{"size_budget": map[string]any{"video_bitrate_kbps": 3500, "quality_target": 93.0, "final": map[string]any{"verdict": "fail", "measured": map[string]any{"vmaf_mean": 90.4}}}}), "quality_below_target"},
		"legacy vmaf":   {base("no_suitable_candidate", map[string]any{"quality_evidence": map[string]any{"vmaf": map[string]any{"mean": 90.1}, "reason_codes": []any{"below_minimum_quality"}}}), "quality_below_target"},
		"audio nothing": {base("audio_only_nothing_to_convert", nil), "nothing_to_do"},
		"compact":       {base("compact_not_needed", nil), "nothing_to_do"},
		"hdr":           {base("hdr_requires_compact", nil), "other"},
		"skipped":       {base("some_new_reason", nil), "nothing_to_do"},
	}
	long := strings.Repeat("a very long worker error ", 20) + "\nsecond line"
	failures := map[string]struct{ text, kind string }{
		"saving":    {"benchmark winner predicts only 8.2% savings, below required minimum 15%", "below_min_savings"},
		"rejected":  {"transcode candidate rejected by user decision", "other"},
		"damaged":   {"ffmpeg_input_corrupt: moov atom not found", "file_damaged"},
		"tracks":    {"choosing the tracks to keep: stream 3 which is not in the file", "tracks_mismatch"},
		"cancelled": {"transcode failed (cancelled): stopped", "cancelled"},
		"changed":   {"transcode failed (source_changed): size moved", "file_changed"},
		"missing":   {"transcode failed (source_unreachable): no such file", "file_missing"},
		"worker":    {"transcode failed (worker_unreachable): dial timeout", "worker_unreachable"},
		"killed":    {"transcode failed (runner_killed): signal 9", "encoder_stopped"},
		"busy":      {"transcode failed (encoder_temporarily_unavailable): busy", "encoder_stopped"},
		"measure":   {"transcode failed (benchmark_evidence_incomplete): no vmaf", "check_failed"},
		"invalid":   {"transcode failed (candidate_invalid): empty", "check_failed"},
		"full":      {"transcode failed (storage_full): no space", "other"},
		"long":      {long, "other"},
	}
	check := func(name, what, line string, wantNumber bool) {
		t.Helper()
		if line == "" || strings.Contains(line, "\n") || len([]rune(line)) > summaryLimit {
			t.Errorf("%s: %s is not one line of at most %d characters: %q", name, what, summaryLimit, line)
		}
		if wantNumber && !strings.ContainsAny(line, "0123456789") {
			t.Errorf("%s: %s has no number: %q", name, what, line)
		}
	}
	for name, tc := range results {
		id := "result-" + strings.ReplaceAll(name, " ", "-")
		seedOperation(t, st, id, "transcode_media", "completed", map[string]any{"path": "/media/" + id + ".mkv", "mode": "size"}, nil, tc.state)
		for _, query := range []string{"group=workflow&limit=100", "id=" + id} {
			job := jobByID(t, getOperations(t, h, query), id)
			cause := operationMap(job["cause"])
			// The list has no probe or plan to blame a stream with; the
			// opened job does.
			if strings.HasPrefix(query, "id=") && cause["kind"] != tc.kind {
				t.Errorf("%s: kind %v, want %s (%v)", name, cause["kind"], tc.kind, cause["summary"])
			}
			check(name, "cause.summary", operationString(cause["summary"]), numbered[operationString(cause["kind"])])
			if e := operationMap(job["explanation"]); e != nil {
				check(name, "explanation.happened", operationString(e["happened"]), numbered[operationString(cause["kind"])])
			}
		}
	}
	for name, tc := range failures {
		id := "failed-" + name
		seedFailed(t, st, id, "transcode_media", map[string]any{"path": "/media/" + id + ".mkv"}, nil, tc.text)
		job := jobByID(t, getOperations(t, h, "id="+id), id)
		cause := operationMap(job["cause"])
		if cause["kind"] != tc.kind {
			t.Errorf("%s: kind %v, want %s", name, cause["kind"], tc.kind)
		}
		check(name, "cause.summary", operationString(cause["summary"]), numbered[tc.kind])
		if e := operationMap(job["explanation"]); e != nil {
			check(name, "explanation.happened", operationString(e["happened"]), false)
		}
	}
}

// A batch's file rows carry their attempt's cause too.
func TestBatchFileRowsCarryTheirCause(t *testing.T) {
	s, h := testUI(t)
	st := s.engine.Deps().Store
	path := "/media/shows/Sample/S01E03.mkv"
	seedOperation(t, st, "batch", "transcode_batch", "completed", map[string]any{"paths": []any{path}}, nil, nil)
	seedOperation(t, st, "child", "transcode_media", "completed", map[string]any{"path": path, "parent_action_id": "batch"}, nil, map[string]any{
		"resolved_path": path, "skip_transcode": true, "reason_code": "size_growth", "original": map[string]any{"size_bytes": 1_000_000_000}, "candidate_size_bytes": 1_200_000_000})
	if err := st.CreateTranscodeBatchItem(store.TranscodeBatchItem{BatchID: "batch", ItemKey: "e3", FilePath: path, DisplayLabel: "e3", ChildActionID: "child", Status: "completed", Decision: "transcode"}); err != nil {
		t.Fatal(err)
	}
	w := request(h, "GET", "/api/maintenance/batch-items?id=batch", "", true)
	var out struct {
		Items []map[string]any `json:"items"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil || len(out.Items) != 1 {
		t.Fatal(w.Code, w.Body.String())
	}
	if c := operationMap(out.Items[0]["cause"]); c["kind"] != "not_smaller" || c["summary"] != "New file 1.1 GB vs original 954 MB: not smaller" {
		t.Fatalf("cause %v", out.Items[0]["cause"])
	}
}

// A setting whose mean met the target but failed another check names that
// check, not the mean, on the list, the opened job and its explanation.
func TestCauseNamesTheCheckThatFailedNotAMeanThatPassed(t *testing.T) {
	s, h := testUI(t)
	st := s.engine.Deps().Store
	rung := func(failures []any) map[string]any {
		final := map[string]any{"verdict": "fail", "failed_checks": []any{"quality_cambi_mean_above_maximum"},
			"measured": map[string]any{"vmaf_mean": 95.0, "vmaf_p5": 90.0, "vmaf_worst_window": 88.0}, "limits": map[string]any{"vmaf_mean_minimum": 93.0, "vmaf_p5_minimum": 85.0, "vmaf_worst_window_minimum": 80.0}}
		if failures != nil {
			final["failures"] = failures
		}
		return map[string]any{"quality": 18, "reason_code": "quality_not_met", "candidate_bytes": 1_000_000_000, "final": final}
	}
	for id, tc := range map[string]struct {
		failures []any
		want     string
	}{
		"described": {[]any{"CAMBI mean 6.2 is above the maximum 5.0"}, "CAMBI mean 6.2 is above the maximum 5.0 at CRF 18 · medium"},
		"codes":     {nil, "Failed the cambi mean above maximum check at CRF 18 · medium (quality 95.0 VMAF met the 93.0 target)"},
	} {
		seedOperation(t, st, id, "transcode_media", "completed", map[string]any{"path": "/media/movies/" + id + ".mkv", "mode": "quality"}, nil, map[string]any{
			"resolved_path": "/media/movies/" + id + ".mkv", "skip_transcode": true, "reason_code": "quality_not_met", "original": map[string]any{"size_bytes": 4_000_000_000},
			"quality_ladder": map[string]any{"video_codec": "libx265", "preset": "medium", "quality_values": []any{18}, "rung": 0, "outcome": "quality_not_met", "tried": []any{rung(tc.failures)}},
		})
		for _, query := range []string{"group=workflow", "id=" + id} {
			job := jobByID(t, getOperations(t, h, query), id)
			c := operationMap(job["cause"])
			if c["summary"] != tc.want || strings.Contains(operationString(c["summary"]), "below the 93.0") {
				t.Fatalf("%s %s: cause %v", id, query, c)
			}
			if query != "group=workflow" {
				if happened := operationString(operationMap(job["explanation"])["happened"]); happened != tc.want+"." {
					t.Fatalf("%s: explanation %q", id, happened)
				}
			}
		}
	}
}

// ready_ids holds only files Replace all may start: not one whose
// replacement is running or failed.
func TestReadyIDsLeaveOutFilesBeingReplaced(t *testing.T) {
	s, h := testUI(t)
	st := s.engine.Deps().Store
	for _, id := range []string{"free", "replacing", "failed"} {
		path := "/media/movies/" + id + ".mkv"
		seedOperation(t, st, id, "transcode_media", "completed", map[string]any{"path": path}, map[string]any{"original_intact": true}, map[string]any{"resolved_path": path, "original": map[string]any{"size_bytes": 10_000}, "candidate_size_bytes": 4000})
	}
	seedOperation(t, st, "promote-running", "promote_transcode_candidate", "running", map[string]any{"transcode_action_id": "replacing"}, nil, nil)
	seedFailed(t, st, "promote-failed", "promote_transcode_candidate", map[string]any{"transcode_action_id": "failed"}, nil, "transcode failed (worker_unreachable): dial timeout")
	page := getOperations(t, h, "status=all&limit=100")
	if got, _ := json.Marshal(page.ReadyIDs); string(got) != `["free"]` {
		t.Fatalf("ready ids %s", got)
	}
}

// One list of indexes in the estimate leaves every track of the other kind
// in place when the languages select nothing.
func TestSourceTracksEstimateKeepsTheKindNotListed(t *testing.T) {
	audio := []mediainspect.DetailedStream{
		{Index: 1, Kind: "audio", Codec: "aac", Language: "eng", Channels: 2, BitRate: 128_000},
		{Index: 2, Kind: "audio", Codec: "aac", Language: "spa", Channels: 2, BitRate: 128_000},
	}
	subs := []mediainspect.DetailedStream{{Index: 3, Kind: "subtitle", Codec: "subrip", Language: "eng"}, {Index: 4, Kind: "subtitle", Codec: "subrip", Language: "spa"}}
	_, video, s := tracksFixture(t, audio, subs)
	h := s.Handler()
	fates := func(query string) string {
		t.Helper()
		w := request(h, "GET", "/api/maintenance/source-tracks?path="+url.QueryEscape(video)+"&mode=size&"+query, "", true)
		var out map[string]any
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatal(w.Code, w.Body.String())
		}
		streams := operationMap(operationMap(out["estimate"])["streams"])
		tracks, _ := streams["audio"].([]any)
		var parts []string
		for _, tr := range tracks {
			parts = append(parts, fmt.Sprintf("a%v:%v", operationMap(tr)["index"], operationString(operationMap(tr)["fate"]) == "dropped"))
		}
		subTracks, _ := operationMap(streams["subtitles"])["tracks"].([]any)
		for _, tr := range subTracks {
			parts = append(parts, fmt.Sprintf("s%v:%v", operationMap(tr)["index"], operationMap(tr)["fate"] == "dropped"))
		}
		return strings.Join(parts, " ")
	}
	if got := fates("keep_audio=1"); got != "a1:false a2:true s3:false s4:false" {
		t.Fatalf("keep_audio only: %s", got)
	}
	if got := fates("keep_subtitles=4"); got != "a1:false a2:false s3:true s4:false" {
		t.Fatalf("keep_subtitles only: %s", got)
	}
}

// The opened job, its explanation and its batch row weigh the same new-file
// size, the last setting tried when the job kept no file, so all of them
// blame the same stream.
func TestDetailCauseUsesTheLastSettingTriedSize(t *testing.T) {
	s, h := testUI(t)
	st := s.engine.Deps().Store
	path := "/media/shows/Sample/S01E04.mkv"
	rep := mediainspect.DetailedReport{
		DurationSec: 1000, SizeBytes: 4_000_000_000,
		Video: []mediainspect.DetailedStream{{Index: 0, Kind: "video", Codec: "h264", Width: 1920, Height: 1080}},
		Audio: []mediainspect.DetailedStream{{Index: 1, Kind: "audio", Codec: "truehd", Channels: 8, Language: "eng", BitRate: 16_000_000}},
	}
	seedOperation(t, st, "batch", "transcode_batch", "completed", map[string]any{"paths": []any{path}}, nil, nil)
	seedOperation(t, st, "child", "transcode_media", "completed", map[string]any{"path": path, "parent_action_id": "batch", "mode": "size"}, nil, map[string]any{
		"resolved_path": path, "skip_transcode": true, "reason_code": "size_budget_exceeded", "original": map[string]any{"size_bytes": 4_000_000_000},
		"size_budget":    map[string]any{"budget_bytes": 2_500_000_000},
		"quality_ladder": map[string]any{"video_codec": "libx265", "preset": "medium", "quality_values": []any{22}, "rung": 0, "tried": []any{ladderRung(22, 95, 3_000_000_000)}},
		"plan":           asJSONMap(transcode.Plan{VideoCodec: transcode.VideoCodecLibX265, AudioMode: "copy"}),
		"source_report":  asJSONMap(rep),
	})
	if err := st.CreateTranscodeBatchItem(store.TranscodeBatchItem{BatchID: "batch", ItemKey: "e4", FilePath: path, DisplayLabel: "e4", ChildActionID: "child", Status: "completed", Decision: "transcode"}); err != nil {
		t.Fatal(err)
	}
	detail := jobByID(t, getOperations(t, h, "id=child"), "child")
	cause := operationMap(detail["cause"])
	if cause["kind"] != "audio_too_big" || cause["stream"] != "audio" {
		t.Fatalf("detail cause %v", cause)
	}
	if happened := operationString(operationMap(detail["explanation"])["happened"]); happened != operationString(cause["summary"])+"." {
		t.Fatalf("explanation %q vs cause %v", happened, cause["summary"])
	}
	w := request(h, "GET", "/api/maintenance/batch-items?id=batch", "", true)
	var out struct {
		Items []map[string]any `json:"items"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil || len(out.Items) != 1 {
		t.Fatal(w.Code, w.Body.String())
	}
	if row := operationMap(out.Items[0]["cause"]); row["summary"] != cause["summary"] {
		t.Fatalf("batch row %v vs detail %v", row["summary"], cause["summary"])
	}
}

// A cause line as long as a line may be keeps its closing period within
// the limit in the explanation.
func TestExplanationLineFitsWithItsPeriod(t *testing.T) {
	s, h := testUI(t)
	st := s.engine.Deps().Store
	preset := strings.Repeat("p", 120)
	seedOperation(t, st, "long", "transcode_media", "completed", map[string]any{"path": "/media/movies/long.mkv", "mode": "size"}, nil, map[string]any{
		"resolved_path": "/media/movies/long.mkv", "skip_transcode": true, "reason_code": "quality_not_met", "original": map[string]any{"size_bytes": 4_000_000_000},
		"quality_ladder": map[string]any{"video_codec": "libx265", "preset": preset, "quality_values": []any{22}, "rung": 0, "tried": []any{ladderRung(22, 89.5, 1_000_000_000)}},
	})
	job := jobByID(t, getOperations(t, h, "id=long"), "long")
	happened := operationString(operationMap(job["explanation"])["happened"])
	if n := len([]rune(happened)); n > summaryLimit || strings.Contains(happened, "\n") || !strings.HasSuffix(happened, "….") {
		t.Fatalf("%d characters: %q", n, happened)
	}
}

// Past the read cap the queue says its total stops at the cap.
func TestReviewQueueSaysWhenItHitTheReadCap(t *testing.T) {
	s, h := testUI(t)
	defer func(old int) { queueReadCap = old }(queueReadCap)
	queueReadCap = 2
	st := s.engine.Deps().Store
	for i := range 3 {
		if _, err := st.AddItem(store.MaintenanceItem{MediaType: "movie", Service: "radarr", MediaID: fmt.Sprint(i + 1), Title: fmt.Sprintf("Sample %d", i), IssueType: "oversized", Priority: i}); err != nil {
			t.Fatal(err)
		}
	}
	w := request(h, "GET", "/api/maintenance/queue", "", true)
	var page queuePage
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	if page.Total != 2 || len(page.Items) != 2 || !page.Truncated || !page.Capped {
		t.Fatalf("page total %d, items %d, truncated %v, capped %v", page.Total, len(page.Items), page.Truncated, page.Capped)
	}
}

// A goal without a recorded minimum or target says no "0%" or "0 B" limit.
func TestCauseLeavesOutALimitWithoutANumber(t *testing.T) {
	s, h := testUI(t)
	st := s.engine.Deps().Store
	seedOperation(t, st, "nomin", "transcode_media", "completed", map[string]any{"path": "/media/movies/nomin.mkv"}, nil, map[string]any{
		"resolved_path": "/media/movies/nomin.mkv", "skip_transcode": true, "reason_code": "insufficient_savings", "original": map[string]any{"size_bytes": 1_000_000_000}, "candidate_size_bytes": 950_000_000})
	rep := mediainspect.DetailedReport{
		DurationSec: 1000, SizeBytes: 4_000_000_000,
		Video: []mediainspect.DetailedStream{{Index: 0, Kind: "video", Codec: "h264", Width: 1920, Height: 1080}},
		Audio: []mediainspect.DetailedStream{{Index: 1, Kind: "audio", Codec: "truehd", Channels: 8, Language: "eng", BitRate: 16_000_000}},
	}
	seedOperation(t, st, "notarget", "transcode_media", "completed", map[string]any{"path": "/media/movies/notarget.mkv", "mode": "size"}, nil, map[string]any{
		"resolved_path": "/media/movies/notarget.mkv", "skip_transcode": true, "reason_code": "size_budget_exceeded", "original": map[string]any{"size_bytes": 4_000_000_000}, "candidate_size_bytes": 3_000_000_000,
		"plan": asJSONMap(transcode.Plan{VideoCodec: transcode.VideoCodecLibX265, AudioMode: "copy"}), "source_report": asJSONMap(rep)})
	for _, id := range []string{"nomin", "notarget"} {
		for _, query := range []string{"group=workflow", "id=" + id} {
			job := jobByID(t, getOperations(t, h, query), id)
			summary := operationString(operationMap(job["cause"])["summary"])
			if summary == "" || strings.Contains(summary, " 0%") || strings.Contains(summary, " 0 B") {
				t.Fatalf("%s %s: %q", id, query, summary)
			}
			if id == "nomin" && summary != "New file 906 MB vs original 954 MB: only 5% smaller" {
				t.Fatalf("%s: %q", query, summary)
			}
		}
	}
}
