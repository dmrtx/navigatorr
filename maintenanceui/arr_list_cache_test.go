package maintenanceui

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestArrListShowsAnOldListAtOnceWhileReadingANewOne(t *testing.T) {
	var c arrListCache
	var calls atomic.Int32
	release := make(chan struct{})
	fetch := func(*serverTiming) ([]map[string]any, error) {
		n := calls.Add(1)
		if n > 1 {
			<-release
		}
		return []map[string]any{{"version": float64(n)}}, nil
	}
	first, hit, err := c.get("radarr", false, newServerTiming(), fetch)
	if err != nil || hit || first[0]["version"] != float64(1) {
		t.Fatal(first, hit, err)
	}
	// Past the TTL but not too old: shown at once, the new read runs behind it.
	e := c.entry("radarr")
	e.mu.Lock()
	e.at = time.Now().Add(-2 * arrListTTL)
	e.mu.Unlock()
	for i := 0; i < 3; i++ {
		old, hit, err := c.get("radarr", false, newServerTiming(), fetch)
		if err != nil || !hit || old[0]["version"] != float64(1) {
			t.Fatal("an old list must be shown without waiting:", old, hit, err)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for calls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if calls.Load() != 2 {
		t.Fatal("expected exactly one background read, got", calls.Load())
	}
	close(release)
	for {
		fresh, _, err := c.get("radarr", false, newServerTiming(), fetch)
		if err != nil || len(fresh) != 1 {
			t.Fatal(fresh, err)
		}
		if fresh[0]["version"] == float64(2) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the background read never landed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if calls.Load() != 2 {
		t.Fatal("the new list was read again:", calls.Load())
	}
}

func TestArrListNeverShowsAnOldListAfterAChangeOrARefreshOrWhenTooOld(t *testing.T) {
	var c arrListCache
	var calls atomic.Int32
	fetch := func(*serverTiming) ([]map[string]any, error) {
		return []map[string]any{{"version": float64(calls.Add(1))}}, nil
	}
	age := func(by time.Duration) {
		e := c.entry("sonarr")
		e.mu.Lock()
		e.at = time.Now().Add(-by)
		e.mu.Unlock()
	}
	c.get("sonarr", false, newServerTiming(), fetch)
	age(2 * arrListTTL)
	c.forget()
	if got, hit, _ := c.get("sonarr", false, newServerTiming(), fetch); hit || got[0]["version"] != float64(2) {
		t.Fatal("a change must not be hidden behind an old list:", got, hit)
	}
	age(2 * arrListTTL)
	if got, hit, _ := c.get("sonarr", true, newServerTiming(), fetch); hit || got[0]["version"] != float64(3) {
		t.Fatal("refresh must read a new list:", got, hit)
	}
	age(2 * arrListStale)
	if got, hit, _ := c.get("sonarr", false, newServerTiming(), fetch); hit || got[0]["version"] != float64(4) {
		t.Fatal("a list too old must be read again first:", got, hit)
	}
}

func TestArrListKeepsTheOldListWhenTheBackgroundReadFails(t *testing.T) {
	var c arrListCache
	var fail atomic.Bool
	var calls atomic.Int32
	fetch := func(*serverTiming) ([]map[string]any, error) {
		calls.Add(1)
		if fail.Load() {
			return nil, errors.New("down")
		}
		return []map[string]any{{"ok": true}}, nil
	}
	c.get("radarr", false, newServerTiming(), fetch)
	e := c.entry("radarr")
	e.mu.Lock()
	e.at = time.Now().Add(-2 * arrListTTL)
	e.mu.Unlock()
	fail.Store(true)
	if got, hit, err := c.get("radarr", false, newServerTiming(), fetch); err != nil || !hit || got[0]["ok"] != true {
		t.Fatal(got, hit, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		e.mu.Lock()
		done := !e.refreshing
		e.mu.Unlock()
		if done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the background read never ended")
		}
		time.Sleep(5 * time.Millisecond)
	}
	before := calls.Load()
	if got, hit, err := c.get("radarr", false, newServerTiming(), fetch); err != nil || !hit || got[0]["ok"] != true || calls.Load() != before {
		t.Fatal("after a failed read the old list is kept and not asked again at once:", got, hit, err)
	}
}

func waitRefreshEnd(t *testing.T, e *arrListEntry) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		e.mu.Lock()
		done := !e.refreshing
		e.mu.Unlock()
		if done {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the background read never ended")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestArrListSurfacesAnErrorOnceTheOldListIsTooOldAndRetriesAfterAPause(t *testing.T) {
	var c arrListCache
	var fail atomic.Bool
	var calls atomic.Int32
	fetch := func(*serverTiming) ([]map[string]any, error) {
		calls.Add(1)
		if fail.Load() {
			return nil, errors.New("down")
		}
		return []map[string]any{{"ok": true}}, nil
	}
	c.get("radarr", false, newServerTiming(), fetch)
	e := c.entry("radarr")
	age := func(by time.Duration) {
		e.mu.Lock()
		e.at = time.Now().Add(-by)
		e.mu.Unlock()
	}
	age(2 * arrListTTL)
	fail.Store(true)
	c.get("radarr", false, newServerTiming(), fetch)
	waitRefreshEnd(t, e)
	// After the pause the background read is tried again.
	e.mu.Lock()
	e.retryAfter = time.Now().Add(-time.Second)
	e.mu.Unlock()
	before := calls.Load()
	c.get("radarr", false, newServerTiming(), fetch)
	waitRefreshEnd(t, e)
	if calls.Load() != before+1 {
		t.Fatal("a failed background read was not tried again after the pause:", calls.Load(), before)
	}
	// Failing for good does not keep an old list alive: past arrListStale the error shows.
	age(2 * arrListStale)
	if _, _, err := c.get("radarr", false, newServerTiming(), fetch); err == nil {
		t.Fatal("an error must surface once the old list is too old to show")
	}
}

func TestArrListBackgroundReadLeavesARenewedOrEndedListAlone(t *testing.T) {
	var c arrListCache
	var calls atomic.Int32
	fetch := func(*serverTiming) ([]map[string]any, error) {
		return []map[string]any{{"version": float64(calls.Add(1))}}, nil
	}
	c.get("radarr", false, newServerTiming(), fetch)
	e := c.entry("radarr")
	e.mu.Lock()
	e.at = time.Now().Add(-2 * arrListTTL)
	e.mu.Unlock()
	// The background read is scheduled, but a refresh renews the list before it runs.
	e.fetchMu.Lock()
	go c.get("radarr", false, newServerTiming(), fetch)
	deadline := time.Now().Add(5 * time.Second)
	for {
		e.mu.Lock()
		started := e.refreshing
		e.mu.Unlock()
		if started {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the background read never started")
		}
		time.Sleep(5 * time.Millisecond)
	}
	e.mu.Lock()
	e.items, e.at = []map[string]any{{"version": float64(99)}}, time.Now()
	e.mu.Unlock()
	e.fetchMu.Unlock()
	waitRefreshEnd(t, e)
	e.mu.Lock()
	got := e.items[0]["version"]
	e.mu.Unlock()
	if got != float64(99) || calls.Load() != 1 {
		t.Fatal("a renewed list was read again or replaced:", got, calls.Load())
	}
}

func TestArrListTogetherRequestsShareOneRead(t *testing.T) {
	var c arrListCache
	var calls atomic.Int32
	fetch := func(*serverTiming) ([]map[string]any, error) {
		calls.Add(1)
		time.Sleep(20 * time.Millisecond)
		return []map[string]any{{"ok": true}}, nil
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.get("sonarr", false, newServerTiming(), fetch)
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatal("requests that arrived together read the *arr", calls.Load(), "times")
	}
	e := c.entry("sonarr")
	e.mu.Lock()
	e.at = time.Now().Add(-2 * arrListTTL)
	e.mu.Unlock()
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.get("sonarr", false, newServerTiming(), fetch)
		}()
	}
	wg.Wait()
	waitRefreshEnd(t, e)
	if calls.Load() != 2 {
		t.Fatal("old-list requests started more than one background read:", calls.Load())
	}
}
