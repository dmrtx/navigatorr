package fsop

// Space is how much room a filesystem has. Free counts blocks reserved for
// privileged use; Avail is what an ordinary writer can still use, as df shows.
// Device tells filesystems apart: paths with the same Device share one volume.
type Space struct {
	Total, Free, Avail uint64
	Device             uint64
}

// ReadRoots returns the directories this resolver serves for reading.
func (r *Resolver) ReadRoots() []string { return append([]string(nil), r.readRoots...) }
