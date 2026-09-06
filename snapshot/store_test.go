package snapshot

import (
	"testing"
	"time"
)

func TestSnapshotCreateAndPagination(t *testing.T) {
	store := NewStore(time.Minute)

	var items []any
	for i := 0; i < 25; i++ {
		items = append(items, map[string]any{"id": i, "title": "Item"})
	}

	snap := store.Create("radarr", "/movie", "", items)
	if snap.Total != 25 {
		t.Fatalf("expected 25 items, got %d", snap.Total)
	}

	found, ok := store.Find("radarr", "/movie", "")
	if !ok || found.ID != snap.ID {
		t.Fatalf("expected to find snapshot by key")
	}

	// Page 1: limit 10
	cursor := EncodeCursor(snap.ID, 0)
	page1, next1, complete1, total1, off1, err := store.GetPage(cursor, 10)
	if err != nil {
		t.Fatalf("page 1 failed: %v", err)
	}
	if len(page1) != 10 || complete1 || total1 != 25 || off1 != 0 {
		t.Fatalf("page 1 unexpected result: len=%d complete=%v total=%d off=%d", len(page1), complete1, total1, off1)
	}

	// Page 2: limit 10
	page2, next2, complete2, total2, off2, err := store.GetPage(next1, 10)
	if err != nil {
		t.Fatalf("page 2 failed: %v", err)
	}
	if len(page2) != 10 || complete2 || total2 != 25 || off2 != 10 {
		t.Fatalf("page 2 unexpected result: len=%d complete=%v total=%d off=%d", len(page2), complete2, total2, off2)
	}

	// Page 3: limit 10 (remaining 5)
	page3, next3, complete3, total3, off3, err := store.GetPage(next2, 10)
	if err != nil {
		t.Fatalf("page 3 failed: %v", err)
	}
	if len(page3) != 5 || !complete3 || next3 != "" || total3 != 25 || off3 != 20 {
		t.Fatalf("page 3 unexpected result: len=%d complete=%v next=%q off=%d", len(page3), complete3, next3, off3)
	}
}

func TestSnapshotInvalidate(t *testing.T) {
	store := NewStore(time.Minute)
	items := []any{"a", "b"}
	store.Create("radarr", "/movie", "", items)
	store.Create("sonarr", "/series", "", items)

	if _, ok := store.Find("radarr", "/movie", ""); !ok {
		t.Fatal("radarr snapshot not found")
	}
	if _, ok := store.Find("sonarr", "/series", ""); !ok {
		t.Fatal("sonarr snapshot not found")
	}

	store.Invalidate("radarr")
	if _, ok := store.Find("radarr", "/movie", ""); ok {
		t.Error("expected radarr snapshot to be invalidated")
	}
	if _, ok := store.Find("sonarr", "/series", ""); !ok {
		t.Error("expected sonarr snapshot to still exist")
	}
}
