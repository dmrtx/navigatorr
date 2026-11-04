package maintenanceui

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/store"
)

func memoInstances(n int) []store.ActionInstance {
	instances := make([]store.ActionInstance, n)
	for i := range instances {
		instances[i] = store.ActionInstance{
			ID:          fmt.Sprintf("a-%d", i),
			ActionName:  "transcode_media",
			Status:      "completed",
			InputsJSON:  fmt.Sprintf(`{"n":%d}`, i),
			OutputsJSON: `{"saved":true}`,
			StateJSON:   fmt.Sprintf(`{"path":"/m/%d.mkv"}`, i),
			Rev:         int64(i + 1),
		}
	}
	return append(instances, store.ActionInstance{ID: "broken", ActionName: "transcode_media", Status: "completed", InputsJSON: `not json`, OutputsJSON: ``, StateJSON: `[1]`, Rev: 1})
}

func sameMap(a, b map[string]any) bool {
	return reflect.ValueOf(a).Pointer() == reflect.ValueOf(b).Pointer()
}

func TestRecordMemoKeepsOrderAndContent(t *testing.T) {
	now := time.Now()
	for _, n := range []int{0, 1, 63, 64, 65, 1000} {
		instances := memoInstances(n)
		var m recordMemo
		for pass := 0; pass < 2; pass++ {
			got := m.records(instances, now)
			if len(got) != len(instances) {
				t.Fatalf("n=%d: %d records for %d instances", n, len(got), len(instances))
			}
			for i, inst := range instances {
				want := operationRecord{inst: inst, inputs: decodeOperationJSON(inst.InputsJSON), outputs: decodeOperationJSON(inst.OutputsJSON), state: decodeOperationJSON(inst.StateJSON)}
				want.savings = projectOperationSavings(want, now)
				if !reflect.DeepEqual(got[i], want) {
					t.Fatalf("n=%d pass %d record %d differs: %v vs %v", n, pass, i, got[i], want)
				}
			}
		}
	}
}

func TestRecordMemoDecodesOnlyWhatWasWritten(t *testing.T) {
	now := time.Now()
	instances := memoInstances(10)
	var m recordMemo
	first := m.records(instances, now)

	// Same revisions: nothing is decoded again, the documents are the ones already held.
	again := m.records(instances, now)
	for i := range first {
		if !sameMap(first[i].state, again[i].state) || !sameMap(first[i].inputs, again[i].inputs) {
			t.Fatalf("record %d was decoded again though it did not change", i)
		}
	}

	// One action written (new revision, new content), one removed.
	changed := append([]store.ActionInstance(nil), instances...)
	changed[3].Rev++
	changed[3].StateJSON = `{"path":"/m/new.mkv"}`
	changed = append(changed[:5], changed[6:]...)
	after := m.records(changed, now)
	if after[3].state["path"] != "/m/new.mkv" {
		t.Fatal("a written action must be read again", after[3].state)
	}
	if !sameMap(after[2].state, first[2].state) {
		t.Fatal("an untouched action must keep its decoded documents")
	}
	if _, kept := m.byID["a-5"]; kept {
		t.Fatal("a removed action must be forgotten")
	}
}

func TestRecordMemoNeverKeepsWhatHasNoRevision(t *testing.T) {
	now := time.Now()
	instances := memoInstances(3)
	for i := range instances {
		instances[i].Rev = 0
	}
	var m recordMemo
	m.records(instances, now)
	instances[1].StateJSON = `{"path":"/m/other.mkv"}`
	if got := m.records(instances, now); got[1].state["path"] != "/m/other.mkv" {
		t.Fatal("a row read without a revision must always be read again", got[1].state)
	}
}

func TestRecordMemoServesConcurrentRequests(t *testing.T) {
	now := time.Now()
	instances := memoInstances(200)
	var m recordMemo
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for round := 0; round < 20; round++ {
				seen := append([]store.ActionInstance(nil), instances...)
				seen[round].Rev += int64(round + 1)
				for i, r := range m.records(seen, now) {
					if i < 200 && (r.inst.ID != seen[i].ID || r.inputs["n"] != float64(i) || r.state["path"] != fmt.Sprintf("/m/%d.mkv", i)) {
						t.Errorf("record %d answered with another action's content: %v %v", i, r.inst.ID, r.state)
						return
					}
				}
			}
		}()
	}
	wg.Wait()
}

// The answer of a server that has memoised its records is the answer of one that has not.
func TestOperationsAnswerDoesNotDependOnTheMemo(t *testing.T) {
	s, h := testUI(t)
	seedActionableFiles(t, s)
	read := func() map[string]any {
		t.Helper()
		w := request(h, "GET", "/api/maintenance/operations?limit=50", "", true)
		var page map[string]any
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil {
			t.Fatal(w.Code, w.Body.String())
		}
		delete(page, "observed_at")
		withoutClockFields(page)
		return page
	}
	read()
	warm := read()
	st := s.engine.Deps().Store
	child, _ := st.GetActionInstance("ready")
	child.StateJSON = `{"reason_code":"changed_after_the_first_read"}`
	if err := st.UpdateActionInstance(*child); err != nil {
		t.Fatal(err)
	}
	updated := read()
	if reflect.DeepEqual(warm, updated) {
		t.Fatal("a write must show in the next answer")
	}
	s.recordMemo = recordMemo{}
	if fresh := read(); !reflect.DeepEqual(updated, fresh) {
		t.Fatal("the memoised answer differs from a cold one")
	}
}

// withoutClockFields drops the durations measured against the clock, which differ between any two reads.
func withoutClockFields(v any) {
	switch v := v.(type) {
	case map[string]any:
		delete(v, "duration_ms")
		delete(v, "wall_duration_ms")
		for _, child := range v {
			withoutClockFields(child)
		}
	case []any:
		for _, child := range v {
			withoutClockFields(child)
		}
	}
}

// Deleting an action and inserting another under its id starts the revision at 1 again; the memo must not
// hand the old action's documents to the new one.
func TestRecordMemoDoesNotServeADeletedActionsRecordToItsIDReused(t *testing.T) {
	now := time.Now()
	instances := memoInstances(3)
	for i := range instances {
		instances[i].CreatedAt = "2026-11-01T10:00:00Z"
	}
	var m recordMemo
	m.records(instances, now)
	reused := append([]store.ActionInstance(nil), instances...)
	reused[1].CreatedAt = "2026-11-04T10:00:00Z"
	reused[1].StateJSON = `{"path":"/m/new.mkv"}`
	if got := m.records(reused, now); got[1].state["path"] != "/m/new.mkv" {
		t.Fatal("an id reused after a delete must be read again", got[1].state)
	}
}

// Running actions go through the memo like the rest; their clock-bound estimate is made again after it.
func TestRecordMemoServesRunningActionsToo(t *testing.T) {
	now := time.Now()
	instances := memoInstances(2)
	instances[0].Status = "running"
	var m recordMemo
	first := m.records(instances, now)
	later := m.records(instances, now.Add(10*time.Minute))
	if first[0].inst.Status != "running" || later[0].inst.Status != "running" || later[1].state["path"] != "/m/1.mkv" {
		t.Fatal("running records are served from the memo", later[0].inst.Status)
	}
}
