package store

import (
	"bytes"
	"log"
	"strings"
	"testing"
	"time"
)

func holdForTest(m *watchedMutex, d time.Duration) {
	m.Lock()
	time.Sleep(d)
	m.Unlock()
}

func TestWatchedMutexNamesALongHolder(t *testing.T) {
	var out bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&out)
	defer log.SetOutput(prev)
	var m watchedMutex
	holdForTest(&m, 10*time.Millisecond)
	if out.Len() != 0 {
		t.Fatalf("a short hold was logged: %q", out.String())
	}
	holdForTest(&m, slowLockHold+50*time.Millisecond)
	if line := out.String(); !strings.Contains(line, "store lock: held") || !strings.Contains(line, "holdForTest") {
		t.Fatalf("long hold not named: %q", line)
	}
}
