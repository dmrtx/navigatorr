package nashelper

import "strings"

const (
	// MaxUsageChildDepth is how many levels below the requested folder a result
	// may report on.
	MaxUsageChildDepth = 2
	// MaxUsageChildren bounds how many subfolders one usage result reports; past
	// it the result carries none, and the caller measures them one by one.
	MaxUsageChildren = 3000
	// maxChildStamps bounds the directories one child stamps. A subfolder with
	// more is reported without stamps and its caller re-measures it on a timer.
	maxChildStamps = 16
	// maxCollectedDirs bounds the directories the walk remembers.
	maxCollectedDirs = 200_000
)

// UsageChild is the size of a folder below the requested one, as if it had been
// asked for on its own. Path is relative to the requested folder, with "/".
type UsageChild struct {
	Path    string           `json:"path"`
	Bytes   int64            `json:"bytes,string"`
	Files   int64            `json:"files,string"`
	Dirs    int64            `json:"dirs,string"`
	Skipped int64            `json:"skipped,string"`
	Stamps  map[string]int64 `json:"stamps,omitempty"`
}

type dirStamp struct {
	rel   string
	mtime int64
}

// usageMark is where a directory's walk began.
type usageMark struct {
	stamp                       int
	bytes, files, dirs, skipped int64
}

// usageCollector gathers, while a walk runs, the stamps of every directory and
// the size of the folders down to a depth the caller asked for, so one pass
// answers for the folder and for what is in it.
type usageCollector struct {
	depth      int
	stamps     []dirStamp
	stampsOK   bool
	children   []UsageChild
	childrenOK bool
}

func newUsageCollector(childDepth int) *usageCollector {
	return &usageCollector{depth: childDepth, stampsOK: true, childrenOK: childDepth > 0}
}

// enter notes a directory about to be walked.
func (c *usageCollector) enter(rel string, mtime int64, res *UsageResult) usageMark {
	mark := usageMark{stamp: len(c.stamps), bytes: res.Bytes, files: res.Files, dirs: res.Dirs, skipped: res.Skipped}
	if c.stampsOK {
		if len(c.stamps) >= maxCollectedDirs {
			c.stampsOK, c.stamps = false, nil
		} else {
			c.stamps = append(c.stamps, dirStamp{rel, mtime})
		}
	}
	return mark
}

// leave closes a directory that sits depth levels below the root.
func (c *usageCollector) leave(mark usageMark, rel string, depth int, res *UsageResult) {
	if !c.childrenOK || depth < 1 || depth > c.depth {
		return
	}
	if len(c.children) >= MaxUsageChildren {
		c.childrenOK, c.children = false, nil
		return
	}
	child := UsageChild{
		Path: rel, Bytes: res.Bytes - mark.bytes, Files: res.Files - mark.files,
		Dirs: res.Dirs - mark.dirs, Skipped: res.Skipped - mark.skipped,
	}
	if c.stampsOK && len(c.stamps)-mark.stamp <= maxChildStamps {
		child.Stamps = make(map[string]int64, len(c.stamps)-mark.stamp)
		for _, s := range c.stamps[mark.stamp:] {
			if s.rel == rel {
				child.Stamps["."] = s.mtime
			} else {
				child.Stamps[strings.TrimPrefix(s.rel, rel+"/")] = s.mtime
			}
		}
	}
	c.children = append(c.children, child)
}

// finish puts what was collected on the result.
func (c *usageCollector) finish(res *UsageResult) {
	if c.stampsOK && len(c.stamps) <= MaxUsageStamps {
		res.Stamps = make(map[string]int64, len(c.stamps))
		for _, s := range c.stamps {
			res.Stamps[s.rel] = s.mtime
		}
	}
	if c.childrenOK {
		res.Children = c.children
	}
}
