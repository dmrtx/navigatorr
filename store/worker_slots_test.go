package store

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestWorkerSlotSettingsRoundTrip(t *testing.T) {
	s := openTest(t)
	if row, err := s.GetWorkerSlotSettings("node-a"); err != nil || row != nil {
		t.Fatalf("a never-configured node has no row: %+v %v", row, err)
	}
	first, err := s.SetWorkerSlotSettings("node-a", 1, 1, 0, 0, 0)
	if err != nil || first.Revision != 1 || first.Shared() || first.UpdatedAt == "" {
		t.Fatalf("first save: %+v %v", first, err)
	}
	second, err := s.SetWorkerSlotSettings("node-a", 2, 1, 0, 0, 0)
	if err != nil || second.Revision != 2 || second.VideoSlots != 2 || second.PodcastSlots != 1 {
		t.Fatalf("update must overwrite and count the revision: %+v %v", second, err)
	}
	cleared, err := s.SetWorkerSlotSettings("node-a", 0, 0, 0, 0, 0)
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
		if _, err := s.SetWorkerSlotSettings(tc[0].(string), tc[1].(int), tc[2].(int), tc[3].(int), 0, 0); err == nil {
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
	if _, err := s.SetWorkerSlotSettings("node-a", 2, 1, 4, 0, 0); err != nil {
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
	row, err := s.SetWorkerSlotSettings("node-a", 0, 0, 2, 0, 0)
	if err != nil || row.Ceiling != 2 || !row.Shared() {
		t.Fatalf("a ceiling without a split: %+v %v", row, err)
	}
	row, err = s.SetWorkerSlotSettings("node-a", 1, 1, 0, 0, 0)
	if err != nil || row.Ceiling != 0 || row.Revision != 2 {
		t.Fatalf("zero follows the worker's config again: %+v %v", row, err)
	}
}

func TestWorkerSlotSettingsDeleteReturnsToNeverConfigured(t *testing.T) {
	s := openTest(t)
	if err := s.DeleteWorkerSlotSettings("node-a"); err != nil {
		t.Fatal("deleting a node without settings is not an error:", err)
	}
	if _, err := s.SetWorkerSlotSettings("node-a", 2, 1, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteWorkerSlotSettings("node-a"); err != nil {
		t.Fatal(err)
	}
	if row, err := s.GetWorkerSlotSettings("node-a"); err != nil || row != nil {
		t.Fatalf("settings survived a delete: %+v %v", row, err)
	}
	first, err := s.SetWorkerSlotSettings("node-a", 1, 1, 0, 0, 0)
	if err != nil || first.Revision != 1 {
		t.Fatalf("a deleted node starts over: %+v %v", first, err)
	}
}

func TestWorkerSlotSettingsX265Limit(t *testing.T) {
	s := openTest(t)
	row, err := s.SetWorkerSlotSettings("node-a", 0, 0, 2, 1, 0)
	if err != nil || row.X265Slots != 1 || row.Ceiling != 2 || !row.Shared() {
		t.Fatalf("%+v %v", row, err)
	}
	got, err := s.GetWorkerSlotSettings("node-a")
	if err != nil || got.X265Slots != 1 {
		t.Fatalf("not stored: %+v %v", got, err)
	}
	if row, err = s.SetWorkerSlotSettings("node-a", 0, 0, 2, 0, 0); err != nil || row.X265Slots != 0 {
		t.Fatalf("zero removes the limit: %+v %v", row, err)
	}
	if _, err := s.SetWorkerSlotSettings("node-a", 0, 0, 2, -1, 0); err == nil {
		t.Fatal("a negative limit was accepted")
	}
}

func TestWorkerSlotSettingsKeepTheCoreBudget(t *testing.T) {
	s := openTest(t)
	row, err := s.SetWorkerSlotSettings("node-a", 0, 0, 0, 0, 6)
	if err != nil || row.CoreBudget != 6 {
		t.Fatalf("budget not stored: %+v %v", row, err)
	}
	got, err := s.GetWorkerSlotSettings("node-a")
	if err != nil || got == nil || got.CoreBudget != 6 {
		t.Fatalf("budget not read back: %+v %v", got, err)
	}
	if row, err = s.SetWorkerSlotSettings("node-a", 0, 0, 0, 0, 0); err != nil || row.CoreBudget != 0 || row.Revision != 2 {
		t.Fatalf("budget not cleared: %+v %v", row, err)
	}
	if _, err := s.SetWorkerSlotSettings("node-a", 0, 0, 0, 0, -1); err == nil {
		t.Fatal("a negative budget was stored")
	}
}

func TestWorkerSlotSettingsCompareAndSwap(t *testing.T) {
	s := openTest(t)
	row := WorkerSlotSettings{Node: "node-a", VideoSlots: 1, PodcastSlots: 1, Ceiling: 4}
	first, err := s.CompareAndSwapWorkerSlotSettings(row)
	if err != nil || first.Revision != 1 {
		t.Fatalf("first save: %+v %v", first, err)
	}
	if _, err := s.CompareAndSwapWorkerSlotSettings(row); !errors.Is(err, ErrWorkerSlotSettingsChanged) {
		t.Fatalf("revision zero overwrote an existing row: %v", err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, cores := range []int{4, 6} {
		go func() {
			<-start
			update := first
			update.CoreBudget = cores
			_, err := s.CompareAndSwapWorkerSlotSettings(update)
			results <- err
		}()
	}
	close(start)
	saved, conflicted := 0, 0
	for range 2 {
		switch err := <-results; {
		case err == nil:
			saved++
		case errors.Is(err, ErrWorkerSlotSettingsChanged):
			conflicted++
		default:
			t.Fatal(err)
		}
	}
	got, err := s.GetWorkerSlotSettings("node-a")
	if saved != 1 || conflicted != 1 || err != nil || got == nil || got.Revision != 2 {
		t.Fatalf("concurrent saves: saved %d, conflicted %d, row %+v, error %v", saved, conflicted, got, err)
	}
	first.Node = "node-b"
	if _, err := s.CompareAndSwapWorkerSlotSettings(first); !errors.Is(err, ErrWorkerSlotSettingsChanged) {
		t.Fatalf("a missing row accepted a positive revision: %v", err)
	}
}

func TestWorkerSlotSettingsRestoreOnlyCurrentRevision(t *testing.T) {
	for _, configured := range []bool{false, true} {
		t.Run(map[bool]string{false: "unconfigured", true: "configured"}[configured], func(t *testing.T) {
			s := openTest(t)
			var previous *WorkerSlotSettings
			if configured {
				row, err := s.SetWorkerSlotSettings("node-a", 1, 1, 4, 1, 6)
				if err != nil {
					t.Fatal(err)
				}
				previous = &row
			}
			saved, err := s.SetWorkerSlotSettings("node-a", 3, 1, 0, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.RestoreWorkerSlotSettings("node-a", saved.Revision, previous); err != nil {
				t.Fatal(err)
			}
			got, err := s.GetWorkerSlotSettings("node-a")
			if err != nil {
				t.Fatal(err)
			}
			if previous == nil {
				if got != nil {
					t.Fatalf("a refused first save must leave no row: %+v", got)
				}
			} else if got == nil || got.VideoSlots != previous.VideoSlots || got.PodcastSlots != previous.PodcastSlots || got.Ceiling != previous.Ceiling || got.X265Slots != previous.X265Slots || got.CoreBudget != previous.CoreBudget || got.Revision != saved.Revision+1 {
				t.Fatalf("previous settings not restored: %+v", got)
			}
			newer, err := s.SetWorkerSlotSettings("node-a", 0, 0, 2, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.RestoreWorkerSlotSettings("node-a", newer.Revision+1, previous); err != nil {
				t.Fatal(err)
			}
			got, err = s.GetWorkerSlotSettings("node-a")
			if err != nil || got == nil || *got != newer {
				t.Fatalf("a revision mismatch changed settings: %+v %v", got, err)
			}
		})
	}
}
