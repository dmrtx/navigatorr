package maintenanceui

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"
)

func TestDailySavingsBucketsByTheClientsDay(t *testing.T) {
	// 03:30 UTC on Oct 26 is still the evening of Oct 25 in Ecuador (UTC-5).
	now := time.Date(2026, 10, 26, 3, 30, 0, 0, time.UTC)
	history := []savingsReplacement{
		{CompletedAt: "2026-09-01T10:00:00Z", SavedBytes: 5, CumulativeBytes: 5},
		{CompletedAt: "2026-10-24T20:00:00Z", SavedBytes: 10, CumulativeBytes: 15},
		{CompletedAt: "2026-10-26T01:00:00Z", SavedBytes: 20, CumulativeBytes: 35},
		{CompletedAt: "2026-10-26T02:00:00Z", SavedBytes: 30, CumulativeBytes: 65},
		{CompletedAt: "garbage", SavedBytes: 99, CumulativeBytes: 164},
	}
	days := dailySavings(history, now, time.FixedZone("ec", -5*3600))
	if len(days) != savingsDays || days[len(days)-1].Date != "2026-10-25" {
		t.Fatalf("window: %+v", days)
	}
	last, before := days[len(days)-1], days[len(days)-2]
	if last.SavedBytes != 50 || last.Count != 2 || last.CumulativeBytes != 65 {
		t.Fatalf("evening of the 25th: %+v", last)
	}
	if before.Date != "2026-10-24" || before.SavedBytes != 10 || before.CumulativeBytes != 15 {
		t.Fatalf("24th: %+v", before)
	}
	// A day with no replacements carries the running total, so the line never dips.
	if days[0].CumulativeBytes != 5 || days[len(days)-3].CumulativeBytes != 5 {
		t.Fatalf("empty days must carry the total from before the window: %+v", days)
	}
	utc := dailySavings(history, now, time.UTC)
	if utc[len(utc)-1].Date != "2026-10-26" || utc[len(utc)-1].SavedBytes != 50 {
		t.Fatalf("UTC day: %+v", utc[len(utc)-1])
	}
}

func TestClientZoneIgnoresNonsense(t *testing.T) {
	for query, want := range map[string]int{"": 0, "tz_offset=-300": -300 * 60, "tz_offset=abc": 0, "tz_offset=99999": 0} {
		_, offset := time.Now().In(clientZone(httptest.NewRequest("GET", "/?"+query, nil))).Zone()
		if offset != want {
			t.Fatalf("%q: offset %d, want %d", query, offset, want)
		}
	}
}

func TestOperationsListsTheLargestReadyCandidatesAndDays(t *testing.T) {
	s, h := testUI(t)
	st := s.engine.Deps().Store
	for i := 0; i < 10; i++ {
		id := string(rune('a' + i))
		media := map[string]any{"original": map[string]any{"size_bytes": 1000}, "result": map[string]any{"size_bytes": 1000 - (i+1)*50}, "original_intact": true, "original_sha256": id}
		seedOperation(t, st, "cand-"+id, "transcode_media", "completed", map[string]any{"path": "/media/" + id + ".mkv"}, media, nil)
	}
	seedOperation(t, st, "promoted", "promote_transcode_candidate", "completed", nil, map[string]any{"promoted": true, "recovery_retained": false, "promotion": map[string]any{"original_path": "/media/p.mkv", "original_sha256": "p", "original_bytes": 1000, "candidate_bytes": 400, "recovery_cleanup_completed": true, "recovery_verified_before_replacement": true}}, nil)
	w := request(h, "GET", "/api/maintenance/operations?limit=1&tz_offset=-300", "", true)
	var page operationsPage
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	if page.Savings.CandidateCount != 10 || len(page.Ready) != savingsReady {
		t.Fatalf("ready: count %d, listed %d", page.Savings.CandidateCount, len(page.Ready))
	}
	if page.Ready[0].SavedBytes != 500 || page.Ready[0].ActionID != "cand-j" || page.Ready[0].SourcePath != "/media/j.mkv" || page.Ready[0].SavedBytes < page.Ready[1].SavedBytes {
		t.Fatalf("largest first: %+v", page.Ready)
	}
	var freed, count int64
	for _, d := range page.Daily {
		freed += d.SavedBytes
		count += int64(d.Count)
	}
	if len(page.Daily) != savingsDays || freed != 600 || count != 1 || page.Daily[len(page.Daily)-1].CumulativeBytes != 600 {
		t.Fatalf("daily: %+v", page.Daily)
	}
}
