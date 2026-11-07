package maintenanceui

import (
	"container/list"
	"os"
	"sync"
	"time"

	"github.com/jakenesler/navigatorr/mediainspect"
)

// sourceCacheSize bounds the files whose reads are kept: enough for a user
// clicking through a few seasons, a few megabytes at most.
const sourceCacheSize = 256

// sourceStamp is what a cached read belongs to. A file rewritten or replaced
// since has another size or modification time, and is read afresh.
type sourceStamp struct {
	size int64
	mod  time.Time
}

func stampOf(info os.FileInfo) sourceStamp {
	return sourceStamp{size: info.Size(), mod: info.ModTime()}
}

type sourceRead struct {
	path    string
	stamp   sourceStamp
	report  *mediainspect.DetailedReport
	dynamic *mediainspect.DynamicRange
}

// sourceReadCache keeps what the goal form's reads found in each file (the
// probe, and the dynamic range its decoded frames gave), so opening the same
// file again answers without touching it. The least recently used file goes
// first. Callers only read a kept report, never change it.
type sourceReadCache struct {
	mu      sync.Mutex
	entries map[string]*list.Element
	order   list.List
}

// get returns the read kept for path, or nil when there is none for this
// stamp. A stale entry is dropped.
func (c *sourceReadCache) get(path string, stamp sourceStamp) *sourceRead {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.entries[path]
	if !ok {
		return nil
	}
	read := el.Value.(*sourceRead)
	if read.stamp != stamp {
		c.order.Remove(el)
		delete(c.entries, path)
		return nil
	}
	c.order.MoveToFront(el)
	out := *read
	return &out
}

// putReport keeps a probe of path at stamp, replacing any older read of it.
func (c *sourceReadCache) putReport(path string, stamp sourceStamp, rep mediainspect.DetailedReport) {
	c.update(path, stamp, func(read *sourceRead) { read.report = &rep })
}

// putDynamic keeps the dynamic range classified for path at stamp.
func (c *sourceReadCache) putDynamic(path string, stamp sourceStamp, d mediainspect.DynamicRange) {
	c.update(path, stamp, func(read *sourceRead) { read.dynamic = &d })
}

func (c *sourceReadCache) update(path string, stamp sourceStamp, set func(*sourceRead)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[string]*list.Element)
	}
	if el, ok := c.entries[path]; ok {
		read := el.Value.(*sourceRead)
		if read.stamp != stamp {
			*read = sourceRead{path: path, stamp: stamp}
		}
		set(read)
		c.order.MoveToFront(el)
		return
	}
	read := &sourceRead{path: path, stamp: stamp}
	set(read)
	c.entries[path] = c.order.PushFront(read)
	for c.order.Len() > sourceCacheSize {
		oldest := c.order.Back()
		c.order.Remove(oldest)
		delete(c.entries, oldest.Value.(*sourceRead).path)
	}
}
