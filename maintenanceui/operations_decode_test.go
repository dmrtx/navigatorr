package maintenanceui

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/jakenesler/navigatorr/store"
)

func TestDecodeOperationRecordsKeepsOrderAndContent(t *testing.T) {
	for _, n := range []int{0, 1, 63, 64, 65, 1000} {
		instances := make([]store.ActionInstance, n)
		for i := range instances {
			instances[i] = store.ActionInstance{
				ID:          fmt.Sprintf("a-%d", i),
				InputsJSON:  fmt.Sprintf(`{"n":%d}`, i),
				OutputsJSON: `{"saved":true}`,
				StateJSON:   fmt.Sprintf(`{"path":"/m/%d.mkv"}`, i),
			}
		}
		instances = append(instances, store.ActionInstance{ID: "broken", InputsJSON: `not json`, OutputsJSON: ``, StateJSON: `[1]`})
		got := decodeOperationRecords(instances)
		if len(got) != len(instances) {
			t.Fatalf("n=%d: %d records for %d instances", n, len(got), len(instances))
		}
		for i, inst := range instances {
			want := operationRecord{inst: inst, inputs: decodeOperationJSON(inst.InputsJSON), outputs: decodeOperationJSON(inst.OutputsJSON), state: decodeOperationJSON(inst.StateJSON)}
			if !reflect.DeepEqual(got[i], want) {
				t.Fatalf("n=%d record %d differs: %v vs %v", n, i, got[i], want)
			}
		}
	}
}
