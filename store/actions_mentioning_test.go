package store

import (
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestListActionInstancesMentioningFiltersInSQL(t *testing.T) {
	s := openTest(t)
	rows := []ActionInstance{
		{ID: "done-a", ActionName: "transcode_media", Status: ActionStatusCompleted, StateJSON: `{"resolved_path":"/m/A.mkv"}`},
		{ID: "failed-a", ActionName: "transcode_media", Status: ActionStatusFailed, StateJSON: `{"resolved_path":"/m/A.mkv"}`},
		{ID: "running-a", ActionName: "transcode_media", Status: ActionStatusRunning, StateJSON: `{"resolved_path":"/m/A.mkv"}`},
		{ID: "done-b", ActionName: "transcode_media", Status: ActionStatusCompleted, StateJSON: `{"resolved_path":"/m/B.mkv"}`},
		{ID: "other-a", ActionName: "promote_transcode_candidate", Status: ActionStatusCompleted, StateJSON: `{"resolved_path":"/m/A.mkv"}`},
	}
	for _, r := range rows {
		if err := s.CreateActionInstance(r); err != nil {
			t.Fatal(err)
		}
	}
	ids := func(in, notIn []string) []string {
		t.Helper()
		got, err := s.ListActionInstancesMentioning("transcode_media", "A.mkv", in, notIn)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, g := range got {
			out = append(out, g.ID)
		}
		slices.Sort(out)
		return out
	}
	if got := ids(nil, nil); !slices.Equal(got, []string{"done-a", "failed-a", "running-a"}) {
		t.Fatalf("name and needle only = %v", got)
	}
	if got := ids([]string{ActionStatusCompleted, ActionStatusFailed}, nil); !slices.Equal(got, []string{"done-a", "failed-a"}) {
		t.Fatalf("in = %v", got)
	}
	if got := ids(nil, []string{ActionStatusCompleted}); !slices.Equal(got, []string{"failed-a", "running-a"}) {
		t.Fatalf("notIn = %v", got)
	}
	if got, err := s.ListActionInstancesMentioning("transcode_media", "", nil, nil); err != nil || got != nil {
		t.Fatalf("empty needle = %v, %v", got, err)
	}
}

func TestListActionInstancesMentioningCombinesFiltersAndMatchesLiterally(t *testing.T) {
	s := openTest(t)
	names := []string{`Dune (2021) 100%_cut.mkv`, `Él said "hi" \ back.mkv`, `Plain.mkv`}
	var rows []ActionInstance
	for i, n := range names {
		for _, st := range []string{ActionStatusCompleted, ActionStatusFailed, ActionStatusCancelled} {
			// State is stored JSON-encoded, as the engine writes it.
			rows = append(rows, ActionInstance{ID: st + "-" + string(rune('a'+i)), ActionName: "transcode_media", Status: st, StateJSON: `{"resolved_path":"/m/` + strings.Trim(mustJSON(t, n), `"`) + `"}`})
		}
	}
	for _, r := range rows {
		if err := s.CreateActionInstance(r); err != nil {
			t.Fatal(err)
		}
	}
	ids := func(needle string, in, notIn []string) []string {
		t.Helper()
		got, err := s.ListActionInstancesMentioning("transcode_media", needle, in, notIn)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, g := range got {
			out = append(out, g.ID)
		}
		slices.Sort(out)
		return out
	}
	// `in` and `notIn` apply together.
	if got := ids("Plain.mkv", []string{ActionStatusCompleted, ActionStatusFailed}, []string{ActionStatusFailed}); !slices.Equal(got, []string{"completed-c"}) {
		t.Fatalf("in and notIn together = %v", got)
	}
	if got := ids("Plain.mkv", []string{ActionStatusCancelled}, nil); !slices.Equal(got, []string{"cancelled-c"}) {
		t.Fatalf("cancelled = %v", got)
	}
	// % and _ are literal, never LIKE patterns; quotes, backslashes and unicode match as encoded.
	for i, n := range names[:2] {
		needle := strings.Trim(mustJSON(t, n), `"`)
		want := []string{"cancelled-" + string(rune('a'+i)), "completed-" + string(rune('a'+i)), "failed-" + string(rune('a'+i))}
		if got := ids(needle, nil, nil); !slices.Equal(got, want) {
			t.Fatalf("%q = %v, want %v", n, got, want)
		}
	}
	if got := ids("100%_cut", nil, nil); len(got) != 3 {
		t.Fatalf("literal percent and underscore = %v", got)
	}
	if got := ids("100xxcut", nil, nil); len(got) != 0 {
		t.Fatalf("a wildcard-shaped miss matched %v", got)
	}
	if got := ids("Nothing.mkv", nil, nil); len(got) != 0 {
		t.Fatalf("no match = %v", got)
	}
}

func mustJSON(t *testing.T, v string) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestListActionInstancesMentioningFindsAnOldMatchPastAHundredRows(t *testing.T) {
	s := openTest(t)
	// Only the first row mentions the file: a scan that stopped at a page of 100 would miss it.
	for i := 0; i < 150; i++ {
		state := `{"resolved_path":"/m/Other` + strconv.Itoa(i) + `.mkv"}`
		if i == 0 {
			state = `{"resolved_path":"/m/Target.mkv"}`
		}
		if err := s.CreateActionInstance(ActionInstance{ID: "row-" + strconv.Itoa(i), ActionName: "transcode_media", Status: ActionStatusCompleted, StateJSON: state}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.ListActionInstancesMentioning("transcode_media", "Target.mkv", nil, nil)
	if err != nil || len(got) != 1 || got[0].ID != "row-0" {
		t.Fatalf("got %v, %v", got, err)
	}
}
