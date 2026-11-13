package store

import (
	"path/filepath"
	"testing"
)

func TestBuildPauseSetListResumeAndRepeat(t *testing.T) {
	s := openTest(t)
	if pauses, err := s.ListBuildPauses(); err != nil || len(pauses) != 0 {
		t.Fatalf("no node has builds paused at the start: %v %v", pauses, err)
	}
	if err := s.SetBuildPause("", true); err == nil {
		t.Fatal("a builds pause needs a node")
	}
	if err := s.SetBuildPause("node-a", true); err != nil {
		t.Fatal(err)
	}
	first, _ := s.ListBuildPauses()
	if first["node-a"].Since == "" {
		t.Fatalf("a pause has a start: %+v", first)
	}
	if err := s.SetBuildPause("node-a", true); err != nil {
		t.Fatal(err)
	}
	if again, _ := s.ListBuildPauses(); again["node-a"].Since != first["node-a"].Since {
		t.Fatal("repeating a pause keeps its start")
	}
	if paused, err := s.BuildsPaused("node-a"); err != nil || !paused {
		t.Fatal("node-a is paused", paused, err)
	}
	if paused, _ := s.BuildsPaused("node-b"); paused {
		t.Fatal("node-b is not")
	}
	// Independent of the transcode pause, both ways.
	if _, err := s.SetWorkerPause("node-b", WorkerPauseGraceful); err != nil {
		t.Fatal(err)
	}
	if paused, _ := s.BuildsPaused("node-b"); paused {
		t.Fatal("a transcode pause is not a builds pause")
	}
	if err := s.SetBuildPause("node-a", false); err != nil {
		t.Fatal(err)
	}
	if pauses, _ := s.ListBuildPauses(); len(pauses) != 0 {
		t.Fatalf("resume clears it: %+v", pauses)
	}
	if err := s.SetBuildPause("node-a", false); err != nil {
		t.Fatal("resuming a node that is not paused changes nothing", err)
	}
	if transcode, _ := s.ListWorkerPauses(); transcode["node-b"].Mode != WorkerPauseGraceful {
		t.Fatal("the builds pause never touches the transcode pause", transcode)
	}
}

func TestBuildPauseSurvivesAReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nav.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetBuildPause("node-a", true); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if paused, err := s.BuildsPaused("node-a"); err != nil || !paused {
		t.Fatal("the pause must survive a restart", paused, err)
	}
}
