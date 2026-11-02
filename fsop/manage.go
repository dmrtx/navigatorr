package fsop

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Folder creation and the removal of one chosen file or folder. Removal is
// always two steps: a plan (read-only) that says exactly what would go and
// carries a digest of it, then a removal that quotes that digest. Anything
// that changed in between, or that something else still depends on, stops it.

const (
	// maxRemoveEntries bounds one removal: a bigger tree is removed in parts.
	maxRemoveEntries = 2000
	// planSampleSize is how many entries a plan lists.
	planSampleSize = 40
	// DefaultRemoveAge keeps anything touched in the last minutes (a copy, an
	// import or a download still writing it).
	DefaultRemoveAge = 10 * time.Minute
	// MinRemoveAge is the smallest age a caller may ask for.
	MinRemoveAge = 5 * time.Minute
)

// Why a removal is blocked.
const (
	BlockedRoot      = "an allowed root, or a folder holding one, cannot be removed"
	BlockedSymlink   = "contains or is a symlink; remove the link itself outside this tool"
	BlockedSpecial   = "contains a file that is neither a regular file nor a folder"
	BlockedProtected = "hidden and system names (.navigatorr-*, .voxsort-trash, @eaDir, #recycle, recovery) are never removed here"
	BlockedTooBig    = "holds more entries than one removal handles; remove its subfolders first"
	BlockedNotEmpty  = "is a folder with files in it; pass recursive=true to remove it with everything inside"
)

// Claim says that something outside the filesystem depends on a path: a
// movie or series folder, or a file a library manager tracks.
type Claim struct {
	Path   string `json:"path"`
	Owner  string `json:"owner"`
	Folder bool   `json:"folder,omitempty"`
}

// RemovePolicy is what the caller knows about what depends on the files.
type RemovePolicy struct {
	// Now and MinAge: anything touched after Now-MinAge is kept.
	Now    time.Time
	MinAge time.Duration
	// Keep and Owned are the download client's folders and files, as in
	// LeftoverPolicy.
	Keep  []string
	Owned []string
	// Tracked are library-manager claims. A tracked file, or a library
	// folder, inside or equal to what is removed blocks the removal.
	Tracked []Claim
}

// PlanEntry is one file or folder a removal would take.
type PlanEntry struct {
	Path string `json:"path"` // relative to the removed path; "." for itself
	Dir  bool   `json:"dir,omitempty"`
	Size int64  `json:"size,omitempty"`
	Mod  string `json:"mod_time"`
}

// RemovePlan says what removing a path would do. It changes nothing.
type RemovePlan struct {
	Path   string      `json:"path"`
	Kind   string      `json:"kind"` // file or folder
	Files  int         `json:"files"`
	Dirs   int         `json:"folders"`
	Bytes  int64       `json:"bytes"`
	Newest string      `json:"newest_change,omitempty"`
	Sample []PlanEntry `json:"sample,omitempty"`
	// Digest names this exact tree (paths, sizes, change times). The removal
	// must quote it, so only what was shown is removed.
	Digest string `json:"plan_digest"`
	// Blocked lists the reasons the removal cannot go ahead; empty means it can.
	Blocked []string `json:"blocked,omitempty"`

	real    string
	wroot   string
	rel     string
	entries []PlanEntry
}

// RemoveResult is the outcome of a removal.
type RemoveResult struct {
	Path   string `json:"path"`
	Status string `json:"status"` // removed, already_absent, skipped or failed
	Reason string `json:"reason,omitempty"`
	Files  int    `json:"files,omitempty"`
	Dirs   int    `json:"folders,omitempty"`
	Bytes  int64  `json:"bytes_freed,omitempty"`
}

// PathsOverlap reports whether a is inside b or b inside a, comparing without
// case and also by the names the paths have once symlinks are followed.
func PathsOverlap(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	forms := func(p string) []string {
		c := filepath.Clean(p)
		if real := evalExistingSymlinks(c); real != c {
			return []string{c, real}
		}
		return []string{c}
	}
	for _, x := range forms(a) {
		for _, y := range forms(b) {
			if foldUnder(x, y) || foldUnder(y, x) {
				return true
			}
		}
	}
	return false
}

// MkdirResult says which folders a Mkdir made.
type MkdirResult struct {
	Path    string   `json:"path"`
	Created []string `json:"created,omitempty"`
	Existed bool     `json:"already_existed,omitempty"`
}

