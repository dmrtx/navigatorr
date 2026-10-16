package recipe

import (
	"os"
	"testing"
)

func TestJoyManagedProfilePersistenceDoesNotChangeExistingProfile(t *testing.T) {
	dir := t.TempDir()
	m, err := NewManager(BuiltinProvider{}, dir, "", "")
	if err != nil {
		t.Fatal(err)
	}
	existing, err := m.SaveManagedProfile("existing", managedTestProfile(), "existing", "", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"joy", "joy-1600"} {
		file := "../../docs/examples/transcode-profile-" + name + ".json"
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		profile, digest, err := DecodeProfileStrict(name, b)
		if err != nil {
			t.Fatal(err)
		}
		rec, err := m.SaveManagedProfile(name, profile, "explicit experiment", "", 0, "")
		if err != nil {
			t.Fatal(err)
		}
		if rec.Digest != digest {
			t.Fatal("save changed Joy recipe")
		}
	}
	m, err = NewManager(BuiltinProvider{}, dir, "", "")
	if err != nil {
		t.Fatal(err)
	}
	got, ok, err := m.GetManagedProfile("joy-1600")
	if err != nil || !ok || got.Profile.Video.X265 == nil || got.Profile.Video.X265.BitrateKbps != 1600 || got.Profile.Audio.Mode != "he_aac" {
		t.Fatalf("Joy profile lost after restart: %+v %v", got, err)
	}
	old, ok, err := m.GetManagedProfile("existing")
	if err != nil || !ok || old.Digest != existing.Digest || old.Generation != existing.Generation {
		t.Fatal("existing profile was changed")
	}
}
