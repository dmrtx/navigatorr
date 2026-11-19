package nodes

import (
	"path/filepath"
	"testing"
	"time"
)

func TestBuildPauseSetListResumeAndRepeat(t *testing.T) {
	s := openReg(t)
	now := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	if pauses, err := s.listPauses(); err != nil || len(pauses) != 0 {
		t.Fatalf("no node has builds paused at the start: %v %v", pauses, err)
	}
	if err := s.setPause("", true, now); err == nil {
		t.Fatal("a builds pause needs a node")
	}
	if err := s.setPause("nodeA", true, now); err != nil {
		t.Fatal(err)
	}
	first, _ := s.listPauses()
	if first["nodeA"].Since == "" {
		t.Fatalf("a pause has a start: %+v", first)
	}
	if err := s.setPause("nodeA", true, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if again, _ := s.listPauses(); again["nodeA"].Since != first["nodeA"].Since {
		t.Fatal("repeating a pause keeps its start")
	}
	if paused, err := s.paused("nodeA"); err != nil || !paused {
		t.Fatal("nodeA is paused", paused, err)
	}
	if paused, _ := s.paused("nodeB"); paused {
		t.Fatal("nodeB is not")
	}
	if err := s.setPause("nodeA", false, now); err != nil {
		t.Fatal(err)
	}
	if pauses, _ := s.listPauses(); len(pauses) != 0 {
		t.Fatalf("resume clears it: %+v", pauses)
	}
	if err := s.setPause("nodeA", false, now); err != nil {
		t.Fatal("resuming a node that is not paused changes nothing", err)
	}
}

func TestBuildPauseSurvivesAReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nodes.db")
	s, err := openRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.setPause("nodeA", true, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.close(); err != nil {
		t.Fatal(err)
	}
	s, err = openRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	if paused, err := s.paused("nodeA"); err != nil || !paused {
		t.Fatal("the pause must survive a restart", paused, err)
	}
}