// Mkdir creates a folder and its missing parents inside the write roots, as
// plain directories. A folder that is already there is not an error. The root
// itself must exist and still be a mounted share: a missing one is never
// created, since its folders would land on the local disk.
func (r *Resolver) Mkdir(path string) (MkdirResult, error) {
	res := MkdirResult{}
	real, err := r.ResolveWrite(path)
	if err != nil {
		return res, err
	}
	res.Path = real
	if r.isRoot(real) {
		res.Existed = true
		return res, r.rootsMounted(real)
	}
	rel := ""
	if wroot, ok := r.writeRootOf(real); ok {
		rel, _ = filepath.Rel(wroot, real)
	}
	for _, part := range strings.Split(rel, string(os.PathSeparator)) {
		if part == "" || part == "." || part == ".." {
			return res, fmt.Errorf("invalid folder name in %q", path)
		}
		if part[0] == '.' || strings.EqualFold(part, "@eaDir") || strings.EqualFold(part, "#recycle") {
			return res, fmt.Errorf("%q is a hidden or system name and cannot be created here", part)
		}
	}
	if err := r.rootsMounted(real); err != nil {
		return res, err
	}
	if fi, err := os.Lstat(real); err == nil {
		if !fi.IsDir() {
			return res, fmt.Errorf("%q already exists and is not a folder", path)
		}
		res.Existed = true
		return res, nil
	}
	created, err := mkdirAllReport(real)
	res.Created = created
	if err == nil {
		// Nothing swapped in a link while the folders were made.
		if again, rerr := r.ResolveWrite(real); rerr != nil || again != real {
			err = fmt.Errorf("%q now resolves somewhere else", real)
		}
	}
	if err != nil {
		removeEmptyDirs(created)
		res.Created = nil
		return res, fmt.Errorf("creating folder: %w", err)
	}
	return res, nil
}

// protectedPath reports a part of rel that is hidden or a system name.
func protectedPath(rel string) bool {
	for _, part := range strings.Split(rel, string(os.PathSeparator)) {
		if part != "." && protectedName(part) {
			return true
		}
	}
	return false
}

// PlanRemove describes what removing path would take, and why it may not go
// ahead. It only reads. A path outside the write roots, or one that is not
// there, is an error; everything else that stops a removal is in Blocked.
func (r *Resolver) PlanRemove(path string, recursive bool, p RemovePolicy) (RemovePlan, error) {
	plan := RemovePlan{Path: path}
	real, err := r.ResolveWrite(path)
	if err != nil {
		return plan, err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return plan, err
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return plan, err
	}
	plan.Path = real
	plan.real = real
	block := func(reason string) { plan.Blocked = appendUnique(plan.Blocked, reason) }
	if info.Mode()&fs.ModeSymlink != 0 || filepath.Clean(abs) != real {
		block(BlockedSymlink)
		return plan, nil
	}
	if r.holdsRoot(real) {
		block(BlockedRoot)
		return plan, nil
	}
	wroot, ok := r.writeRootOf(real)
	if !ok {
		block(BlockedRoot)
		return plan, nil
	}
	plan.wroot = wroot
	rel, err := filepath.Rel(wroot, real)
	if err != nil {
		return plan, err
	}
	plan.rel = rel
	if protectedPath(rel) {
		block(BlockedProtected)
		return plan, nil
	}
	if err := r.rootsMounted(real); err != nil {
		block(err.Error())
		return plan, nil
	}

	now := p.Now
	if now.IsZero() {
		now = time.Now()
	}
	var newest time.Time
	add := func(relPath string, fi fs.FileInfo) {
		e := PlanEntry{Path: relPath, Dir: fi.IsDir(), Mod: fi.ModTime().UTC().Format(time.RFC3339Nano)}
		if !fi.IsDir() {
			e.Size = fi.Size()
			plan.Files++
			plan.Bytes += fi.Size()
		} else {
			plan.Dirs++
		}
		if fi.ModTime().After(newest) {
			newest = fi.ModTime()
		}
		plan.entries = append(plan.entries, e)
	}
	var walk func(dirAbs, dirRel string)
	walk = func(dirAbs, dirRel string) {
		entries, err := os.ReadDir(dirAbs)
		if err != nil {
			block("a folder could not be read: " + err.Error())
			return
		}
		for _, e := range entries {
			if len(plan.entries) >= maxRemoveEntries {
				block(BlockedTooBig)
				return
			}
			full := filepath.Join(dirAbs, e.Name())
			relPath := filepath.Join(dirRel, e.Name())
			fi, err := os.Lstat(full)
			if err != nil {
				block("an entry could not be read: " + err.Error())
				return
			}
			switch {
			case fi.Mode()&fs.ModeSymlink != 0:
				block(BlockedSymlink)
			case fi.IsDir():
				if protectedName(e.Name()) {
					block(BlockedProtected)
					continue
				}
				add(relPath, fi)
				walk(full, relPath)
			case fi.Mode().IsRegular():
				add(relPath, fi)
			default:
				block(BlockedSpecial)
			}
		}
	}
	if info.IsDir() {
		plan.Kind = "folder"
		add(".", info)
		walk(real, "")
		if !recursive && plan.Files+plan.Dirs > 1 {
			block(BlockedNotEmpty)
		}
	} else if info.Mode().IsRegular() {
		plan.Kind = "file"
		add(".", info)
	} else {
		plan.Kind = "other"
		block(BlockedSpecial)
		return plan, nil
	}
	if !newest.IsZero() {
		plan.Newest = newest.UTC().Format(time.RFC3339)
		if p.MinAge > 0 && now.Sub(newest) < p.MinAge {
			block(fmt.Sprintf("changed %s ago, less than the %s a file must rest; try again later",
				now.Sub(newest).Round(time.Second), p.MinAge))
		}
	}
	for _, reason := range dependents(real, plan.Kind == "folder", &p) {
		block(reason)
	}

	// The path itself first, then lexicographic: a folder always sorts before
	// what is inside it.
	sort.Slice(plan.entries, func(i, j int) bool {
		a, b := plan.entries[i].Path, plan.entries[j].Path
		if (a == ".") != (b == ".") {
			return a == "."
		}
		return a < b
	})
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\n", plan.Kind, real)
	for _, e := range plan.entries {
		fmt.Fprintf(h, "%s\x00%v\x00%d\x00%s\n", e.Path, e.Dir, e.Size, e.Mod)
	}
	plan.Digest = hex.EncodeToString(h.Sum(nil))
	// The sample lists the biggest files first: they are what a person checks.
	sample := append([]PlanEntry(nil), plan.entries...)
	sort.SliceStable(sample, func(i, j int) bool { return sample[i].Size > sample[j].Size })
	if len(sample) > planSampleSize {
		sample = sample[:planSampleSize]
	}
	plan.Sample = sample
	return plan, nil
}

