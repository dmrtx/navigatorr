package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestActionLeaseAcrossConnectionsExpiresAndFencesOldOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shared.db")
	a, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	inst := ActionInstance{ID: "shared-action", ActionName: "transcode_media", Status: ActionStatusWaitingExternal}
	if err := a.CreateActionInstance(inst); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if ok, err := a.ClaimActionExecution(inst.ID, "first", now, time.Minute); err != nil || !ok {
		t.Fatalf("first claim: %v %v", ok, err)
	}
	if ok, err := b.ClaimActionExecution(inst.ID, "second", now, time.Minute); err != nil || ok {
		t.Fatalf("live claim was stolen: %v %v", ok, err)
	}
	if err := b.ReleaseActionExecution(inst.ID, "second"); err != nil {
		t.Fatal(err)
	}
	if ok, err := a.RenewActionExecution(inst.ID, "first", now, time.Minute); err != nil || !ok {
		t.Fatalf("owner renewal failed: %v %v", ok, err)
	}
	if ok, err := b.ClaimActionExecution(inst.ID, "second", now.Add(2*time.Minute), time.Minute); err != nil || !ok {
		t.Fatalf("crashed owner not recovered: %v %v", ok, err)
	}
	inst.Status = ActionStatusCompleted
	if err := a.UpdateClaimedActionInstance(inst, "first"); err == nil {
		t.Fatal("expired owner overwrote new executor")
	}
	if err := b.UpdateClaimedActionInstance(inst, "second"); err != nil {
		t.Fatal(err)
	}
	stored, err := b.GetActionInstance(inst.ID)
	if err != nil || stored.Status != ActionStatusCompleted {
		t.Fatalf("new owner did not commit: %+v %v", stored, err)
	}
}
