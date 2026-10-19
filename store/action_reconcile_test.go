package store

import (
	"path/filepath"
	"sync"
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

func TestPromotionSeriesReservationAcrossConnectionsAndTerminalStates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "promotions.db")
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
	for _, id := range []string{"first", "second", "other-series"} {
		if err := a.CreateActionInstance(ActionInstance{ID: id, ActionName: "promote_transcode_candidate", Status: ActionStatusWaitingExternal}); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	wins := make(chan string, 2)
	for i, s := range []*Store{a, b} {
		id := []string{"first", "second"}[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := s.ClaimPromotionSeries("Sonarr", 218, id)
			if err != nil {
				t.Error(err)
			}
			if ok {
				wins <- id
			}
		}()
	}
	wg.Wait()
	close(wins)
	winner := ""
	for id := range wins {
		if winner != "" {
			t.Fatal("concurrent reservations both succeeded")
		}
		winner = id
	}
	if winner == "" {
		t.Fatal("no owner acquired reservation")
	}
	loser := "first"
	if winner == loser {
		loser = "second"
	}
	if ok, err := b.ClaimPromotionSeries("sonarr", 219, "other-series"); err != nil || !ok {
		t.Fatalf("unrelated series blocked: %v %v", ok, err)
	}
	for _, status := range []string{ActionStatusWaitingExternal, ActionStatusFailed, ActionStatusCancelled} {
		inst, _ := a.GetActionInstance(winner)
		inst.Status = status
		if err := a.UpdateActionInstance(*inst); err != nil {
			t.Fatal(err)
		}
		if ok, err := b.ClaimPromotionSeries("sonarr", 218, loser); err != nil || ok {
			t.Fatalf("unsafe owner takeover in %s: %v %v", status, ok, err)
		}
		if ok, err := b.ClaimPromotionSeries("sonarr", 218, winner); err != nil || !ok {
			t.Fatalf("owner lost reservation: %v %v", ok, err)
		}
	}
	inst, _ := a.GetActionInstance(winner)
	inst.Status = ActionStatusCompleted
	if err := a.UpdateActionInstance(*inst); err != nil {
		t.Fatal(err)
	}
	if ok, err := b.ClaimPromotionSeries("sonarr", 218, loser); err != nil || !ok {
		t.Fatalf("completed owner retained reservation: %v %v", ok, err)
	}
}

func TestPromotionSeriesReservationReleasesOnlyBeforeFirstImport(t *testing.T) {
	for _, status := range []string{ActionStatusFailed, ActionStatusCancelled} {
		for _, state := range []string{
			`{"promotion":{"approved":true}}`,
			`{"promotion":{"approved":true,"commands":{"import":{"sent_at":"accepted-or-uncertain"}}}}`,
			`{"promotion":{"approved":true},"promotion_reimport_history":[{"previous_import":{"done":true}}]}`,
			`{"promotion":{"approved":true},"filesystem_publish_started":true}`,
			`{"promotion":{"approved":true},"filesystem_published":true}`,
		} {
			s, err := Open(filepath.Join(t.TempDir(), "reservation.db"))
			if err != nil {
				t.Fatal(err)
			}
			for _, id := range []string{"owner", "next"} {
				if err := s.CreateActionInstance(ActionInstance{ID: id, ActionName: "promote_transcode_candidate", Status: ActionStatusWaitingExternal}); err != nil {
					t.Fatal(err)
				}
			}
			if ok, err := s.ClaimPromotionSeries("sonarr", 1, "owner"); err != nil || !ok {
				t.Fatalf("initial claim: %v %v", ok, err)
			}
			inst, _ := s.GetActionInstance("owner")
			inst.Status, inst.StateJSON = status, state
			if err := s.UpdateActionInstance(*inst); err != nil {
				t.Fatal(err)
			}
			ok, err := s.ClaimPromotionSeries("sonarr", 1, "next")
			if err != nil || ok != (state == `{"promotion":{"approved":true}}`) {
				t.Fatalf("unsafe terminal takeover: status=%s state=%s claimed=%v err=%v", status, state, ok, err)
			}
			s.Close()
		}
	}
}

