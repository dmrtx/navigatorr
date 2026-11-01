package maintenanceui

import (
	"sync"
	"sync/atomic"
	"time"
)

// A library page is 100 titles, but Radarr and Sonarr only answer for the whole
// collection: hundreds of titles and several megabytes each time. The projected
// list is kept for a short while so paging, searching and sorting read memory
// instead of asking again. The entries are shared and must not be modified;
// a caller copies an item before adding anything to it.
const (
	arrListTTL = 45 * time.Second
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
	// mu is held while the list is fetched, so requests that arrive together
	// share one fetch instead of each starting their own.
	mu         sync.Mutex
	at         time.Time
	generation uint64
	items      []map[string]any
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

// get returns the service's list, fetching it when there is none, it is older
// than the TTL, or refresh asks for a new one. hit says it came from memory.
// A failed fetch is an error for this call and leaves the last good list as it was.
func (c *arrListCache) get(service string, refresh bool, fetch func() ([]map[string]any, error)) (items []map[string]any, hit bool, err error) {
	e := c.entry(service)
	e.mu.Lock()
	defer e.mu.Unlock()
	generation := c.generation.Load()
	if !refresh && e.items != nil && e.generation == generation && time.Since(e.at) < arrListTTL {
		return e.items, true, nil
	}
	items, err = fetch()
	if err != nil {
		// The last good list stays for the next request that can use it.
		return nil, false, err
	}
	e.items, e.at, e.generation = items, time.Now(), generation
	return items, false, nil
}

// forget ends every list, such as after something was changed through the console.
func (c *arrListCache) forget() { c.generation.Add(1) }
