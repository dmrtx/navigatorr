package store

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestMaintenanceArchivePersistsWithoutChangingActions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	for _, row := range []ActionInstance{{ID: "batch", ActionName: "transcode_batch", Status: ActionStatusCompleted}, {ID: "child", ActionName: "transcode_media", Status: ActionStatusFailed}} {
		if err := s.CreateActionInstance(row); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetMaintenanceArchived("batch", []string{"batch", "child"}, true); err != nil {
		t.Fatal(err)
	}
	first, _ := s.MaintenanceArchives()
	if first["batch"] == "" {
		t.Fatal("archive not recorded")
	}
	if err := s.SetMaintenanceArchived("batch", []string{"batch", "child"}, true); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := s.MaintenanceArchives()
	if got["batch"] != first["batch"] {
		t.Fatal("archive did not survive reopening")
	}
	child, _ := s.GetActionInstance("child")
	if child.Status != ActionStatusFailed {
		t.Fatal("archive changed action state")
	}
	child.Status = ActionStatusWaitingExternal
	if err := s.UpdateActionInstance(*child); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMaintenanceArchived("batch", []string{"batch", "child"}, true); !errors.Is(err, ErrArchiveActiveWorkflow) {
		t.Fatal("active child accepted", err)
	}
	if err := s.SetMaintenanceArchived("batch", nil, false); err != nil {
		t.Fatal(err)
	}
	got, _ = s.MaintenanceArchives()
	if len(got) != 0 {
		t.Fatal("restore did not clear archive")
	}
}

func TestArchiveRefusesAnActionNamedByKeyThatIsStillActive(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, row := range []ActionInstance{
		{ID: "preview", ActionName: "transcode_batch", Status: ActionStatusCompleted},
		{ID: "run", ActionName: "transcode_batch", Status: ActionStatusRunning, IdempotencyKey: "web-preview:preview"},
	} {
		if err := s.CreateActionInstance(row); err != nil {
			t.Fatal(err)
		}
	}
	keys := []ActionKey{{"transcode_batch", "web-preview:preview"}}
	// The member list is the stale one from before the run existed.
	if err := s.SetMaintenanceArchivedWith("preview", []string{"preview"}, true, keys); !errors.Is(err, ErrArchiveActiveWorkflow) {
		t.Fatal("a running action named by key was ignored", err)
	}
	if got, _ := s.MaintenanceArchives(); len(got) != 0 {
		t.Fatal("a refused archive left a row", got)
	}
	run, _ := s.GetActionInstance("run")
	run.Status = ActionStatusCompleted
	if err := s.UpdateActionInstance(*run); err != nil {
		t.Fatal(err)
	}
	// Another type of action under the same key, running, is not the batch: it must not block.
	if err := s.CreateActionInstance(ActionInstance{ID: "other", ActionName: "promote_files", Status: ActionStatusRunning, IdempotencyKey: "web-preview:preview"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMaintenanceArchivedWith("preview", []string{"preview"}, true, keys); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMaintenanceArchivedWith("preview", []string{"preview"}, true, []ActionKey{{"transcode_batch", "web-preview:none"}}); err != nil {
		t.Fatal("a key with no action must not block", err)
	}
	if err := s.SetMaintenanceArchivedWith("preview", []string{"preview"}, true, []ActionKey{{"promote_files", "web-preview:preview"}}); !errors.Is(err, ErrArchiveActiveWorkflow) {
		t.Fatal("the key is matched together with the action type", err)
	}
}
