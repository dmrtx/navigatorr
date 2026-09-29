package store

import (
	"fmt"
	"testing"
)

func TestStatusCountsAreExactAndReturnErrors(t *testing.T) {
	s := openTest(t)
	for i := 0; i < 125; i++ {
		if err := s.CreateActionInstance(ActionInstance{ID: fmt.Sprint(i), ActionName: "transcode_media", Status: "running"}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.AddItem(MaintenanceItem{Service: "sonarr", MediaType: "series", MediaID: fmt.Sprint(i), Title: "Test series", IssueType: "oversized", Status: MaintPending}); err != nil {
			t.Fatal(err)
		}
	}
	a, m, err := s.StatusCounts()
	if err != nil || a["running"] != 125 || m[MaintPending] != 125 {
		t.Fatalf("counts: %v %v %v", a, m, err)
	}
	s.Close()
	if _, _, err := s.StatusCounts(); err == nil {
		t.Fatal("closed database reported counts")
	}
}
