package store

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// Acceptance: a preference survives a storage restart.
func TestPersistenceAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nav.db")

	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := s.SetPreference("anime", "preferred_resolution", `"1080p"`, "user", 0); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	p, err := s2.GetPreference("anime", "preferred_resolution")
	if err != nil {
		t.Fatalf("preference did not survive restart: %v", err)
	}
	var res string
	if err := p.Value(&res); err != nil || res != "1080p" {
		t.Errorf("got value %q, want 1080p", p.ValueJSON)
	}
}

// Acceptance: re-adding the same active media+issue returns the survivor.
func TestAddItemIdempotent(t *testing.T) {
	s := openTest(t)
	base := MaintenanceItem{MediaType: "series", Service: "sonarr", MediaID: "248",
		Title: "The Case Study of Vanitas", IssueType: "oversized", Priority: 5}
	a, err := s.AddItem(base)
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	b, err := s.AddItem(base)
	if err != nil {
		t.Fatalf("re-add: %v", err)
	}
	if a.ID != b.ID {
		t.Errorf("duplicate created: %d vs %d", a.ID, b.ID)
	}
	items, err := s.ListItems(ItemFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 1 {
		t.Errorf("got %d items, want 1", len(items))
	}
}

// A finished job does not block a fresh one for the same media+issue.
func TestAddItemAfterDoneCreatesNew(t *testing.T) {
	s := openTest(t)
	base := MaintenanceItem{MediaType: "movie", Service: "radarr", MediaID: "1",
		Title: "Oldboy", IssueType: "missing_accessible_language"}
	a, err := s.AddItem(base)
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	for _, next := range []string{MaintResearching, MaintCandidate, MaintDownloading,
		MaintDownloaded, MaintVerifying, MaintImporting, MaintReplacing} {
		if _, err := s.Transition(a.ID, next, ""); err != nil {
			t.Fatalf("transition to %s: %v", next, err)
		}
		a.Status = next
	}
	if _, err := s.ResolveItem(a.ID, MaintDone, "ok"); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	b, err := s.AddItem(base)
	if err != nil {
		t.Fatalf("re-add after done: %v", err)
	}
	if b.ID == a.ID {
		t.Error("re-scan after done returned the finished item instead of fresh work")
	}
}

// Acceptance: maintenance_next returns the highest-priority actionable job.
func TestNextItemPriority(t *testing.T) {
	s := openTest(t)
	low, err := s.AddItem(MaintenanceItem{MediaType: "series", Service: "sonarr",
		MediaID: "1", Title: "Low", IssueType: "oversized", Priority: 1})
	if err != nil {
		t.Fatalf("add low: %v", err)
	}
	high, err := s.AddItem(MaintenanceItem{MediaType: "series", Service: "sonarr",
		MediaID: "2", Title: "High", IssueType: "oversized", Priority: 9})
	if err != nil {
		t.Fatalf("add high: %v", err)
	}
	_ = low
	next, err := s.NextItem()
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if next.ID != high.ID {
		t.Errorf("next returned %q, want High", next.Title)
	}
}

// Acceptance: invalid transitions are rejected.
func TestStateMachineRejectsInvalid(t *testing.T) {
	s := openTest(t)
	it, err := s.AddItem(MaintenanceItem{MediaType: "series", Service: "sonarr",
		MediaID: "3", Title: "Vanitas", IssueType: "oversized"})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, err := s.Transition(it.ID, MaintDone, "skip"); err == nil {
		t.Error("pending -> done was allowed; the safe workflow must forbid it")
	}
	if _, err := s.ResolveItem(it.ID, MaintDone, "skip"); err == nil {
		t.Error("resolve done from pending was allowed")
	}
	if _, err := s.ResolveItem(it.ID, "bogus", "x"); err == nil {
		t.Error("bogus resolve status was allowed")
	}
	// Legal edges still work.
	if _, err := s.Transition(it.ID, MaintResearching, ""); err != nil {
		t.Errorf("legal transition rejected: %v", err)
	}
	if _, err := s.ResolveItem(it.ID, MaintFailed, "no candidates"); err != nil {
		t.Errorf("resolve failed rejected: %v", err)
	}
	if _, err := s.ResolveItem(it.ID, MaintFailed, "again"); err == nil {
		t.Error("double resolve was allowed")
	}
}

// Claiming is a lease: a second owner fails until release.
func TestClaimLease(t *testing.T) {
	s := openTest(t)
	it, _ := s.AddItem(MaintenanceItem{MediaType: "movie", Service: "radarr",
		MediaID: "7", Title: "Ip Man", IssueType: "missing_accessible_language"})
	if _, err := s.ClaimItem(it.ID, "agent-a", time.Minute); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := s.ClaimItem(it.ID, "agent-b", time.Minute); err == nil {
		t.Error("double claim was allowed")
	}
	if _, err := s.ReleaseItem(it.ID); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, err := s.ClaimItem(it.ID, "agent-b", time.Minute); err != nil {
		t.Errorf("claim after release rejected: %v", err)
	}
}

// Acceptance: an expired fact is not returned as vigente.
func TestExpiredFactNotReturned(t *testing.T) {
	s := openTest(t)
	if _, err := s.SetPreference("global", "qb_seeders_x", `159`, "fact", time.Millisecond); err != nil {
		t.Fatalf("set: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	if _, err := s.GetPreference("global", "qb_seeders_x"); err == nil {
		t.Error("expired fact was returned as vigente")
	}
	if list, _ := s.ListPreferences("global"); len(list) != 0 {
		t.Errorf("expired fact listed: %+v", list)
	}
}

// Secrets must never land in the audit log.
func TestActionLogRedactsSecrets(t *testing.T) {
	s := openTest(t)
	if err := s.LogAction("call", "sonarr", "x",
		`{"api_key":"SUPERSECRET","path":"/series"}`, "ok"); err != nil {
		t.Fatalf("log: %v", err)
	}
	recent, err := s.RecentActions(5)
	if err != nil {
		t.Fatalf("recent: %v", err)
	}
	if len(recent) != 1 {
		t.Fatalf("got %d entries", len(recent))
	}
	if strings.Contains(recent[0]["args"].(string), "SUPERSECRET") {
		t.Error("secret leaked into action_log")
	}
}

func TestPreferenceValueCap(t *testing.T) {
	s := openTest(t)
	big := strings.Repeat("x", MaxPreferenceValueLen+1)
	if _, err := s.SetPreference("global", "blob", `"`+big+`"`, "user", 0); err == nil {
		t.Errorf("oversized value accepted; limit is %d", MaxPreferenceValueLen)
	}
	if _, err := s.SetPreference("global", "ok", `"small"`, "user", 0); err != nil {
		t.Errorf("normal value rejected: %v", err)
	}
}

func TestBlocklist(t *testing.T) {
	s := openTest(t)
	if s.IsBlocked("abc123") {
		t.Error("unblocked hash reported blocked")
	}
	if err := s.BlockRelease("abc123", "malware", "auto"); err != nil {
		t.Fatalf("block: %v", err)
	}
	if !s.IsBlocked("abc123") {
		t.Error("blocked hash not reported")
	}
}

func TestDecisionsAppendOnly(t *testing.T) {
	s := openTest(t)
	it, _ := s.AddItem(MaintenanceItem{MediaType: "series", Service: "sonarr",
		MediaID: "9", Title: "Fate/strange Fake", IssueType: "oversized"})
	if _, err := s.RecordDecision(ReleaseDecision{MaintenanceItemID: it.ID,
		Title: "Judas release", Decision: DecisionSelected, ReasonsJSON: `["multi_subs"]`}); err != nil {
		t.Fatalf("record selected: %v", err)
	}
	if _, err := s.RecordDecision(ReleaseDecision{MaintenanceItemID: it.ID,
		Title: "SubsPlease release", Decision: DecisionRejected, ReasonsJSON: `["too big"]`}); err != nil {
		t.Fatalf("record rejected: %v", err)
	}
	list, err := s.ListDecisions(it.ID, 10)
	if err != nil || len(list) != 2 {
		t.Fatalf("decisions: %+v %v", list, err)
	}
	if list[0].Decision != DecisionRejected {
		t.Error("newest-first order broken")
	}
}
