package maintenanceui

import (
	"runtime"
	"sync"
	"time"

	"github.com/jakenesler/navigatorr/store"
)

// recordMemo keeps what a request derives from each action (its three decoded JSON documents and its
// savings) between requests, labelled by the revision of the row it came from. The ledger holds
// thousands of finished actions with tens of kilobytes of state each, and decoding them again for every
// request was most of what a request cost; now only the actions written since the last request are
// decoded. The decoded maps are shared by every request and are only read.
type recordMemo struct {
	mu   sync.Mutex
	byID map[string]memoRecord
}

type memoRecord struct {
	rev int64
	rec operationRecord
}

// sameAction tells apart two rows that carry the same id and revision. Deleting an action and inserting
// another under its id starts the revision at 1 again, so the revision alone would serve the old one.
func sameAction(a, b store.ActionInstance) bool {
	return a.CreatedAt == b.CreatedAt && a.IdempotencyKey == b.IdempotencyKey && a.ActionName == b.ActionName
}

// records returns one record per instance, in the order of `instances`.
func (m *recordMemo) records(instances []store.ActionInstance, now time.Time) []operationRecord {
	out := make([]operationRecord, len(instances))
	var missing []int
	m.mu.Lock()
	for i, inst := range instances {
		if e, ok := m.byID[inst.ID]; ok && inst.Rev != 0 && e.rev == inst.Rev && sameAction(e.rec.inst, inst) {
			out[i] = e.rec
			out[i].inst = inst
		} else {
			missing = append(missing, i)
		}
	}
	m.mu.Unlock()
	decodeOperationRecords(instances, missing, out, now)
	m.mu.Lock()
	if m.byID == nil {
		m.byID = make(map[string]memoRecord, len(instances))
	}
	for _, i := range missing {
		if instances[i].Rev != 0 {
			m.byID[instances[i].ID] = memoRecord{rev: instances[i].Rev, rec: out[i]}
		}
	}
	if len(m.byID) > len(instances) {
		present := make(map[string]struct{}, len(instances))
		for _, inst := range instances {
			present[inst.ID] = struct{}{}
		}
		for id := range m.byID {
			if _, ok := present[id]; !ok {
				delete(m.byID, id)
			}
		}
	}
	m.mu.Unlock()
	// An estimate of the time left depends on the clock, not on a write, so a running action's is made again.
	for i := range out {
		if operationActive(out[i].inst.Status) {
			out[i].savings = projectOperationSavings(out[i], now)
		}
	}
	return out
}

// decodeOperationRecords fills out[i] for each index in `which`: the three JSON documents of the action and
// its savings. Each record is independent, so they are split across the cores.
func decodeOperationRecords(instances []store.ActionInstance, which []int, out []operationRecord, now time.Time) {
	workers := min(runtime.GOMAXPROCS(0), len(which)/64+1)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := w; k < len(which); k += workers {
				i := which[k]
				inst := instances[i]
				rec := operationRecord{inst: inst, inputs: decodeOperationJSON(inst.InputsJSON), outputs: decodeOperationJSON(inst.OutputsJSON), state: decodeOperationJSON(inst.StateJSON)}
				rec.savings = projectOperationSavings(rec, now)
				out[i] = rec
			}
		}()
	}
	wg.Wait()
}

// WarmRecords decodes every action once so the first request after a start
// does not pay for it.
func (s *Server) WarmRecords() error {
	instances, err := s.engine.Deps().Store.ListMaintenanceActionSnapshot()
	if err != nil {
		return err
	}
	s.recordMemo.records(instances, time.Now().UTC())
	return nil
}
