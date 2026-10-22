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
	first, err := s.SetWorkerSlotSettings("node-a", 1, 1, 0)
	if err != nil || first.Revision != 1 || first.Shared() || first.UpdatedAt == "" {
		t.Fatalf("first save: %+v %v", first, err)
	}
	second, err := s.SetWorkerSlotSettings("node-a", 2, 1, 0)
	if err != nil || second.Revision != 2 || second.VideoSlots != 2 || second.PodcastSlots != 1 {
		t.Fatalf("update must overwrite and count the revision: %+v %v", second, err)
	}
	cleared, err := s.SetWorkerSlotSettings("node-a", 0, 0, 0)
	if err != nil || !cleared.Shared() || cleared.Revision != 3 {
		t.Fatalf("clearing keeps the row so revisions keep counting: %+v %v", cleared, err)
	}
	if other, _ := s.GetWorkerSlotSettings("node-b"); other != nil {
		t.Fatal("settings leaked across nodes", other)
	}
}

func TestWorkerSlotSettingsRejectValuesNoCallerCouldMean(t *testing.T) {
	s := openTest(t)
	for name, tc := range map[string][4]any{
		"negative video":   {"n", -1, 1, 0},
		"negative pod":     {"n", 1, -1, 0},
		"mixed zero":       {"n", 0, 2, 0},
		"mixed zero swap":  {"n", 2, 0, 0},
		"blank node":       {" ", 1, 1, 0},
		"negative ceiling": {"n", 0, 0, -1},
	} {
		if _, err := s.SetWorkerSlotSettings(tc[0].(string), tc[1].(int), tc[2].(int), tc[3].(int)); err == nil {
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
	if _, err := s.SetWorkerSlotSettings("node-a", 2, 1, 4); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	row, err := s.GetWorkerSlotSettings("node-a")
	if err != nil || row == nil || row.VideoSlots != 2 || row.PodcastSlots != 1 || row.Ceiling != 4 {
		t.Fatalf("split lost across restart: %+v %v", row, err)
	}
}

func TestWorkerSlotSettingsCarryTheCeiling(t *testing.T) {
	s := openTest(t)
	row, err := s.SetWorkerSlotSettings("node-a", 0, 0, 2)
	if err != nil || row.Ceiling != 2 || !row.Shared() {
		t.Fatalf("a ceiling without a split: %+v %v", row, err)
	}
	row, err = s.SetWorkerSlotSettings("node-a", 1, 1, 0)
	if err != nil || row.Ceiling != 0 || row.Revision != 2 {
		t.Fatalf("zero follows the worker's config again: %+v %v", row, err)
	}
}

func TestWorkerSlotSettingsDeleteReturnsToNeverConfigured(t *testing.T) {
	s := openTest(t)
	if err := s.DeleteWorkerSlotSettings("node-a"); err != nil {
		t.Fatal("deleting a node without settings is not an error:", err)
	}
	if _, err := s.SetWorkerSlotSettings("node-a", 2, 1, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteWorkerSlotSettings("node-a"); err != nil {
		t.Fatal(err)
	}
	if row, err := s.GetWorkerSlotSettings("node-a"); err != nil || row != nil {
		t.Fatalf("settings survived a delete: %+v %v", row, err)
	}
	first, err := s.SetWorkerSlotSettings("node-a", 1, 1, 0)
	if err != nil || first.Revision != 1 {
		t.Fatalf("a deleted node starts over: %+v %v", first, err)
	}
}
