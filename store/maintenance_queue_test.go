package store

import (
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func addCandidate(t *testing.T, s *Store, mediaID string) MaintenanceItem {
	t.Helper()
	it, err := s.AddItem(MaintenanceItem{MediaType: "movie", Service: "radarr", MediaID: mediaID, Title: "Film " + mediaID, IssueType: "oversized", Notes: "Candidato a transcodificar"})
	if err != nil {
		t.Fatal(err)
	}
	return it
}

func TestDismissedCandidateStaysDismissed(t *testing.T) {
	s := openTest(t)
	it := addCandidate(t, s, "10")
	dropped, err := s.DismissItem(it.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if dropped.Status != MaintFailed || dropped.ConsoleClosed() != "dismissed" {
		t.Fatalf("got %s %q", dropped.Status, dropped.ConsoleClosed())
	}
	again := addCandidate(t, s, "10")
	if again.ID != it.ID || again.Status != MaintFailed {
		t.Fatalf("re-adding a dismissed candidate returned %d %s, want the dismissed one", again.ID, again.Status)
	}
	if _, err := s.DismissItem(it.ID, ""); err != nil {
		t.Fatalf("dismissing twice must be a no-op: %v", err)
	}
}

func TestWorkflowFailureIsNotDismissal(t *testing.T) {
	s := openTest(t)
	it := addCandidate(t, s, "11")
	if _, err := s.ResolveItem(it.ID, MaintFailed, "network"); err != nil {
		t.Fatal(err)
	}
	again := addCandidate(t, s, "11")
	if again.ID == it.ID {
		t.Fatal("a workflow failure must not block queueing the candidate again")
	}
	if _, err := s.RestoreItem(it.ID); !errors.Is(err, ErrMaintConflict) {
		t.Fatalf("restoring while the same entry is queued again must be a conflict: %v", err)
	}
	if _, err := s.ResolveItem(again.ID, MaintFailed, "gone"); err != nil {
		t.Fatal(err)
	}
	// It shows under Dropped, can be restored, and can be dropped for good.
	back, err := s.RestoreItem(it.ID)
	if err != nil || back.Status != MaintPending {
		t.Fatalf("a workflow failure can be restored: %v %+v", err, back)
	}
	if _, err := s.ResolveItem(it.ID, MaintFailed, "network again"); err != nil {
		t.Fatal(err)
	}
	if dropped, err := s.DismissItem(it.ID, ""); err != nil || dropped.ConsoleClosed() != "dismissed" {
		t.Fatalf("a workflow failure can be dropped for good: %v %+v", err, dropped)
	}
}

func TestMarkInsideAModelsNoteIsNotAConsoleClose(t *testing.T) {
	s := openTest(t)
	it, err := s.AddItem(MaintenanceItem{MediaType: "movie", Service: "radarr", MediaID: "30", Title: "Film 30", IssueType: "oversized", Notes: "see " + ConsoleDismissedMark + " in the old log; next"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveItem(it.ID, MaintFailed, "see "+ConsoleDismissedMark+" in the old log; next"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetItem(it.ID)
	if got.ConsoleClosed() != "" {
		t.Fatal("words inside a model's note must not count as the owner's verdict")
	}
	if again := addCandidate(t, s, "30"); again.ID == it.ID {
		t.Fatal("such an item must not block queueing the candidate again")
	}
}

func TestConsoleRefusesWhatAWorkerOwns(t *testing.T) {
	s := openTest(t)
	busy := addCandidate(t, s, "40")
	for _, to := range []string{MaintResearching, MaintCandidate, MaintDownloading} {
		if _, err := s.Transition(busy.ID, to, ""); err != nil {
			t.Fatal(err)
		}
	}
	for _, act := range []func(int64, string) (MaintenanceItem, error){s.DismissItem, s.MarkItemProcessed} {
		if _, err := act(busy.ID, ""); !errors.Is(err, ErrMaintConflict) {
			t.Fatalf("an item being downloaded must be refused: %v", err)
		}
	}
	claimed := addCandidate(t, s, "41")
	if _, err := s.ClaimItem(claimed.ID, "agent", time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DismissItem(claimed.ID, ""); !errors.Is(err, ErrMaintConflict) {
		t.Fatalf("an item another agent holds must be refused: %v", err)
	}
	if _, err := s.DismissItem(99999, ""); !errors.Is(err, ErrMaintNotFound) {
		t.Fatalf("unknown id: %v", err)
	}
}

func TestNotesDoNotGrowAcrossCloseAndRestore(t *testing.T) {
	s := openTest(t)
	it := addCandidate(t, s, "50")
	for i := 0; i < 5; i++ {
		if _, err := s.DismissItem(it.ID, "no\x00 me\ninteresa; nunca"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.RestoreItem(it.ID); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := s.GetItem(it.ID)
	if got.Notes != "Candidato a transcodificar" {
		t.Fatalf("notes after five cycles: %q", got.Notes)
	}
	long := strings.Repeat("é", 2000)
	closed, err := s.DismissItem(it.ID, long)
	if err != nil || utf8.RuneCountInString(closed.Notes) > 600 || strings.ContainsAny(closed.Notes, "\n\x00") {
		t.Fatalf("note must be one short printable line: %v %d", err, utf8.RuneCountInString(closed.Notes))
	}
}

func TestMarkProcessedAndRestore(t *testing.T) {
	s := openTest(t)
	it := addCandidate(t, s, "12")
	done, err := s.MarkItemProcessed(it.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != MaintDone || done.ConsoleClosed() != "processed" {
		t.Fatalf("got %s %q", done.Status, done.ConsoleClosed())
	}
	back, err := s.RestoreItem(it.ID)
	if err != nil || back.Status != MaintPending || back.ConsoleClosed() != "" {
		t.Fatalf("restore: %v %+v", err, back)
	}
	if _, err := s.RestoreItem(it.ID); err != nil {
		t.Fatalf("restoring an open item must be a no-op: %v", err)
	}
	// A done item the workflow closed stays immutable.
	wf := addCandidate(t, s, "13")
	if _, err := s.db.Exec(`UPDATE maintenance_items SET status='done' WHERE id=?`, wf.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkItemProcessed(wf.ID, ""); err == nil {
		t.Fatal("a done item cannot be marked again")
	}
	if _, err := s.RestoreItem(wf.ID); err == nil {
		t.Fatal("restore must refuse a done item the workflow closed")
	}
}

func TestListAndCountForConsole(t *testing.T) {
	s := openTest(t)
	a, b, c := addCandidate(t, s, "20"), addCandidate(t, s, "21"), addCandidate(t, s, "22")
	if _, err := s.DismissItem(a.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkItemProcessed(b.ID, ""); err != nil {
		t.Fatal(err)
	}
	counts, err := s.CountItemsForConsole("oversized")
	if err != nil || counts != (MaintenanceCounts{Open: 1, Processed: 1, Dismissed: 1}) {
		t.Fatalf("counts %+v %v", counts, err)
	}
	open, err := s.ListItemsForConsole([]string{MaintPending}, "oversized", 0)
	if err != nil || len(open) != 1 || open[0].ID != c.ID {
		t.Fatalf("open %+v %v", open, err)
	}
	if _, err := s.ListItemsForConsole([]string{"bogus"}, "", 0); err == nil {
		t.Fatal("unknown status must be refused")
	}
}
