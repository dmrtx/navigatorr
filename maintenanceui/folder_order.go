package maintenanceui

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// A size scan can finish between pages. Freeze the listing for this browse so
// pagination and size polling never move or duplicate rows underneath the user.
type folderListing struct {
	Path, Query, Order string
	Items              []folderItem
	Created            time.Time
	Pending            bool
}
type folderListingCache struct {
	mu      sync.Mutex
	Entries map[string]folderListing
}

func (c *folderListingCache) save(list folderListing) (string, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Entries == nil {
		c.Entries = map[string]folderListing{}
	}
	total := len(list.Items)
	for key, item := range c.Entries {
		if time.Since(item.Created) > 15*time.Minute {
			delete(c.Entries, key)
		} else {
			total += len(item.Items)
		}
	}
	for len(c.Entries) >= 32 || total > 64000 {
		oldest := ""
		for key, item := range c.Entries {
			if oldest == "" || item.Created.Before(c.Entries[oldest].Created) {
				oldest = key
			}
		}
		if oldest == "" {
			return "", fmt.Errorf("folder is too large to sort; choose a smaller folder")
		}
		total -= len(c.Entries[oldest].Items)
		delete(c.Entries, oldest)
	}
	key := hex.EncodeToString(nonce[:])
	list.Created = time.Now()
	c.Entries[key] = list
	return key, nil
}
func (c *folderListingCache) load(key, path, query, order string) (folderListing, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	list, ok := c.Entries[key]
	if !ok || time.Since(list.Created) > 15*time.Minute || list.Path != path || list.Query != query || list.Order != order {
		return folderListing{}, fmt.Errorf("folder listing expired or changed; reload files")
	}
	return list, nil
}
func sortFolderItems(items []folderItem, order string) {
	size := func(item folderItem) (int64, bool) {
		if !item.IsDir {
			return item.Size, true
		}
		if item.FolderSize != nil && item.FolderSize.Status == "ready" && item.FolderSize.Bytes != nil {
			return *item.FolderSize.Bytes, true
		}
		return 0, false
	}
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if order == "size_desc" || order == "size_asc" {
			av, ak := size(a)
			bv, bk := size(b)
			if ak != bk {
				return ak
			}
			if av != bv {
				if order == "size_desc" {
					return av > bv
				}
				return av < bv
			}
		}
		return strings.ToLower(a.Path) < strings.ToLower(b.Path)
	})
}
