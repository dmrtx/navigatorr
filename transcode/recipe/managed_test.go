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
		Subtitles: SubtitleProfile{Mode: "copy", ConvertIncompatible: true},
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
	rec1, err := m.SaveManagedProfile("anime-x265", managedTestProfile(), "first", "act-1", "")
	if err != nil {
		t.Fatal(err)
	}
	if rec1.Generation != 1 || rec1.Digest == "" {
		t.Fatalf("unexpected first record: %+v", rec1)
	}

	p2 := managedTestProfile()
	p2.Video.Quality = 22
	rec2, err := m.SaveManagedProfile("anime-x265", p2, "second", "act-2", rec1.Digest)
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

	if _, err := reopened.DeleteManagedProfile("anime-x265", rec1.Digest); err == nil {
		t.Fatal("expected optimistic concurrency mismatch")
	}
	deleted, err := reopened.DeleteManagedProfile("anime-x265", rec2.Digest)
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
