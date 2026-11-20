package store

import (
	"database/sql"
	"errors"
	"path/filepath"
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
	// a delete moves the revision on and a re-created window continues from
	// it, so a client holding an old revision cannot write over it
	if rev, err := s.WorkerWindowRevision("m4"); err != nil || rev != 3 {
		t.Fatal("revision of a deleted window", rev, err)
	}
	if _, err := s.SetWorkerWindow(WorkerWindow{Worker: "m4"}, ptr64(2)); !errors.As(err, &stale) || stale.Current != 3 {
		t.Fatal("a client holding the deleted window's revision wrote a new one", err)
	}
	w3, err := s.SetWorkerWindow(WorkerWindow{Worker: "m4", MarginSec: 60}, ptr64(3))
	if err != nil || w3.Revision != 4 {
		t.Fatal(w3, err)
	}
	if _, err := s.DeleteWorkerWindow("m4", ptr64(2)); !errors.As(err, &stale) {
		t.Fatal("a stale delete removed the new window", err)
	}
	if _, ok, _ := s.GetWorkerWindow("m4"); !ok {
		t.Fatal("the new window is gone")
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

// A blank media type is an error, not a dropped entry.
func TestWorkerWindowBlankMediaTypeIsRejected(t *testing.T) {
	s := openTest(t)
	for _, types := range [][]string{{""}, {"movie", "  "}} {
		if _, err := s.SetWorkerWindow(WorkerWindow{Worker: "m4", MediaTypes: types}, nil); err == nil {
			t.Fatalf("%q accepted", types)
		}
	}
}

// The counter survives deleting the window, so a stale holder of an old
// revision never matches a window created later.
func TestWorkerWindowRevisionCounterSurvivesDeletion(t *testing.T) {
	s := openTest(t)
	if r, err := s.WorkerWindowRevision("m4"); err != nil || r != 0 {
		t.Fatal(r, err)
	}
	w, _ := s.SetWorkerWindow(WorkerWindow{Worker: "m4", MarginSec: 1}, nil)
	if w.Revision != 1 {
		t.Fatal(w.Revision)
	}
	if had, err := s.DeleteWorkerWindow("m4", ptr64(1)); err != nil || !had {
		t.Fatal(had, err)
	}
	if r, _ := s.WorkerWindowRevision("m4"); r != 2 {
		t.Fatalf("counter after delete: %d", r)
	}
	// Absent window: the expected revision is the counter.
	if _, err := s.SetWorkerWindow(WorkerWindow{Worker: "m4", MarginSec: 2}, ptr64(1)); err == nil {
		t.Fatal("a stale revision created a window")
	}
	if w, err := s.SetWorkerWindow(WorkerWindow{Worker: "m4", MarginSec: 2}, ptr64(2)); err != nil || w.Revision != 3 {
		t.Fatal(w, err)
	}
}

// The waiter query reads the same source as the claims: the preflight's
// original when it has both numbers, else the early check's queue_probe.
func TestWaitersReadOriginalThenQueueProbe(t *testing.T) {
	s := openTest(t)
	conv := func(id, state string) {
		if err := s.CreateActionInstance(ActionInstance{ID: id, ActionName: "transcode_media", Status: ActionStatusWaitingExternal, InputsJSON: `{}`, StateJSON: state}); err != nil {
			t.Fatal(err)
		}
	}
	conv("both", `{"transcode_queue_order":"1|a","original":{"duration_sec":900,"resolution":"3840x2160"},"queue_probe":{"duration_sec":1,"width":10,"height":10}}`)
	conv("probe", `{"transcode_queue_order":"1|b","queue_probe":{"duration_sec":600,"width":1920,"height":1080}}`)
	conv("half", `{"transcode_queue_order":"1|c","original":{"duration_sec":900},"queue_probe":{"duration_sec":600,"width":1280,"height":720}}`)
	ws, err := s.ListTranscodeQueueWaiters()
	if err != nil {
		t.Fatal(err)
	}
	type p struct {
		d    float64
		w, h int
	}
	got := map[string]p{}
	for _, w := range ws {
		got[w.ID] = p{w.ProbeDurationSec, w.ProbeWidth, w.ProbeHeight}
	}
	if got["both"] != (p{900, 3840, 2160}) || got["probe"] != (p{600, 1920, 1080}) || got["half"] != (p{600, 1280, 720}) {
		t.Fatalf("%+v", got)
	}
}

// A database that was already at version 27 gets the revision counter table
// from migration 28, and deleting a window works there.
func TestWindowRevisionTableComesFromMigration28(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v27.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`DROP TABLE worker_window_revisions`, `DROP TABLE worker_windows`, `DELETE FROM schema_version WHERE version >= 28`} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(q, err)
		}
	}
	db.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='worker_window_revisions'`).Scan(&n); err != nil || n != 1 {
		t.Fatal("table missing after migrating from 27", n, err)
	}
	if _, err := s.SetWorkerWindow(WorkerWindow{Worker: "m4", MarginSec: 1}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteWorkerWindow("m4", nil); err != nil {
		t.Fatal(err)
	}
	if r, _ := s.WorkerWindowRevision("m4"); r != 2 {
		t.Fatalf("counter %d", r)
	}
}

// A missing counter row cannot skip the bump of a delete.
func TestDeleteWorkerWindowBumpsEvenWithoutACounterRow(t *testing.T) {
	s := openTest(t)
	if _, err := s.SetWorkerWindow(WorkerWindow{Worker: "m4", MarginSec: 1}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DELETE FROM worker_window_revisions`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE worker_windows SET revision=5 WHERE worker='m4'`); err != nil {
		t.Fatal(err)
	}
	if had, err := s.DeleteWorkerWindow("m4", nil); err != nil || !had {
		t.Fatal(had, err)
	}
	if r, _ := s.WorkerWindowRevision("m4"); r != 6 {
		t.Fatalf("counter %d after deleting revision 5, want 6", r)
	}
}