// dependents returns why a download client or a library manager still needs
// what is at real. isDir tells whether real is a folder.
func dependents(real string, isDir bool, p *RemovePolicy) []string {
	var out []string
	for _, o := range p.Owned {
		if o != "" && PathsOverlap(real, o) {
			out = append(out, "the download client (qBittorrent) still owns "+o)
			break
		}
	}
	for _, k := range p.Keep {
		if k != "" && foldUnder(k, real) {
			out = append(out, "the download client saves into "+k)
			break
		}
	}
	for _, c := range p.Tracked {
		if c.Path == "" {
			continue
		}
		if c.Folder {
			// A library folder that is, or sits inside, what is removed.
			if foldUnder(c.Path, real) {
				out = append(out, fmt.Sprintf("%s holds the library folder %s; remove it through %s", c.Owner, c.Path, c.Owner))
			}
			continue
		}
		if foldUnder(c.Path, real) {
			out = append(out, fmt.Sprintf("%s tracks the file %s; remove it through %s", c.Owner, c.Path, c.Owner))
		}
	}
	return out
}

func appendUnique(list []string, s string) []string {
	for _, v := range list {
		if v == s {
			return list
		}
	}
	return append(list, s)
}

// Remove removes what PlanRemove described, provided the tree still has the
// digest the caller was shown. It plans again here, then removes entry by
// entry through a handle on the write root, innermost first, checking each
// one is still the entry that was planned: a name swapped for a link, or a
// file that changed, stops it and the rest stays.
func (r *Resolver) Remove(path string, recursive bool, digest string, p RemovePolicy) RemoveResult {
	res := RemoveResult{Path: path}
	skip := func(reason string) RemoveResult { res.Status, res.Reason = "skipped", reason; return res }
	fail := func(err error) RemoveResult { res.Status, res.Reason = "failed", err.Error(); return res }

	if digest == "" {
		return skip("plan_digest is required: preview first and quote its plan_digest")
	}
	plan, err := r.PlanRemove(path, recursive, p)
	if err != nil {
		if os.IsNotExist(err) {
			res.Status = "already_absent"
			return res
		}
		return skip(err.Error())
	}
	res.Path = plan.Path
	if len(plan.Blocked) > 0 {
		return skip(strings.Join(plan.Blocked, "; "))
	}
	if plan.Digest != digest {
		return skip("changed since the preview (or a different path); preview again and review before removing")
	}
	parent, err := os.OpenRoot(plan.wroot)
	if err != nil {
		return fail(err)
	}
	defer parent.Close()
	// Children sort after their folder, so the reverse order removes the
	// inside of a folder before the folder itself.
	for i := len(plan.entries) - 1; i >= 0; i-- {
		e := plan.entries[i]
		name := filepath.Join(plan.rel, e.Path)
		fi, err := parent.Lstat(name)
		if err != nil {
			return fail(fmt.Errorf("%s: %w (the rest was kept)", e.Path, err))
		}
		if fi.Mode()&fs.ModeSymlink != 0 || fi.IsDir() != e.Dir ||
			(!e.Dir && (fi.Size() != e.Size || !fi.Mode().IsRegular())) {
			return fail(fmt.Errorf("%s: changed while removing; the rest was kept", e.Path))
		}
		if !e.Dir && fi.ModTime().UTC().Format(time.RFC3339Nano) != e.Mod {
			return fail(fmt.Errorf("%s: changed while removing; the rest was kept", e.Path))
		}
		if err := parent.Remove(name); err != nil {
			return fail(fmt.Errorf("%s: %w (the rest was kept)", e.Path, err))
		}
		if e.Dir {
			res.Dirs++
		} else {
			res.Files++
			res.Bytes += e.Size
		}
	}
	res.Status = "removed"
	return res
}
