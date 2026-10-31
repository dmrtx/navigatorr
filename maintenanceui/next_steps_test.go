package maintenanceui

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/store"
)

// heavyAudioReport is an hour of 1080p SDR video at about 2.6 GB with 1.6 GB
// of audio: a lossless 5.1 track and an AC-3 commentary, plus one subtitle.
func heavyAudioReport(height int) map[string]any {
	rep := mediainspect.DetailedReport{SizeBytes: 4_000_000_000, DurationSec: 3600,
		Video:     []mediainspect.DetailedStream{{Index: 0, Codec: "hevc", Width: 1920, Height: height, BitDepth: 10}},
		Audio:     []mediainspect.DetailedStream{{Index: 1, Codec: "dts", Profile: "DTS-HD MA", Channels: 6, ChannelLayout: "5.1(side)", Language: "eng", BitRate: 3_000_000}, {Index: 2, Codec: "ac3", Channels: 6, ChannelLayout: "5.1(side)", Language: "eng", BitRate: 640_000}},
		Subtitles: []mediainspect.DetailedStream{{Index: 3, Codec: "subrip", Language: "eng"}},
	}
	data, _ := json.Marshal(rep)
	var out map[string]any
	_ = json.Unmarshal(data, &out)
	return out
}

func stepIDs(steps []nextStep) string {
	ids := make([]string, len(steps))
	for i, s := range steps {
		ids[i] = s.ID
	}
	return strings.Join(ids, ",")
}

func endedRecord(status string, inputs, state map[string]any) operationRecord {
	return operationRecord{inst: store.ActionInstance{ID: "job", ActionName: "transcode_media", Status: status}, inputs: inputs, outputs: map[string]any{}, state: state}
}

// A size goal that missed its saving on a file whose audio is heavy is offered
// Audio only first, then a target size, x265 (it ran on the media engine), the
// track picker, and the same settings last. It says what it reached and why.
func TestNextStepsAfterAMissedSavingFollowWhatWasMeasured(t *testing.T) {
	r := endedRecord("completed", map[string]any{"path": "/media/a.mkv", "mode": "size"}, map[string]any{
		"reason_code": "insufficient_savings", "original_size": float64(4_000_000_000), "candidate_size_bytes": float64(3_640_000_000),
		"mode_policy":   map[string]any{"mode": "size", "min_savings_percent": float64(15), "size": map[string]any{"hardware_search": map[string]any{"margin_percent": float64(30)}}},
		"source_report": heavyAudioReport(1080),
	})
	if got := stepIDs(nextStepsFor(r)); got != "audio_only,budget,x265,tracks,same" {
		t.Fatalf("steps %s", got)
	}
	e := explainResult(r)
	if !strings.Contains(e.Happened, "9% smaller") || !strings.Contains(e.Happened, "at least 15% smaller") {
		t.Fatalf("happened %q", e.Happened)
	}
	if len(e.Learned) < 2 || !strings.Contains(e.Learned[0], "audio is") || !strings.Contains(e.Learned[len(e.Learned)-1], "Converting the audio alone") {
		t.Fatalf("learned %q", e.Learned)
	}
	audio, _ := stepByID(r, "audio_only")
	if audio.Inputs["mode"] != "size" || audio.Inputs["audio_only"] != true {
		t.Fatalf("audio only step %+v", audio)
	}

	// A 4K source is also offered Shrink to 1080p SDR; a run without the media
	// engine is not offered x265 again.
	r.state["source_report"] = heavyAudioReport(2160)
	r.state["mode_policy"] = map[string]any{"mode": "size", "min_savings_percent": float64(15), "size": map[string]any{}}
	if got := stepIDs(nextStepsFor(r)); got != "audio_only,budget,compact,tracks,same" {
		t.Fatalf("4K steps %s", got)
	}
}

