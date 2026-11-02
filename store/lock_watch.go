package store

import (
	"log"
	"runtime"
	"sync"
	"time"
)

// slowLockHold is how long the store's lock may be held before the holder is
// named in the log. Every store call takes this one lock, so a holder that
// keeps it for seconds is what makes unrelated requests (the queue, the
// workers, the files) all slow at once.
const slowLockHold = time.Second

// watchedMutex is a sync.Mutex that logs who held it for too long. It
// records only the function that took the lock and the time, never what the
// holder was working on.
type watchedMutex struct {
	mu sync.Mutex
	// Set by the holder, read by the holder when it unlocks.
	pc uintptr
	at time.Time
	// slow and logf default to slowLockHold and log.Printf; a test sets them.
	slow time.Duration
	logf func(format string, args ...any)
}

func (m *watchedMutex) Lock() {
	m.mu.Lock()
	m.at = time.Now()
	m.pc, _, _, _ = runtime.Caller(1)
}

func (m *watchedMutex) Unlock() {
	held, pc := time.Since(m.at), m.pc
	m.mu.Unlock()
	slow, logf := m.slow, m.logf
	if slow == 0 {
		slow = slowLockHold
	}
	if logf == nil {
		logf = log.Printf
	}
	if held > slow {
		name := "unknown"
		// pc is a return address; the one before it lies inside the caller.
		if fn := runtime.FuncForPC(pc - 1); fn != nil {
			name = fn.Name()
		}
		logf("store lock: held %s by %s", held.Round(time.Millisecond), name)
	}
}
