package store

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestWorkerWindowRevisionAndRoundTrip(t *testing.T) {
	s := openTest(t)
	if _, ok, err := s.GetWorkerWindow("m4"); err != nil || ok {
		t.Fatal(ok, err)
	}
	w, err := s.SetWorkerWindow(WorkerWindow{Worker: "m4", Until: "2030-01-01T08:00:00+02:00", MarginSec: 600, MediaTypes: []string{"Movie", "tv", "movie"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if w.Revision != 1 || w.Until != "2030-01-01T06:00:00Z" || !reflect.DeepEqual(w.MediaTypes, []string{"tv", "movie"}) {
		t.Fatalf("%+v", w)
	}
	if closes, ok := w.ClosesAt(); !ok || closes != time.Date(2030, 1, 1, 5, 50, 0, 0, time.UTC) {
		t.Fatal(closes, ok)
	}
	// an identical write changes nothing
	if again, err := s.SetWorkerWindow(WorkerWindow{Worker: "m4", Until: "2030-01-01T06:00:00Z", MarginSec: 600, MediaTypes: []string{"tv", "movie"}}, nil); err != nil || again.Revision != 1 {
		t.Fatal(again, err)
	}
	// a change moves the revision
	w2, err := s.SetWorkerWindow(WorkerWindow{Worker: "m4", MarginSec: 0}, ptr64(1))
	if err != nil || w2.Revision != 2 || w2.Until != "" || len(w2.MediaTypes) != 0 {
		t.Fatalf("%+v %v", w2, err)
	}
	if _, ok := w2.ClosesAt(); ok {
		t.Fatal("no until, no closing time")
	}
	// a stale revision is refused and changes nothing
	var stale *StaleWorkerWindowError
	if _, err := s.SetWorkerWindow(WorkerWindow{Worker: "m4", MarginSec: 5}, ptr64(1)); !errors.As(err, &stale) || stale.Current != 2 {
		t.Fatal(err)
	}
	if _, err := s.DeleteWorkerWindow("m4", ptr64(1)); !errors.As(err, &stale) {
		t.Fatal(err)
	}
	if got, _, _ := s.GetWorkerWindow("m4"); got.MarginSec != 0 || got.Revision != 2 {
		t.Fatalf("%+v", got)
	}
	// revision 0 means "it has none": only valid when there is no window
	if _, err := s.SetWorkerWindow(WorkerWindow{Worker: "m1"}, ptr64(3)); !errors.As(err, &stale) {
		t.Fatal(err)
	}
	if _, err := s.SetWorkerWindow(WorkerWindow{Worker: "m1"}, ptr64(0)); err != nil {
		t.Fatal(err)
	}
	all, err := s.ListWorkerWindows()
	if err != nil || len(all) != 2 {
		t.Fatal(all, err)
	}
	if had, err := s.DeleteWorkerWindow("m4", ptr64(2)); err != nil || !had {
		t.Fatal(had, err)
	}
	if had, err := s.DeleteWorkerWindow("m4", nil); err != nil || had {
		t.Fatal(had, err)
	}
	// a window deleted and written again starts over
	if w, err := s.SetWorkerWindow(WorkerWindow{Worker: "m4"}, nil); err != nil || w.Revision != 1 {
		t.Fatal(w, err)
	}
}

func TestWorkerWindowValidation(t *testing.T) {
	s := openTest(t)
	for name, w := range map[string]WorkerWindow{
		"no worker":  {},
		"bad type":   {Worker: "m4", MediaTypes: []string{"music"}},
		"neg margin": {Worker: "m4", MarginSec: -1},
		"bad until":  {Worker: "m4", Until: "soon"},
	} {
		if _, err := s.SetWorkerWindow(w, nil); err == nil {
			t.Fatal(name, "should be refused")
		}
	}
	// an unreadable stored until closes the node
	if closes, ok := (WorkerWindow{Until: "garbage"}).ClosesAt(); !ok || closes.After(time.Unix(1, 0)) {
		t.Fatal(closes, ok)
	}
}

func ptr64(n int64) *int64 { return &n }
