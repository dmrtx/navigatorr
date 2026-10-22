package store

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestKeepLanguagesRoundTrip(t *testing.T) {
	s := openTest(t)
	if row, err := s.GetKeepLanguages(); err != nil || row != nil {
		t.Fatalf("a never-configured list has no row: %+v %v", row, err)
	}
	first, err := s.SetKeepLanguages([]string{"eng", "spa"})
	if err != nil || first.Revision != 1 || first.UpdatedAt == "" || !reflect.DeepEqual(first.Languages, []string{"eng", "spa"}) {
		t.Fatalf("first save: %+v %v", first, err)
	}
	second, err := s.SetKeepLanguages([]string{"jpn"})
	if err != nil || second.Revision != 2 {
		t.Fatalf("an update counts the revision: %+v %v", second, err)
	}
	got, err := s.GetKeepLanguages()
	if err != nil || got == nil || !reflect.DeepEqual(got.Languages, []string{"jpn"}) || got.Revision != 2 {
		t.Fatalf("read back: %+v %v", got, err)
	}
}

// An empty list is a real choice (keep every track), distinct from never set.
func TestKeepLanguagesEmptyListIsStored(t *testing.T) {
	s := openTest(t)
	for _, in := range [][]string{{}, nil} {
		if _, err := s.SetKeepLanguages(in); err != nil {
			t.Fatal(err)
		}
		got, err := s.GetKeepLanguages()
		if err != nil || got == nil || got.Languages == nil || len(got.Languages) != 0 {
			t.Fatalf("the empty list must read back as a non-nil empty list: %+v %v", got, err)
		}
	}
}

func TestKeepLanguagesSurviveReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nav.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetKeepLanguages([]string{"deu", "eng"}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.GetKeepLanguages()
	if err != nil || got == nil || !reflect.DeepEqual(got.Languages, []string{"deu", "eng"}) {
		t.Fatalf("after reopen: %+v %v", got, err)
	}
}
