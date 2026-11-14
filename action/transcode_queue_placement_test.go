package action

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/fsop"
	"github.com/jakenesler/navigatorr/store"
)

// pollQueue is one poll of a waiting conversion: it asks for a worker and, as
// the engine does, saves the state the wait left.
func pollQueue(t *testing.T, e *Engine, id string) (string, bool) {
	t.Helper()
	ec := parseExecutionContextMust(t, e, id)
	_, waiting := e.ensureWorkerAssigned(context.Background(), ec)
	if waiting {
		if err := e.persistExecutionState(context.Background(), ec); err != nil {
			t.Fatal(err)
		}
	}
	return assignedWorker(parseExecutionContextMust(t, e, id).State), waiting
}

// The production case: one node is busy, the other is idle with room for
// small files only. The oldest conversion is a standalone one created before
// sizes were recorded, with a source the idle node cannot hold. It must not be
// given that node (and so must not hold its slot); the small conversion and
// the batch behind it must get it, and the size must be known after its first
// turn. "m4" is the idle node, "m1" the busy one.
func TestAnOldUnsizedConversionTheIdleNodeCannotHoldDoesNotHoldItsSlot(t *testing.T) {
	for _, tc := range []struct {
		name    string
		refused bool
	}{
		{"just refused by the idle node", true},
		{"never placed yet", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			big := filepath.Join(dir, "big.mkv")
			f, err := os.Create(big)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.Truncate(70 * gib); err != nil { // sparse
				t.Fatal(err)
			}
			f.Close()
			resolver, err := fsop.NewResolver([]string{dir}, []string{dir})
			if err != nil {
				t.Fatal(err)
			}

			// m4 has room for a small file, not for 70 GiB; m1 is full.
			e := retentionWorkers(t, diskNode(1000*gib, 10*gib, 40*gib), diskNode(1000*gib, 10*gib, 500*gib))
			e.deps.Fs = resolver
			st := e.deps.Store
			seedActionInstance(t, st, "busy-m1", "transcode_media", StatusRunning, 0, "", map[string]any{}, map[string]any{"worker": "m1"})

			// The old conversion: first in the queue, no submitted_source_bytes.
			state := map[string]any{transcodeQueueOrderKey: queueOrder(queueRankPlain, e.now().Add(-10*time.Minute), "old-a", 0)}
			if tc.refused {
				state[requeuedFromKey] = "m4"
				state[roomRefusedByKey] = "m4"
				state[roomRefusedAtKey] = e.now().Add(-5 * time.Second).Format(time.RFC3339Nano)
			}
			seedActionInstance(t, st, "old-a", "transcode_media", StatusRunning, 0, "", map[string]any{"path": big}, state)

			// A small conversion, then a batch with a queued item, both behind it.
			seedActionInstance(t, st, "small-b", "transcode_media", StatusRunning, 0, "", map[string]any{}, map[string]any{SubmittedSourceBytesKey: 10 * gib})
			seedActionInstance(t, st, "batch-1", "transcode_batch", StatusRunning, 0, "", map[string]any{}, map[string]any{})
			if err := st.CreateTranscodeBatchItem(store.TranscodeBatchItem{BatchID: "batch-1", ItemKey: "item-1", FilePath: filepath.Join(dir, "small.mkv"), Status: "queued"}); err != nil {
				t.Fatal(err)
			}

			if name, waiting := pollQueue(t, e, "old-a"); name == "m4" || !waiting {
				t.Fatalf("the old conversion was given %q (waiting %v); m4 cannot hold it", name, waiting)
			}
			if got := getInt64(actionState(t, st, "old-a"), SubmittedSourceBytesKey); got != 70*gib {
				t.Fatalf("the old conversion's size after its first turn is %d, want %d", got, 70*gib)
			}
			if n := e.batchQueueAllowance(context.Background(), "batch-1"); n < 1 {
				t.Fatalf("the batch may start %d item(s); the old conversion holds m4's slot against it", n)
			}
			if name, waiting := pollQueue(t, e, "small-b"); waiting || name != "m4" {
				t.Fatalf("the small conversion behind it was given %q (waiting %v); want m4", name, waiting)
			}
		})
	}
}
