package nodes

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestHealthyWhenOpenAndMetaReadable(t *testing.T) {
	s, _ := buildSvc(t)
	if err := s.Healthy(context.Background()); err != nil {
		t.Fatalf("healthy registry reported %v", err)
	}
}

func TestHealthyFailsOnAClosedDatabase(t *testing.T) {
	s, _ := buildSvc(t)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Healthy(context.Background()); err == nil {
		t.Fatal("a closed registry reported healthy")
	}
}

func TestHealthyFailsWhenTheMetaTableIsGone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nodes.db")
	s, _ := buildSvcAt(t, path)
	other, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err := other.Exec(`DROP TABLE meta`); err != nil {
		t.Fatal(err)
	}
	if err := s.Healthy(context.Background()); err == nil {
		t.Fatal("a registry without its meta table reported healthy")
	}
}

func TestHealthyOnANilService(t *testing.T) {
	var s *Service
	if err := s.Healthy(context.Background()); err == nil {
		t.Fatal("a nil service reported healthy")
	}
}