// The steps of the endings that are not a missed saving.
func TestNextStepsForOtherEndings(t *testing.T) {
	for name, tc := range map[string]struct {
		status string
		inputs map[string]any
		state  map[string]any
		want   string
	}{
		"cancelled": {"cancelled", map[string]any{"path": "/a", "mode": "size"}, map[string]any{}, "same"},
		"HDR under Save space": {"completed", map[string]any{"path": "/a", "mode": "size"}, map[string]any{"reason_code": "hdr_requires_compact", "source_report": heavyAudioReport(2160)}, "compact,audio_only,tracks"},
		"media engine missed quality": {"completed", map[string]any{"path": "/a", "mode": "size"}, map[string]any{"reason_code": "quality_not_met", "quality_ladder": map[string]any{"retry_without_hardware": true, "tried": []any{map[string]any{}, map[string]any{}, map[string]any{}}}}, "x265,same"},
		"budget missed": {"completed", map[string]any{"path": "/a", "mode": "size", "size_budget_percent": float64(40)}, map[string]any{"reason_code": "size_budget_exceeded", "original_size": float64(1000), "candidate_size_bytes": float64(520), "size_budget": map[string]any{"budget_bytes": float64(400)}}, "budget,same"},
		"Audio only had nothing to do": {"completed", map[string]any{"path": "/a", "mode": "size", "audio_only": true}, map[string]any{"reason_code": "audio_only_nothing_to_convert", "skip_transcode": true}, "size"},
		"a result is not a step":       {"completed", map[string]any{"path": "/a", "mode": "size"}, map[string]any{}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			r := endedRecord(tc.status, tc.inputs, tc.state)
			if got := stepIDs(nextStepsFor(r)); got != tc.want {
				t.Fatalf("steps %q, want %q", got, tc.want)
			}
		})
	}
	budget := endedRecord("completed", map[string]any{"path": "/a", "mode": "size"}, map[string]any{"reason_code": "size_budget_exceeded", "original_size": float64(1000), "candidate_size_bytes": float64(520), "size_budget": map[string]any{"budget_bytes": float64(400)}})
	if s, _ := stepByID(budget, "budget"); s.Inputs["size_budget_percent"] != float64(60) {
		t.Fatalf("raised budget %+v", s)
	}
	if e := explainResult(budget); !strings.Contains(e.Happened, "52% of the original; the target was 40%") {
		t.Fatalf("budget explanation %q", e.Happened)
	}
	ladder := endedRecord("completed", map[string]any{"path": "/a", "mode": "size"}, map[string]any{"reason_code": "quality_not_met", "quality_ladder": map[string]any{"tried": []any{1, 2, 3}}})
	if e := explainResult(ladder); !strings.Contains(e.Happened, "Each of the 3 settings") {
		t.Fatalf("ladder explanation %q", e.Happened)
	}
}

// A step starts a new job from the neutral inputs and the step's own; the
// server refuses a step it does not offer and the track picker.
func TestRerunStartsAnOfferedStep(t *testing.T) {
	s, h := testUI(t)
	st := s.engine.Deps().Store
	inputs := map[string]any{"path": "/media/Show/S01E01.mkv", "mode": "size", "replace_original": false, "allow_hardware": true, "keep_languages": []any{"eng"}}
	state := map[string]any{"reason_code": "insufficient_savings", "original_size": float64(4_000_000_000), "candidate_size_bytes": float64(3_640_000_000), "source_report": heavyAudioReport(1080),
		"mode_policy": map[string]any{"mode": "size", "min_savings_percent": float64(15), "size": map[string]any{"hardware_search": map[string]any{"margin_percent": float64(30)}}}}
	seedOperation(t, st, "missed", "transcode_media", "completed", inputs, map[string]any{"reason_code": "insufficient_savings"}, state)

	job := rerunJobView(t, h, "missed")
	steps, _ := job["next_steps"].([]any)
	if len(steps) != 5 || operationMap(steps[0])["id"] != "audio_only" || operationMap(job["explanation"])["happened"] == nil {
		t.Fatalf("job view %v", job)
	}
	for _, refused := range []string{`{"id":"missed","key":"step-compact","step":"compact"}`, `{"id":"missed","key":"step-tracks","step":"tracks"}`, `{"id":"missed","key":"step-mixed","step":"audio_only","without_hardware":true}`} {
		if w := request(h, "POST", "/api/maintenance/rerun", refused, true); w.Code != 409 {
			t.Fatalf("%s: %d %s", refused, w.Code, w.Body.String())
		}
	}
	start := func(body string) map[string]any {
		w := request(h, "POST", "/api/maintenance/rerun", body, true)
		if w.Code != 202 {
			t.Fatal(w.Code, w.Body.String())
		}
		var started struct{ ID string }
		_ = json.Unmarshal(w.Body.Bytes(), &started)
		created, _ := st.GetActionInstance(started.ID)
		// Let the next start see this one as finished, not active on the file.
		created.Status = store.ActionStatusCancelled
		if err := st.UpdateActionInstance(*created); err != nil {
			t.Fatal(err)
		}
		return decodeOperationJSON(created.InputsJSON)
	}
	audio := start(`{"id":"missed","key":"step-audio","step":"audio_only"}`)
	if audio["mode"] != "size" || audio["audio_only"] != true || audio["replace_original"] != false || audio["path"] != inputs["path"] {
		t.Fatalf("audio only inputs %v", audio)
	}
	for _, dropped := range []string{"allow_hardware", "keep_languages", "hardware_margin_percent"} {
		if _, has := audio[dropped]; has {
			t.Fatalf("goal switch carried %s: %v", dropped, audio)
		}
	}
	x265 := start(`{"id":"missed","key":"step-x265","step":"x265"}`)
	if x265["allow_hardware"] != false || x265["keep_languages"] == nil {
		t.Fatalf("x265 inputs %v", x265)
	}
	budget := start(`{"id":"missed","key":"step-budget","step":"budget"}`)
	if budget["size_budget_percent"] != offeredBudgetPercent {
		t.Fatalf("budget inputs %v", budget)
	}
}
