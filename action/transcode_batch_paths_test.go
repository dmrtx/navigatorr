package action

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jakenesler/navigatorr/transcode"
)

func TestFilesystemBatchSelectionWithoutLibrary(t *testing.T) {
	mock := &mockTranscodeExecutor{}
	e, st, _, srv, root := setupBatchTestEnv(t, mock, 1)
	defer st.Close()
	defer srv.Close()
	e.deps.Registry = nil // The filesystem source must not require or query *arr.
	a := filepath.Join(root, "Kaguya S01E01-E02.mkv")
	b := filepath.Join(root, "Kaguya S02E02.mkv")
	alias := filepath.Join(root, "alias.mkv")
	if err := os.Symlink(a, alias); err != nil {
		t.Fatal(err)
	}
	r, err := e.Run(context.Background(), "transcode_batch", map[string]any{
		"paths": []string{b, alias, a, b}, "profile": "general-hevc", "dry_run": true, "media_type": "movie",
	})
	if err != nil || r.Status != StatusCompleted {
		t.Fatalf("filesystem preview failed: %+v %v", r, err)
	}
	items, err := st.ListTranscodeBatchItems(r.ID)
	a, _ = e.deps.Fs.ResolveRead(a)
	b, _ = e.deps.Fs.ResolveRead(b)
	files := map[string]bool{}
	for _, item := range items {
		files[item.FilePath] = true
	}
	if err != nil || len(items) != 2 || !files[a] || !files[b] {
		t.Fatalf("selection was not canonical and deduplicated: %+v %v", items, err)
	}
	for _, item := range items {
		if !strings.HasPrefix(item.ItemKey, "file-") || item.EpisodeInfo != "" || item.Status != "queued" {
			t.Fatalf("filesystem file misrepresented: %+v", item)
		}
	}
	if atomic.LoadInt32(&mock.submitCalls) != 0 || r.State["source_kind"] != "filesystem" {
		t.Fatalf("preview submitted encoding or lost source: %+v", r)
	}
}

func TestFilesystemBatchRejectsInvalidSourcesBeforeCreatingItems(t *testing.T) {
	e, st, _, srv, root := setupBatchTestEnv(t, &mockTranscodeExecutor{}, 1)
	defer st.Close()
	defer srv.Close()
	e.deps.Registry = nil
	valid := filepath.Join(root, "Kaguya S01E01-E02.mkv")
	outside := filepath.Join(t.TempDir(), "outside.mkv")
	if err := os.WriteFile(outside, []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "escaped.mkv")
	if err := os.Symlink(outside, alias); err != nil {
		t.Fatal(err)
	}
	audio := filepath.Join(root, "audio.mp3")
	if err := os.WriteFile(audio, []byte("audio"), 0600); err != nil {
		t.Fatal(err)
	}
	tooMany := make([]string, 1001)
	for i := range tooMany {
		tooMany[i] = valid
	}
	for _, tc := range []struct {
		name  string
		paths any
		extra map[string]any
	}{
		{"empty", []string{}, nil},
		{"null", nil, nil},
		{"invalid-type", []int{1}, nil},
		{"blank", []string{valid, ""}, nil},
		{"too-many", tooMany, nil},
		{"directory", []string{root}, nil},
		{"escaped-symlink", []string{valid, alias}, nil},
		{"audio", []string{audio}, nil},
		{"service", []string{valid}, map[string]any{"service": "sonarr"}},
		{"series", []string{valid}, map[string]any{"series_id": 10}},
		{"season", []string{valid}, map[string]any{"season": 1}},
		{"file-id", []string{valid}, map[string]any{"episode_file_ids": []int{101}}},
		{"promotion", []string{valid}, map[string]any{"promote_candidates": true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inputs := map[string]any{"paths": tc.paths, "dry_run": true}
			for key, value := range tc.extra {
				inputs[key] = value
			}
			r, err := e.Run(context.Background(), "transcode_batch", inputs)
			if err != nil || r.Status != StatusFailed {
				t.Fatalf("invalid source admitted: %+v %v", r, err)
			}
			items, err := st.ListTranscodeBatchItems(r.ID)
			if err != nil || len(items) != 0 {
				t.Fatalf("partial source admission: %+v %v", items, err)
			}
		})
	}
}

func TestFilesystemBatchPauseResumeUsesExistingScheduler(t *testing.T) {
	mock := &mockTranscodeExecutor{submitFunc: func(_ context.Context, req transcode.Request) (transcode.Job, error) {
		writeCandidateOutput(req.CandidatePath)
		return transcode.Job{ID: req.ID}, nil
	}}
	e, st, _, srv, root := setupBatchTestEnv(t, mock, 1)
	defer st.Close()
	defer srv.Close()
	e.deps.Registry = nil
	path := filepath.Join(root, "Kaguya S01E01-E02.mkv")
	r, err := e.Run(context.Background(), "transcode_batch", map[string]any{
		"paths": []string{path}, "profile": "general-hevc", "paused": true, "media_type": "movie",
	})
	if err != nil || r.Status != StatusWaitingDecision || atomic.LoadInt32(&mock.submitCalls) != 0 {
		t.Fatalf("paused source dispatched encoding: %+v %v", r, err)
	}
	r, err = e.Resume(context.Background(), r.ID, "resume", nil)
	if err != nil || r.Status != StatusCompleted || atomic.LoadInt32(&mock.submitCalls) != 1 {
		t.Fatalf("filesystem scheduler did not complete: %+v %v", r, err)
	}
	items, err := st.ListTranscodeBatchItems(r.ID)
	if err != nil || len(items) != 1 || items[0].Status != "completed" || items[0].ChildActionID == "" || items[0].CandidatePath == "" {
		t.Fatalf("candidate tracking missing: %+v %v", items, err)
	}
	child, err := e.Status(context.Background(), items[0].ChildActionID)
	if err != nil || child.Inputs["media_type"] != "movie" || child.Inputs["parent_action_id"] != r.ID {
		t.Fatalf("child context lost: %+v %v", child, err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Size() != 1400000000 {
		t.Fatalf("original was modified: %+v %v", fi, err)
	}
}
