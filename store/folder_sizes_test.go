package store

import (
	"fmt"
	"path/filepath"
	"testing"
)

func TestFolderSizeSnapshotsAreSavedTogetherAndKeptWithinTheCap(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.SaveFolderSizeSnapshot("/nas/one", `{"n":1}`); err != nil {
		t.Fatal(err)
	}
	if raw, err := s.FolderSizeSnapshot("/nas/one"); err != nil || raw != `{"n":1}` {
		t.Fatalf("%q %v", raw, err)
	}
	batch := map[string]string{}
	for i := 0; i < folderSizeRows+25; i++ {
		batch[fmt.Sprintf("/nas/f%05d", i)] = fmt.Sprintf(`{"n":%d}`, i)
	}
	if err := s.SaveFolderSizeSnapshots(batch); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM folder_size_cache`).Scan(&rows); err != nil || rows != folderSizeRows {
		t.Fatalf("%d rows kept: %v", rows, err)
	}
	if err := s.SaveFolderSizeSnapshots(nil); err != nil {
		t.Fatal(err)
	}
}
