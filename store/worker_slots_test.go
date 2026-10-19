package store

import (
	"path/filepath"
	"testing"
)

func TestWorkerSlotSettingsRoundTrip(t *testing.T) {
	s := openTest(t)
	if row, err := s.GetWorkerSlotSettings("node-a"); err != nil || row != nil {
		t.Fatalf("a never-configured node has no row: %+v %v", row, err)
	}
	first, err := s.SetWorkerSlotSettings("node-a", 1, 1)
	if err != nil || first.Revision != 1 || first.Shared() || first.UpdatedAt == "" {
		t.Fatalf("first save: %+v %v", first, err)
	}
	second, err := s.SetWorkerSlotSettings("node-a", 2, 1)
	if err != nil || second.Revision != 2 || second.VideoSlots != 2 || second.PodcastSlots != 1 {
		t.Fatalf("update must overwrite and count the revision: %+v %v", second, err)
	}
	cleared, err := s.SetWorkerSlotSettings("node-a", 0, 0)
	if err != nil || !cleared.Shared() || cleared.Revision != 3 {
		t.Fatalf("clearing keeps the row so revisions keep counting: %+v %v", cleared, err)
	}
	if other, _ := s.GetWorkerSlotSettings("node-b"); other != nil {
		t.Fatal("settings leaked across nodes", other)
	}
}

func TestWorkerSlotSettingsRejectValuesNoCallerCouldMean(t *testing.T) {
	s := openTest(t)
	for name, tc := range map[string][3]any{
		"negative video":  {"n", -1, 1},
		"negative pod":    {"n", 1, -1},
		"mixed zero":      {"n", 0, 2},
		"mixed zero swap": {"n", 2, 0},
		"blank node":      {" ", 1, 1},
	} {
		if _, err := s.SetWorkerSlotSettings(tc[0].(string), tc[1].(int), tc[2].(int)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if row, _ := s.GetWorkerSlotSettings("n"); row != nil {
		t.Fatal("a rejected save stored a row", row)
	}
}

func TestWorkerSlotSettingsSurviveReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nav.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetWorkerSlotSettings("node-a", 2, 1); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	row, err := s.GetWorkerSlotSettings("node-a")
	if err != nil || row == nil || row.VideoSlots != 2 || row.PodcastSlots != 1 {
		t.Fatalf("split lost across restart: %+v %v", row, err)
	}
}
