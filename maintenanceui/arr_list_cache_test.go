package maintenanceui

import (
	"errors"
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
		fresh, _, _ := c.get("radarr", false, newServerTiming(), fetch)
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
