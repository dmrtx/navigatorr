package store

import "testing"

func outcomeFixture(action, sha string) TranscodeOutcome {
	return TranscodeOutcome{ActionID: action, BenchmarkJobID: "bench-" + action, SourceSHA256: sha, SourceSize: 1000, Season: -1, Episode: -1,
		Mode: "size", PolicyVersion: "v-test", PolicyDigest: "sha256:policy", Outcome: OutcomeWinner, WinnerJSON: `{"w":1}`}
}

func TestTranscodeOutcomeRecordKeepsFinalOnRerecord(t *testing.T) {
	s := openTest(t)
	o := outcomeFixture("a1", "sha-1")
	if err := s.RecordTranscodeOutcome(o); err != nil {
		t.Fatal(err)
	}
	if found, err := s.RecordTranscodeOutcomeFinal("a1", "bench-a1", "pass", `{"verdict":"pass"}`); err != nil || !found {
		t.Fatalf("final: %v %v", found, err)
	}
	// A restarted wait step records the same search again.
	o.PointsJSON = `[{"candidate_id":"c"}]`
	if err := s.RecordTranscodeOutcome(o); err != nil {
		t.Fatal(err)
	}
	rows, err := s.TranscodeOutcomesForSource("sha-1", "sha256:policy", "", 5)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows: %+v %v", rows, err)
	}
	if rows[0].FinalVerdict != "pass" || rows[0].FinalJSON == "" || rows[0].PointsJSON == "" {
		t.Fatalf("re-recording must keep final validation and update points: %+v", rows[0])
	}
	if found, _ := s.RecordTranscodeOutcomeFinal("missing", "bench-missing", "fail", "{}"); found {
		t.Fatal("final for an unrecorded search must report not found")
	}
}

func TestTranscodeOutcomeRejectsIncompleteRows(t *testing.T) {
	s := openTest(t)
	o := outcomeFixture("a1", "")
	if s.RecordTranscodeOutcome(o) == nil {
		t.Fatal("a row without a source digest must be refused")
	}
	o = outcomeFixture("a1", "sha")
	o.Outcome = "maybe"
	if s.RecordTranscodeOutcome(o) == nil {
		t.Fatal("an unknown outcome must be refused")
	}
	if _, err := s.RecordTranscodeOutcomeFinal("a1", "bench-a1", "unknown", "{}"); err == nil {
		t.Fatal("an unknown verdict must be refused")
	}
}

func TestTranscodeOutcomesForSourceMatchesBytesAndPolicy(t *testing.T) {
	s := openTest(t)
	for _, o := range []TranscodeOutcome{outcomeFixture("a1", "sha-1"), outcomeFixture("a2", "sha-1"), outcomeFixture("a3", "sha-2")} {
		if err := s.RecordTranscodeOutcome(o); err != nil {
			t.Fatal(err)
		}
	}
	other := outcomeFixture("a4", "sha-1")
	other.PolicyDigest = "sha256:other"
	if err := s.RecordTranscodeOutcome(other); err != nil {
		t.Fatal(err)
	}
	rows, err := s.TranscodeOutcomesForSource("sha-1", "sha256:policy", "a2", 5)
	if err != nil || len(rows) != 1 || rows[0].ActionID != "a1" {
		t.Fatalf("only the other action's search of the same bytes and policy: %+v %v", rows, err)
	}
	rows, _ = s.TranscodeOutcomesForSource("sha-1", "sha256:policy", "", 5)
	if len(rows) != 2 || rows[0].ActionID != "a2" {
		t.Fatalf("newest first: %+v", rows)
	}
}

func TestSiblingTranscodeOutcomes(t *testing.T) {
	s := openTest(t)
	add := func(action, sha string, season int, folder string) {
		o := outcomeFixture(action, sha)
		o.LibraryService, o.LibraryID, o.Season, o.FolderKey = "sonarr", 7, season, folder
		if season < 0 {
			o.LibraryService, o.LibraryID = "", 0
		}
		if err := s.RecordTranscodeOutcome(o); err != nil {
			t.Fatal(err)
		}
	}
	add("s1e1", "sha-a", 1, "f1")
	add("s1e2", "sha-b", 1, "f1")
	add("s2e1", "sha-c", 2, "f2")
	add("loose", "sha-d", -1, "f3")
	rows, err := s.SiblingTranscodeOutcomes(SiblingQuery{LibraryService: "sonarr", LibraryID: 7, Season: 1, ExcludeSHA256: "sha-a", Limit: 10})
	if err != nil || len(rows) != 1 || rows[0].ActionID != "s1e2" {
		t.Fatalf("same season, source excluded: %+v %v", rows, err)
	}
	rows, _ = s.SiblingTranscodeOutcomes(SiblingQuery{LibraryService: "sonarr", LibraryID: 7, Season: -1, Limit: 10})
	if len(rows) != 3 {
		t.Fatalf("any season: %+v", rows)
	}
	rows, _ = s.SiblingTranscodeOutcomes(SiblingQuery{FolderKey: "f3", Season: -1, Limit: 10})
	if len(rows) != 1 || rows[0].ActionID != "loose" {
		t.Fatalf("folder fallback: %+v", rows)
	}
	if rows, _ := s.SiblingTranscodeOutcomes(SiblingQuery{Season: -1, Limit: 10}); rows != nil {
		t.Fatalf("a query without identity matches nothing: %+v", rows)
	}
}