func TestPromotionSeriesTakeoverFromTerminalBatch(t *testing.T) {
	const (
		clean      = `{"promotion":{"approved":true}}`
		unapproved = `{}`
		imported   = `{"promotion":{"approved":true,"commands":{"import":{"sent_at":"accepted-or-uncertain"}}}}`
		deleted    = `{"promotion":{"approved":true,"delete_sent_at":"sent"}}`
		reimport   = `{"promotion":{"approved":true},"promotion_reimport_history":[{"previous_import":{"done":true}}]}`
	)
	type child struct{ status, state string }
	const (
		rescanSent   = `{"batch_promotion_commands":{"rescan":{"sent_at":"t"}}}`
		rescanIDOnly = `{"batch_promotion_commands":{"rescan":{"sent_at":"t","id":9}}}`
		rescanDone   = `{"batch_promotion_commands":{"rescan":{"sent_at":"t","id":9,"done":true}}}`
		rescanFailed = `{"batch_promotion_commands":{"rescan":{"sent_at":"t","id":9,"failed":true}}}`
		rescanUnsent = `{"batch_promotion_commands":{"rescan":{}}}`
	)
	for _, tc := range []struct {
		name        string
		batchStatus string
		batchState  string
		children    []child
		want        bool
	}{
		{"rescan sent, outcome unknown", ActionStatusCancelled, rescanSent, []child{{ActionStatusCompleted, imported}}, false},
		{"rescan accepted, still running", ActionStatusCancelled, rescanIDOnly, []child{{ActionStatusCompleted, imported}}, false},
		{"rescan sent in failed batch", ActionStatusFailed, rescanSent, []child{{ActionStatusCompleted, imported}}, false},
		{"rescan done", ActionStatusCancelled, rescanDone, []child{{ActionStatusCompleted, imported}}, true},
		{"rescan confirmed failed", ActionStatusFailed, rescanFailed, []child{{ActionStatusCompleted, imported}}, true},
		{"rescan never sent", ActionStatusCancelled, rescanUnsent, []child{{ActionStatusCompleted, imported}}, true},
		{"batch state unreadable", ActionStatusCancelled, `not json`, []child{{ActionStatusCompleted, imported}}, false},
		{"cancelled batch, all children completed", ActionStatusCancelled, "", []child{{ActionStatusCompleted, imported}, {ActionStatusCompleted, deleted}}, true},
		{"failed batch, all children completed", ActionStatusFailed, "", []child{{ActionStatusCompleted, imported}}, true},
		{"completed batch", ActionStatusCompleted, "", []child{{ActionStatusCompleted, imported}}, true},
		{"cancelled batch without children", ActionStatusCancelled, "", nil, true},
		{"children stopped before any side effect", ActionStatusCancelled, "", []child{{ActionStatusCompleted, imported}, {ActionStatusCancelled, clean}, {ActionStatusFailed, unapproved}}, true},
		{"child cancelled after import", ActionStatusCancelled, "", []child{{ActionStatusCompleted, imported}, {ActionStatusCancelled, imported}}, false},
		{"child failed after delete", ActionStatusCancelled, "", []child{{ActionStatusFailed, deleted}}, false},
		{"child failed with reimport history", ActionStatusFailed, "", []child{{ActionStatusFailed, reimport}}, false},
		{"child still waiting", ActionStatusCancelled, "", []child{{ActionStatusCompleted, imported}, {ActionStatusWaitingExternal, clean}}, false},
		{"child still running", ActionStatusCancelled, "", []child{{ActionStatusRunning, clean}}, false},
		{"batch still running", ActionStatusRunning, "", []child{{ActionStatusCompleted, imported}}, false},
		{"batch waiting", ActionStatusWaitingExternal, "", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := Open(filepath.Join(t.TempDir(), "batch.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			must := func(inst ActionInstance) {
				t.Helper()
				if err := s.CreateActionInstance(inst); err != nil {
					t.Fatal(err)
				}
			}
			must(ActionInstance{ID: "batch", ActionName: "transcode_batch", Status: ActionStatusWaitingExternal})
			must(ActionInstance{ID: "standalone", ActionName: "promote_transcode_candidate", Status: ActionStatusWaitingExternal})
			// Children of another batch never influence this reservation.
			must(ActionInstance{ID: "unrelated", ActionName: "promote_transcode_candidate", Status: ActionStatusWaitingExternal,
				InputsJSON: `{"batch_promote_parent_id":"other-batch"}`})
			for i, c := range tc.children {
				must(ActionInstance{ID: "child-" + string(rune('a'+i)), ActionName: "promote_transcode_candidate",
					Status: c.status, StateJSON: c.state, InputsJSON: `{"batch_promote_parent_id":"batch"}`})
			}
			if ok, err := s.ClaimPromotionSeries("sonarr", 7, "batch"); err != nil || !ok {
				t.Fatalf("batch claim: %v %v", ok, err)
			}
			batch, _ := s.GetActionInstance("batch")
			batch.Status, batch.StateJSON = tc.batchStatus, tc.batchState
			if batch.StateJSON == "" {
				batch.StateJSON = "{}"
			}
			if err := s.UpdateActionInstance(*batch); err != nil {
				t.Fatal(err)
			}
			ok, err := s.ClaimPromotionSeries("sonarr", 7, "standalone")
			if err != nil || ok != tc.want {
				t.Fatalf("takeover = %v (err %v), want %v", ok, err, tc.want)
			}
			owner, err := s.PromotionSeriesOwner("sonarr", 7)
			if err != nil {
				t.Fatal(err)
			}
			wantOwner := "batch"
			if tc.want {
				wantOwner = "standalone"
			}
			if owner.ActionID != wantOwner {
				t.Fatalf("owner = %+v, want %s", owner, wantOwner)
			}
			if !tc.want && (owner.ActionName != "transcode_batch" || owner.Status != tc.batchStatus) {
				t.Fatalf("owner details = %+v", owner)
			}
		})
	}
}
