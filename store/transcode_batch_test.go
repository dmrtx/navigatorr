package store

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func TestTranscodeBatchItemCRUD(t *testing.T) {
	s := openTest(t)

	item := TranscodeBatchItem{
		BatchID:       "batch-1",
		ItemKey:       "epfile-101",
		FilePath:      "/media/tv/series/s01e01.mkv",
		DisplayLabel:  "Series - S01E01",
		EpisodeInfo:   "S01E01",
		Decision:      "transcode",
		Profile:       "general-hevc",
		Reasons:       []string{"h264 1080p high bitrate"},
		Status:        "queued",
		ChildActionID: "act-child-1",
		JobID:         "job-transcode-99",
		CandidatePath: "/media/tv/series/s01e01.candidate.mkv",
		Error:         "",
		Attempts:      1,
	}

	if err := s.CreateTranscodeBatchItem(item); err != nil {
		t.Fatalf("CreateTranscodeBatchItem: %v", err)
	}

	fetched, err := s.GetTranscodeBatchItem("batch-1", "epfile-101")
	if err != nil {
		t.Fatalf("GetTranscodeBatchItem: %v", err)
	}
	if fetched == nil {
		t.Fatalf("expected item, got nil")
	}
	if fetched.JobID != "job-transcode-99" {
		t.Errorf("expected JobID job-transcode-99, got %q", fetched.JobID)
	}
	if fetched.ChildActionID != "act-child-1" {
		t.Errorf("expected ChildActionID act-child-1, got %q", fetched.ChildActionID)
	}
	if len(fetched.Reasons) != 1 || fetched.Reasons[0] != "h264 1080p high bitrate" {
		t.Errorf("unexpected reasons: %v", fetched.Reasons)
	}

	// Update JobID and status
	fetched.Status = "completed"
	fetched.JobID = "job-transcode-updated"
	fetched.Attempts = 2
	if err := s.UpdateTranscodeBatchItem(*fetched); err != nil {
		t.Fatalf("UpdateTranscodeBatchItem: %v", err)
	}

	updated, err := s.GetTranscodeBatchItem("batch-1", "epfile-101")
	if err != nil {
		t.Fatalf("GetTranscodeBatchItem after update: %v", err)
	}
	if updated.JobID != "job-transcode-updated" {
		t.Errorf("expected updated JobID job-transcode-updated, got %q", updated.JobID)
	}
	if updated.Status != "completed" {
		t.Errorf("expected status completed, got %q", updated.Status)
	}
	if updated.Attempts != 2 {
		t.Errorf("expected attempts 2, got %d", updated.Attempts)
	}

	items, err := s.ListTranscodeBatchItems("batch-1")
	if err != nil {
		t.Fatalf("ListTranscodeBatchItems: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].JobID != "job-transcode-updated" {
		t.Errorf("expected listed item JobID job-transcode-updated, got %q", items[0].JobID)
	}
}

func TestMigrationV4ToV5(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "migrate_v4_v5.db")

	// Open raw DB and apply migrations 1 through 4 manually
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("opening raw db: %v", err)
	}
	defer db.Close()

	if _, err := db.Exec(`CREATE TABLE schema_version (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatalf("creating schema_version: %v", err)
	}

	for _, m := range migrations {
		if m.version > 4 {
			continue
		}
		for _, stmt := range m.statements {
			if _, err := db.Exec(stmt); err != nil {
				t.Fatalf("applying migration %d: %v", m.version, err)
			}
		}
		if _, err := db.Exec(`INSERT INTO schema_version (version, applied_at) VALUES (?, ?)`,
			m.version, time.Now().UTC().Format(time.RFC3339)); err != nil {
			t.Fatalf("recording migration %d: %v", m.version, err)
		}
	}

	// Insert row under v4 schema (without job_id)
	_, err = db.Exec(`INSERT INTO transcode_batch_items (
		batch_id, item_key, file_path, display_label, episode_info,
		decision, profile, reasons_json, status, child_action_id,
		candidate_path, error, attempts, created_at, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"batch-old", "epfile-1", "/path/ep1.mkv", "Ep 1", "S01E01",
		"transcode", "general-hevc", "[]", "queued", "child-1",
		"", "", 0, "now", "now")
	if err != nil {
		t.Fatalf("inserting v4 item: %v", err)
	}
	db.Close()

	// Now open using Store.Open, which should run migration 5
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Store.Open failed: %v", err)
	}
	defer s.Close()

	var ver int
	if err := s.db.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&ver); err != nil {
		t.Fatalf("reading version: %v", err)
	}
	if ver != 5 {
		t.Fatalf("expected schema version 5, got %d", ver)
	}

	// Verify old item can be read and defaults job_id to empty string
	it, err := s.GetTranscodeBatchItem("batch-old", "epfile-1")
	if err != nil {
		t.Fatalf("GetTranscodeBatchItem after migration: %v", err)
	}
	if it == nil {
		t.Fatalf("item not found")
	}
	if it.JobID != "" {
		t.Errorf("expected empty job_id for migrated item, got %q", it.JobID)
	}

	// Update item with a new JobID
	it.JobID = "job-new-migrated"
	if err := s.UpdateTranscodeBatchItem(*it); err != nil {
		t.Fatalf("UpdateTranscodeBatchItem after migration: %v", err)
	}

	it2, err := s.GetTranscodeBatchItem("batch-old", "epfile-1")
	if err != nil {
		t.Fatalf("GetTranscodeBatchItem after update: %v", err)
	}
	if it2.JobID != "job-new-migrated" {
		t.Errorf("expected job_id job-new-migrated, got %q", it2.JobID)
	}
}
