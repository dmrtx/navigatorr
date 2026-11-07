package action

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/store"
)

func batchPicksFixture(t *testing.T) (*Engine, *store.Store, string, string) {
	t.Helper()
	dir := t.TempDir()
	picked, other := filepath.Join(dir, "picked.mkv"), filepath.Join(dir, "other.mkv")
	for _, f := range []string{picked, other} {
		if err := os.WriteFile(f, make([]byte, 100), 0600); err != nil {
			t.Fatal(err)
		}
	}
	e, st := setupTranscodeEngine(t, &mockTranscodeExecutor{}, writeProbeScript(t, dir, h264SDRProbeJSON), []string{dir}, []string{dir}, false)
	t.Cleanup(func() { st.Close() })
	return e, st, picked, other
}

func runPicksBatch(t *testing.T, e *Engine, inputs map[string]any) (*ActionResult, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	inputs["dry_run"] = true
	return e.Run(ctx, "transcode_batch", inputs)
}

// A file picked by hand keeps exactly those tracks; the batch's other files
// follow its languages, here every track.
func TestBatchFilePicksReachOnlyThatFile(t *testing.T) {
	e, st, picked, other := batchPicksFixture(t)
	res, err := runPicksBatch(t, e, map[string]any{"paths": []any{picked, other}, "mode": "size", "keep_languages": []any{},
		"track_picks": map[string]any{picked: map[string]any{"audio": []any{float64(2), float64(1)}, "subtitles": []any{}}}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusCompleted {
		t.Fatalf("batch %s: %s", res.Status, res.Error)
	}
	items, err := st.ListTranscodeBatchItems(res.ID)
	if err != nil || len(items) != 2 {
		t.Fatalf("items %+v %v", items, err)
	}
	child := func(name string) *ModePolicySnapshot {
		i := slices.IndexFunc(items, func(it store.TranscodeBatchItem) bool { return filepath.Base(it.FilePath) == filepath.Base(name) })
		ec := &ExecutionContext{InstanceID: "child", ActionName: "transcode_media", Inputs: map[string]any{"path": items[i].FilePath, "mode": "size", "parent_action_id": res.ID, "batch_item_key": items[i].ItemKey}, State: map[string]any{}}
		if err := e.bindModeParent(ec); err != nil {
			t.Fatal(err)
		}
		p, err := freezeModePolicy(ec)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	p := child(picked)
	if p.Tracks == nil || p.Tracks.AudioStreams == nil || !slices.Equal(*p.Tracks.AudioStreams, []int{1, 2}) || p.Tracks.SubtitleStreams == nil || len(*p.Tracks.SubtitleStreams) != 0 || len(p.Tracks.Languages) != 0 {
		t.Fatalf("picked file tracks %+v", p.Tracks)
	}
	if o := child(other); o.Tracks != nil || o.Digest == p.Digest {
		t.Fatalf("the other file took the picks: %+v", o.Tracks)
	}
}

func TestBatchFilePicksAreChecked(t *testing.T) {
	e, _, picked, other := batchPicksFixture(t)
	for name, tc := range map[string]struct {
		inputs map[string]any
		want   string
	}{
		"not in the batch": {map[string]any{"paths": []any{other}, "mode": "size", "track_picks": map[string]any{picked: map[string]any{"audio": []any{float64(1)}}}}, "not in paths"},
		"no audio":         {map[string]any{"paths": []any{picked}, "mode": "size", "track_picks": map[string]any{picked: map[string]any{"audio": []any{}}}}, "at least one audio"},
		"other kind":       {map[string]any{"paths": []any{picked}, "mode": "size", "track_picks": map[string]any{picked: map[string]any{"video": []any{float64(0)}}}}, "takes audio and subtitles"},
		"goal copies all":  {map[string]any{"paths": []any{picked}, "mode": "quality", "track_picks": map[string]any{picked: map[string]any{"audio": []any{float64(1)}}}}, "mode=size or mode=compact"},
	} {
		// Refused when sent, before anything is queued.
		if _, err := runPicksBatch(t, e, tc.inputs); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
