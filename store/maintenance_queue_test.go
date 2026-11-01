package store

import "testing"

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
	if again := addCandidate(t, s, "11"); again.ID == it.ID {
		t.Fatal("a workflow failure must not block queueing the candidate again")
	}
	if _, err := s.RestoreItem(it.ID); err == nil {
		t.Fatal("restore must refuse an item the workflow closed")
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
