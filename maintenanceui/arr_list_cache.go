package maintenanceui

import (
	"sync"
	"sync/atomic"
	"time"
)

// A library page is 100 titles, but Radarr and Sonarr only answer for the whole
// collection: hundreds of titles and several megabytes each time (about half a
// second for Radarr and a second for Sonarr on a large library). The projected
// list is kept so paging, searching and sorting read memory instead of asking
// again. Past the TTL a list that is not too old is still shown at once while
// one request in the background asks again, so no one waits for the *arr to
// find out that nothing changed. A change made through the console, or refresh=1,
// is never answered from an old list. The entries are shared and must not be
// modified; a caller copies an item before adding anything to it.
const (
	arrListTTL = 45 * time.Second
	// arrListStale is how old a list may be and still be shown while a new one is read.
	arrListStale = 10 * time.Minute
	// arrFetchTimeout bounds one fetch of a collection, however many requests wait on it.
	arrFetchTimeout = time.Minute
)

type arrListCache struct {
	mu      sync.Mutex
	entries map[string]*arrListEntry
	// generation moves on when something was changed, which ends every entry
	// fetched before it without waiting for a fetch in progress.
	generation atomic.Uint64
}

type arrListEntry struct {
	// fetchMu is held while the list is fetched, so requests that arrive together
	// share one fetch instead of each starting their own.
	fetchMu sync.Mutex
	// mu guards the fields below and is never held across a fetch.
	mu         sync.Mutex
	at         time.Time
	generation uint64
	items      []map[string]any
	refreshing bool
}

func (c *arrListCache) entry(service string) *arrListEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[string]*arrListEntry{}
	}
	e := c.entries[service]
	if e == nil {
		e = &arrListEntry{}
		c.entries[service] = e
	}
	return e
}

// get returns the service's list, fetching it when there is none, it is too old
// to show, or refresh asks for a new one. hit says it came from memory. A list
// past the TTL but within arrListStale is returned at once and read again in
// the background (with a timing of its own, since the request will be over).
// A failed fetch is an error for this call and leaves the last good list as it was.
func (c *arrListCache) get(service string, refresh bool, timing *serverTiming, fetch func(*serverTiming) ([]map[string]any, error)) (items []map[string]any, hit bool, err error) {
	e := c.entry(service)
	asked := time.Now()
	generation := c.generation.Load()
	e.mu.Lock()
	if !refresh && e.items != nil && e.generation == generation {
		age := time.Since(e.at)
		if age < arrListTTL {
			items = e.items
			e.mu.Unlock()
			return items, true, nil
		}
		if age < arrListStale {
			items = e.items
			start := !e.refreshing
			e.refreshing = true
			e.mu.Unlock()
			if start {
				go c.refresh(e, fetch)
			}
			return items, true, nil
		}
	}
	e.mu.Unlock()

	e.fetchMu.Lock()
	defer e.fetchMu.Unlock()
	// A request that waited behind a fetch already got what it asked for when the
	// list was read after it asked.
	e.mu.Lock()
	if e.items != nil && e.generation == generation && (e.at.After(asked) || (!refresh && time.Since(e.at) < arrListTTL)) {
		items = e.items
		e.mu.Unlock()
		return items, true, nil
	}
	e.mu.Unlock()
	items, err = fetch(timing)
	if err != nil {
		// The last good list stays for the next request that can use it.
		return nil, false, err
	}
	e.mu.Lock()
	e.items, e.at, e.generation = items, time.Now(), generation
	e.mu.Unlock()
	return items, false, nil
}

// refresh reads the list again for the requests that were shown an old one. A
// failure keeps the old list, which is then read again by the next request.
func (c *arrListCache) refresh(e *arrListEntry, fetch func(*serverTiming) ([]map[string]any, error)) {
	e.fetchMu.Lock()
	defer e.fetchMu.Unlock()
	generation := c.generation.Load()
	items, err := fetch(newServerTiming())
	e.mu.Lock()
	defer e.mu.Unlock()
	e.refreshing = false
	if err == nil {
		e.items, e.at, e.generation = items, time.Now(), generation
	} else {
		// Try again in a few seconds instead of on every request.
		e.at = time.Now().Add(-arrListTTL + 10*time.Second)
	}
}

// forget ends every list, such as after something was changed through the console.
func (c *arrListCache) forget() { c.generation.Add(1) }
