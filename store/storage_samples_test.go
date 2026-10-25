package store

import (
	"testing"
	"time"
)

func TestStorageSamplesRoundTripInOrderAndPrune(t *testing.T) {
	s := openTest(t)
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	old := StorageSample{TakenAt: now.AddDate(0, 0, -900), Scope: "folder", Path: "/m/Anime", Volume: "/m/Anime", UsedBytes: 1, Source: "helper"}
	first := StorageSample{TakenAt: now.Add(-6 * time.Hour), Scope: "volume", Path: "/m/Anime", Volume: "/m/Anime", UsedBytes: 600, TotalBytes: 1000, FreeBytes: 380, Source: "helper"}
	second := StorageSample{TakenAt: now, Scope: "folder", Path: "/m/Anime", Volume: "/m/Anime", UsedBytes: 300, Source: "mount"}
	if err := s.AddStorageSamples([]StorageSample{second, old, first}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddStorageSamples(nil); err != nil {
		t.Fatal(err)
	}
	got, err := s.StorageSamples(now.AddDate(0, 0, -1000))
	if err != nil || len(got) != 3 || got[0].UsedBytes != 1 || got[1].TotalBytes != 1000 || got[2].Source != "mount" || got[2].Volume != "/m/Anime" {
		t.Fatalf("%+v %v", got, err)
	}
	last, err := s.LastStorageSampleTime()
	if err != nil || !last.Equal(now) {
		t.Fatalf("last %v %v", last, err)
	}
	if err := s.PruneStorageSamples(now.AddDate(0, 0, -800)); err != nil {
		t.Fatal(err)
	}
	got, _ = s.StorageSamples(now.AddDate(0, 0, -1000))
	if len(got) != 2 {
		t.Fatalf("pruning kept %d", len(got))
	}
	empty := openTest(t)
	if last, err := empty.LastStorageSampleTime(); err != nil || !last.IsZero() {
		t.Fatalf("empty store: %v %v", last, err)
	}
}
