package store

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func openTestTB(tb testing.TB) *Store {
	tb.Helper()
	s, err := Open(filepath.Join(tb.TempDir(), "bench.db"))
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { s.Close() })
	return s
}

// seedFileStatusHistory fills the store like a library that has been converted
// for a while: many finished batches, each item with an attempt whose outputs
// carry a few KB of steps, and a replacement for some of them.
func seedFileStatusHistory(tb testing.TB, s *Store, batches, perBatch int) {
	tb.Helper()
	filler := `"log":"` + strings.Repeat("frame=  1234 fps= 48 q=28.0 size=  102400kB time=00:41:12 bitrate=339.1kbits/s speed=2.1x\\n", 30) + `"`
	tx, err := s.db.Begin()
	if err != nil {
		tb.Fatal(err)
	}
	insertAction := func(id, name, status, key, inputs, outputs string) {
		if _, err := tx.Exec(`INSERT INTO action_instances (id, action_name, status, idempotency_key, inputs_json, outputs_json, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, 'x', 'x')`, id, name, status, key, inputs, outputs); err != nil {
			tb.Fatal(err)
		}
	}
	for b := 0; b < batches; b++ {
		batch := fmt.Sprintf("batch-%d", b)
		insertAction(batch, "transcode_batch", "completed", "", fmt.Sprintf(`{"dry_run":false,"label":"%s"}`, batch), `{}`)
		for i := 0; i < perBatch; i++ {
			child := fmt.Sprintf("child-%d-%d", b, i)
			outputs := fmt.Sprintf(`{"candidate":"/media/x/%d/%d.new.mkv",%s,"skip_transcode":%t}`, b, i, filler, i%7 == 0)
			insertAction(child, "transcode_media", "completed", "", `{}`, outputs)
			status, decision := "completed", "transcode"
			if i%7 == 0 {
				decision = ""
			}
			if _, err := tx.Exec(`INSERT INTO transcode_batch_items (batch_id, item_key, file_path, display_label, status, decision, child_action_id, candidate_path, created_at, updated_at) VALUES (?, ?, ?, 'l', ?, ?, ?, ?, 'x', 'x')`,
				batch, fmt.Sprint(i), fmt.Sprintf("/media/show-%d/Season 1/e%03d.mkv", b, i), status, decision, child, "/media/c.mkv"); err != nil {
				tb.Fatal(err)
			}
			if i%3 == 0 {
				insertAction("promote-"+child, "promote_transcode_candidate", "completed", "promote:filesystem:"+child, `{}`, `{"promoted":true,`+filler+`}`)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		tb.Fatal(err)
	}
}

func BenchmarkListFileStatusRows(b *testing.B) {
	s := openTestTB(b)
	seedFileStatusHistory(b, s, 100, 100) // 10,000 files
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rows, err := s.ListFileStatusRows()
		if err != nil || len(rows) != 10000 {
			b.Fatal(err, len(rows))
		}
	}
}
