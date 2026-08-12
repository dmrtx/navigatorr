package queue

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "queue.json"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestAddListClaimResolve(t *testing.T) {
	s := openTemp(t)

	it, err := s.Add("boston legal", "imessage")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if it.ID != "r1" || it.Status != StatusPending {
		t.Fatalf("Add returned %+v", it)
	}
	if got := s.List(StatusPending); len(got) != 1 {
		t.Fatalf("List(pending) = %d items, want 1", len(got))
	}
	if _, err := s.Claim("r1"); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if _, err := s.Claim("r1"); err == nil {
		t.Error("Claim on an already-claimed item should fail")
	}
	got, err := s.Resolve("r1", StatusDone, "added, 5 seasons")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Status != StatusDone || got.Note != "added, 5 seasons" {
		t.Errorf("Resolve returned %+v", got)
	}
}

// Release put finished work back in the pending queue, where an agent would
// action it a second time.
func TestReleaseRejectsUnclaimedItem(t *testing.T) {
	s := openTemp(t)
	s.Add("boston legal", "")
	s.Claim("r1")
	s.Resolve("r1", StatusDone, "added, 5 seasons")

	if _, err := s.Release("r1"); err == nil {
		t.Fatal("Release on a done item should fail")
	}
	if got := s.List(StatusPending); len(got) != 0 {
		t.Errorf("done item is back in pending: %+v", got)
	}
	// A pending item was never claimed, so there is nothing to release either.
	s.Add("the wire", "")
	if _, err := s.Release("r2"); err == nil {
		t.Error("Release on a pending item should fail")
	}
}

// Resolve validated the target status but never the current one, so a second
// resolve clobbered the first outcome's note.
func TestResolveRejectsAlreadyResolved(t *testing.T) {
	s := openTemp(t)
	s.Add("boston legal", "")
	s.Claim("r1")
	if _, err := s.Resolve("r1", StatusDone, "added"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Resolve("r1", StatusFailed, "clobbered"); err == nil {
		t.Fatal("second Resolve should fail")
	}
	if got := s.List(StatusDone); len(got) != 1 || got[0].Note != "added" {
		t.Errorf("original note was lost: %+v", got)
	}
}

func TestAddRejectsEmptyAndOversizedText(t *testing.T) {
	s := openTemp(t)
	if _, err := s.Add("   ", ""); err == nil {
		t.Error("blank text should be rejected")
	}
	if _, err := s.Add(strings.Repeat("a", MaxTextLen+1), ""); err == nil {
		t.Error("oversized text should be rejected")
	}
	if _, err := s.Add(strings.Repeat("a", MaxTextLen), ""); err != nil {
		t.Errorf("text at exactly the limit should be accepted: %v", err)
	}
}

// A queue file carrying items but no next_id (older format, hand-edit, partial
// restore) minted a duplicate ID, and find() then returned the wrong request.
func TestOpenReconcilesNextIDFromItems(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "queue.json")
	os.WriteFile(path, []byte(`{"items":[
	  {"id":"r1","text":"boston legal","status":"pending"},
	  {"id":"r7","text":"the wire","status":"done"}
	]}`), 0o600)

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	it, err := s.Add("severance", "")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if it.ID != "r8" {
		t.Errorf("new item got ID %q, want r8 (highest existing is r7)", it.ID)
	}
}

// A zero-byte file is what a crash between create and write leaves behind.
// Failing to parse it took the whole MCP server down, not just the queue.
func TestOpenToleratesEmptyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.json")
	os.WriteFile(path, nil, 0o600)

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open on a zero-byte file: %v", err)
	}
	defer s.Close()
	if got := s.List(""); len(got) != 0 {
		t.Errorf("want empty queue, got %+v", got)
	}
}

func TestOpenSkipsNullItems(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.json")
	os.WriteFile(path, []byte(`{"next_id":2,"items":[null,{"id":"r1","text":"x","status":"pending"}]}`), 0o600)

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	if got := s.List(""); len(got) != 1 { // must not panic
		t.Errorf("want 1 item, got %d", len(got))
	}
}

// A second process must not be able to open the same queue and overwrite the
// first one's items.
func TestOpenIsExclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.json")
	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()

	if second, err := Open(path); err == nil {
		second.Close()
		t.Fatal("a second Open on the same queue should fail")
	}
	// Closing the first releases the lock.
	first.Close()
	third, err := Open(path)
	if err != nil {
		t.Fatalf("Open after Close should succeed: %v", err)
	}
	third.Close()
}

// Every mutating method used to return a *Item aliasing live state, which the
// HTTP handler then encoded outside the lock while another goroutine wrote to
// it. Run with -race.
func TestConcurrentAccessIsRaceFree(t *testing.T) {
	s := openTemp(t)
	for i := 0; i < 20; i++ {
		if _, err := s.Add("item", "test"); err != nil {
			t.Fatal(err)
		}
	}

	var wg sync.WaitGroup
	for i := 1; i <= 20; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			id := "r" + itoa(n)
			if it, err := s.Claim(id); err == nil {
				_ = it.Status // read the returned copy while others mutate
				s.Resolve(id, StatusDone, "done")
			}
		}(i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, it := range s.List("") {
				_ = it.Text
			}
			_ = s.Counts()
		}()
	}
	wg.Wait()

	if got := s.List(StatusDone); len(got) != 20 {
		t.Errorf("want 20 done, got %d", len(got))
	}
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// Persisted state must survive a reopen with statuses and notes intact.
func TestStatePersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Add("boston legal", "imessage")
	s.Claim("r1")
	s.Resolve("r1", StatusDone, "added, 5 seasons")
	s.Add("the wire", "imessage")
	s.Close()

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()

	counts := reopened.Counts()
	if counts[StatusDone] != 1 || counts[StatusPending] != 1 {
		t.Errorf("counts after reopen = %v", counts)
	}
	if got := reopened.List(StatusDone); got[0].Note != "added, 5 seasons" {
		t.Errorf("note did not persist: %+v", got[0])
	}
}
