package store

import (
	"fmt"
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
	var lines []string
	m := watchedMutex{
		slow: 30 * time.Millisecond,
		logf: func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) },
	}
	holdForTest(&m, 0)
	if len(lines) != 0 {
		t.Fatalf("a short hold was logged: %q", lines)
	}
	holdForTest(&m, 60*time.Millisecond)
	if len(lines) != 1 || !strings.Contains(lines[0], "store lock: held") || !strings.Contains(lines[0], "holdForTest") {
		t.Fatalf("long hold not named: %q", lines)
	}
}
