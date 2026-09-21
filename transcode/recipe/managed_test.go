package recipe

import (
	"strings"
	"testing"
)

func managedTestProfile() Profile {
	return Profile{
		Container: "mkv",
		Video:     VideoProfile{Codec: "libx265", Quality: 24, Preset: "slow", Profile: "main", PixelFormat: "yuv420p"},
		Audio:     AudioProfile{Mode: "copy"},
		Subtitles: SubtitleProfile{Mode: "preserve", ConvertIncompatible: true},
		Preserve:  PreserveProfile{Metadata: true, Chapters: true, Attachments: true},
		Resilience: ResilienceProfile{
			MaxAttempts:  1,
			MaxFallbacks: 1,
			Fallbacks:    []FallbackRule{{When: "container_subtitle_incompatible", Action: "apply_container_conversion"}},
		},
		Optimization: &OptimizationPolicy{
			Enabled: true,
			Search:  &SearchPolicy{QualityValues: []int{20, 22, 24, 26}},
		},
	}
}

func TestManagedProfilePersistenceHistoryAndDelete(t *testing.T) {
	dir := t.TempDir()
	m, err := NewManager(BuiltinProvider{}, dir, "", "")
	if err != nil {
		t.Fatal(err)
	}
	rec1, err := m.SaveManagedProfile("anime-x265", managedTestProfile(), "first", "act-1", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if rec1.Generation != 1 || rec1.Digest == "" {
		t.Fatalf("unexpected first record: %+v", rec1)
	}

	p2 := managedTestProfile()
	p2.Video.Quality = 22
	if _, err := m.SaveManagedProfile("anime-x265", p2, "second", "act-2", 0, rec1.Digest); err == nil || !strings.Contains(err.Error(), "expected_generation is required") {
		t.Fatalf("expected update without expected_generation to fail closed, got %v", err)
	}
	if _, err := m.SaveManagedProfile("anime-x265", p2, "second", "act-2", rec1.Generation, ""); err == nil || !strings.Contains(err.Error(), "expected_digest is required") {
		t.Fatalf("expected update without expected_digest to fail closed, got %v", err)
	}
	rec2, err := m.SaveManagedProfile("anime-x265", p2, "second", "act-2", rec1.Generation, rec1.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if rec2.Generation != 2 || rec2.Digest == rec1.Digest {
		t.Fatalf("expected generation/digest change: first=%+v second=%+v", rec1, rec2)
	}

	reopened, err := NewManager(BuiltinProvider{}, dir, "", "")
	if err != nil {
		t.Fatal(err)
	}
	got, ok, err := reopened.GetManagedProfile("anime-x265")
	if err != nil || !ok {
		t.Fatalf("reopen get: ok=%v err=%v", ok, err)
	}
	if got.Generation != 2 || got.Digest != rec2.Digest {
		t.Fatalf("persisted record mismatch: %+v", got)
	}
	history, err := reopened.ManagedProfileHistory("anime-x265")
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 || history[0].Event != "saved" || history[1].Generation != 2 {
		t.Fatalf("unexpected history: %+v", history)
	}

	if _, err := reopened.DeleteManagedProfile("anime-x265", 0, rec2.Digest); err == nil || !strings.Contains(err.Error(), "expected_generation is required") {
		t.Fatalf("expected delete without expected_generation to fail closed, got %v", err)
	}
	if _, err := reopened.DeleteManagedProfile("anime-x265", rec2.Generation, ""); err == nil || !strings.Contains(err.Error(), "expected_digest is required") {
		t.Fatalf("expected delete without expected_digest to fail closed, got %v", err)
	}
	if _, err := reopened.DeleteManagedProfile("anime-x265", rec1.Generation, rec1.Digest); err == nil {
		t.Fatal("expected optimistic concurrency mismatch")
	}
	deleted, err := reopened.DeleteManagedProfile("anime-x265", rec2.Generation, rec2.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if deleted.Event != "deleted" {
		t.Fatalf("unexpected delete event: %+v", deleted)
	}
	if _, ok, err := reopened.GetManagedProfile("anime-x265"); err != nil || ok {
		t.Fatalf("profile should be deleted, ok=%v err=%v", ok, err)
	}
	history, err = reopened.ManagedProfileHistory("anime-x265")
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 3 || history[2].Event != "deleted" {
		t.Fatalf("delete not recorded: %+v", history)
	}

	p3 := managedTestProfile()
	p3.Video.Quality = 20
	rec3, err := reopened.SaveManagedProfile("anime-x265", p3, "recreated", "act-3", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if rec3.Generation != 3 {
		t.Fatalf("generation must remain monotonic across delete/recreate: got %d, want 3", rec3.Generation)
	}
	if rec3.Digest == rec1.Digest || rec3.Digest == rec2.Digest {
		t.Fatalf("recreated profile should have a distinct digest in this fixture: %+v", rec3)
	}
	history, err = reopened.ManagedProfileHistory("anime-x265")
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 4 || history[3].Event != "saved" || history[3].Generation != 3 {
		t.Fatalf("recreate generation/history mismatch: %+v", history)
	}
}


func TestManagedProfileCASRejectsMetadataOnlyConcurrentChange(t *testing.T) {
	m, err := NewManager(BuiltinProvider{}, t.TempDir(), "", "")
	if err != nil {
		t.Fatal(err)
	}

	p := managedTestProfile()
	rec1, err := m.SaveManagedProfile("anime-x265", p, "first metadata", "act-1", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	rec2, err := m.SaveManagedProfile("anime-x265", p, "metadata changed", "act-2", rec1.Generation, rec1.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if rec2.Generation != 2 {
		t.Fatalf("metadata-only save should advance generation: got %d", rec2.Generation)
	}
	if rec2.Digest != rec1.Digest {
		t.Fatalf("metadata-only save should preserve profile digest: %s != %s", rec2.Digest, rec1.Digest)
	}

	p3 := managedTestProfile()
	p3.Video.Quality = 22
	if _, err := m.SaveManagedProfile("anime-x265", p3, "stale writer", "act-stale", rec1.Generation, rec1.Digest); err == nil || !strings.Contains(err.Error(), "expected generation 1, current 2") {
		t.Fatalf("stale metadata-era update should fail on generation even with matching digest, got %v", err)
	}
	if _, err := m.DeleteManagedProfile("anime-x265", rec1.Generation, rec1.Digest); err == nil || !strings.Contains(err.Error(), "expected generation 1, current 2") {
		t.Fatalf("stale metadata-era delete should fail on generation even with matching digest, got %v", err)
	}
}

func TestManagedProfileCASRejectsABAContentCycle(t *testing.T) {
	m, err := NewManager(BuiltinProvider{}, t.TempDir(), "", "")
	if err != nil {
		t.Fatal(err)
	}

	a := managedTestProfile()
	recA1, err := m.SaveManagedProfile("anime-x265", a, "A1", "act-a1", 0, "")
	if err != nil {
		t.Fatal(err)
	}

	b := managedTestProfile()
	b.Video.Quality = 22
	recB, err := m.SaveManagedProfile("anime-x265", b, "B", "act-b", recA1.Generation, recA1.Digest)
	if err != nil {
		t.Fatal(err)
	}
	recA3, err := m.SaveManagedProfile("anime-x265", a, "A3", "act-a3", recB.Generation, recB.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if recA3.Generation != 3 || recA3.Digest != recA1.Digest {
		t.Fatalf("expected A -> B -> A to restore digest but advance generation: A1=%+v A3=%+v", recA1, recA3)
	}

	stale := managedTestProfile()
	stale.Video.Quality = 20
	if _, err := m.SaveManagedProfile("anime-x265", stale, "stale A1 writer", "act-stale", recA1.Generation, recA1.Digest); err == nil || !strings.Contains(err.Error(), "expected generation 1, current 3") {
		t.Fatalf("ABA stale update should fail on generation, got %v", err)
	}
	if _, err := m.DeleteManagedProfile("anime-x265", recA1.Generation, recA1.Digest); err == nil || !strings.Contains(err.Error(), "expected generation 1, current 3") {
		t.Fatalf("ABA stale delete should fail on generation, got %v", err)
	}
}

func TestDecodeProfileStrictRejectsUnknownFields(t *testing.T) {
	raw := []byte(`{
	  "container":"mkv",
	  "video":{"codec":"libx265","quality":24,"preset":"slow","totally_unknown":true},
	  "audio":{"mode":"copy"},
	  "subtitles":{"mode":"copy","convert_incompatible":true},
	  "preserve":{"metadata":true,"chapters":true,"attachments":true},
	  "resilience":{"max_attempts":1,"max_fallbacks":1,"fallbacks":[{"when":"container_subtitle_incompatible","action":"apply_container_conversion"}]}
	}`)
	_, _, err := DecodeProfileStrict("ephemeral", raw)
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("expected unknown-field rejection, got %v", err)
	}
}

func TestNormalizeAndDigestProfileIsStable(t *testing.T) {
	p := managedTestProfile()
	p.Container = " MKV "
	p.Video.Codec = " LIBX265 "
	a, da, err := NormalizeAndDigestProfile("anime-x265", p)
	if err != nil {
		t.Fatal(err)
	}
	b, db, err := NormalizeAndDigestProfile("anime-x265", a)
	if err != nil {
		t.Fatal(err)
	}
	if da != db {
		t.Fatalf("digest not stable: %s != %s", da, db)
	}
	if b.Container != "mkv" || b.Video.Codec != "libx265" {
		t.Fatalf("normalization failed: %+v", b.Video)
	}
}
