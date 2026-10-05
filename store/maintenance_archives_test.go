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
