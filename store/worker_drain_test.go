package store

import "testing"

func TestWorkerDrainIsDurableAndIdempotent(t *testing.T) {
	path := t.TempDir() + "/drain.db"
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.GetWorkerDrain(); err != nil || got.Enabled || got.Revision != 0 {
		t.Fatalf("never used drain must read as off: %+v %v", got, err)
	}
	on, err := s.SetWorkerDrain(true)
	if err != nil || !on.Enabled || on.Since == "" || on.Revision != 1 {
		t.Fatalf("drain on: %+v %v", on, err)
	}
	again, err := s.SetWorkerDrain(true)
	if err != nil || again.Revision != 1 || again.Since != on.Since {
		t.Fatalf("repeating drain must not move the start or revision: %+v %v", again, err)
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got, err := s.GetWorkerDrain(); err != nil || !got.Enabled || got.Since != on.Since {
		t.Fatalf("drain must survive a restart: %+v %v", got, err)
	}
	off, err := s.SetWorkerDrain(false)
	if err != nil || off.Enabled || off.Since != "" || off.Revision != 2 {
		t.Fatalf("resume: %+v %v", off, err)
	}
	if got, _ := s.GetWorkerDrain(); got.Enabled || got.Since != "" {
		t.Fatalf("resume must persist: %+v", got)
	}
}
