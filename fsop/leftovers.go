package fsop

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Leftovers are folders a finished download left behind: nothing in them but
// empty folders, torrent junk (.nfo, screenshots, "downloaded from" notes) and,
// kept apart, loose subtitles. Nothing here ever removes a media file, a
// partial download, a symlink or a folder a download client still owns; a
// folder is either wholly leftover or it is kept, with the reason.

// LeftoverClass says what is left in a leftover folder.
type LeftoverClass string

const (
	// LeftoverEmpty holds no file at all, only (possibly nested) empty folders.
	LeftoverEmpty LeftoverClass = "empty"
	// LeftoverJunk holds only junk files.
	LeftoverJunk LeftoverClass = "junk_only"
	// LeftoverJunkSubtitles holds junk plus loose subtitles, which can be the
	// only copy of a good subtitle, so they are removed only when asked for.
	LeftoverJunkSubtitles LeftoverClass = "junk_with_subtitles"
)

// Why a folder is kept.
const (
	KeptUnknownFile    = "media_or_unknown_file"
	KeptPartial        = "partial_download"
	KeptSymlink        = "symlink"
	KeptSpecialFile    = "special_file"
	KeptProtectedName  = "protected_name"
	KeptRecent         = "recently_modified"
	KeptOwned          = "owned_by_download_client"
	KeptClientFolder   = "download_client_folder"
	KeptRoot           = "allowed_root"
	KeptTooLarge       = "too_large_to_inspect"
	KeptUnreadable     = "unreadable"
	defaultInspectCap  = 5000
	defaultScanDirsCap = 4000
)

// LeftoverPolicy carries what the caller knows about live downloads.
type LeftoverPolicy struct {
	// Now and MinAge: anything touched after Now-MinAge is kept.
	Now    time.Time
	MinAge time.Duration
	// Keep lists folders that must stay: neither they nor their ancestors are
	// removed (a client's save paths and category folders).
	Keep []string
	// Owned lists paths a download client is working on: neither they, nor
	// anything inside them, nor any folder holding them is removed.
	Owned []string
	// MaxEntries bounds how many entries one folder may hold to be inspected.
	MaxEntries int
}

// Leftover is one removable folder.
type Leftover struct {
	Path      string        `json:"path"`
	Class     LeftoverClass `json:"class"`
	Dirs      int           `json:"dirs"`
	Files     int           `json:"files"`
	Bytes     int64         `json:"bytes"`
	Newest    string        `json:"newest_change"`
	Sample    []string      `json:"files_sample,omitempty"`
	Subtitles []string      `json:"subtitles,omitempty"`
}

// KeptDir is a top-level folder of a scan that stays, and why.
type KeptDir struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// LeftoverScan is the read-only answer.
type LeftoverScan struct {
	Root       string         `json:"root"`
	Found      []Leftover     `json:"leftovers"`
	Kept       []KeptDir      `json:"kept,omitempty"`
	KeptCounts map[string]int `json:"kept_counts,omitempty"`
	Truncated  bool           `json:"truncated"`
}

// LeftoverRemoval is the outcome for one requested folder.
type LeftoverRemoval struct {
	Path   string `json:"path"`
	Status string `json:"status"` // removed, already_absent, skipped, failed
	Reason string `json:"reason,omitempty"`
	Dirs   int    `json:"dirs,omitempty"`
	Files  int    `json:"files,omitempty"`
	Bytes  int64  `json:"bytes,omitempty"`
}

var junkExt = map[string]bool{
	".nfo": true, ".png": true, ".jpg": true, ".jpeg": true, ".txt": true,
	".sfv": true, ".m3u": true, ".m3u8": true, ".url": true, ".md5": true,
}

var junkName = map[string]bool{
	".ds_store": true, "thumbs.db": true, "desktop.ini": true,
}

var subtitleExt = map[string]bool{
	".srt": true, ".ass": true, ".ssa": true, ".sub": true, ".idx": true, ".vtt": true,
}

var partialExt = map[string]bool{
	".!qb": true, ".parts": true, ".part": true, ".partial": true, ".crdownload": true, ".aria2": true,
}

// protectedName reports a name that is never entered or removed: hidden or
// system folders (.navigatorr-candidates, .voxsort-trash, .oz-media-migration,
// @eaDir, #recycle) and a recovery folder.
func protectedName(name string) bool {
	if name == "" {
		return true
	}
	switch name[0] {
	case '.', '@', '#':
		return true
	}
	return strings.EqualFold(name, "recovery")
}

type fileKind int

const (
	kindJunk fileKind = iota
	kindSubtitle
	kindPartial
	kindOther
)

func kindOf(name string) fileKind {
	low := strings.ToLower(name)
	if junkName[low] {
		return kindJunk
	}
	ext := filepath.Ext(low)
	switch {
	case partialExt[ext]:
		return kindPartial
	case subtitleExt[ext]:
		return kindSubtitle
	case junkExt[ext]:
		return kindJunk
	}
	return kindOther
}

func foldUnder(child, parent string) bool {
	c, p := strings.ToLower(filepath.Clean(child)), strings.ToLower(filepath.Clean(parent))
	return c == p || strings.HasPrefix(c, p+string(os.PathSeparator))
}

// blocked reports why the policy keeps dir, or "". Comparison ignores case so
// a share that folds case never hides a match.
func (p *LeftoverPolicy) blocked(dir string) string {
	for _, k := range p.Keep {
		if k != "" && foldUnder(k, dir) {
			return KeptClientFolder
		}
	}
	for _, o := range p.Owned {
		if o != "" && (foldUnder(dir, o) || foldUnder(o, dir)) {
			return KeptOwned
		}
	}
	return ""
}

// WithResolvedLinks adds, for every Keep and Owned path, the name it has once
// symlinks are followed, so a download client that names a folder through a
// link still matches the real folder (the roots and every path here are
// resolved the same way). Call it once per request.
func (p LeftoverPolicy) WithResolvedLinks() LeftoverPolicy {
	widen := func(in []string) []string {
		out := make([]string, 0, len(in))
		seen := map[string]bool{}
		add := func(v string) {
			if v != "" && !seen[v] {
				seen[v] = true
				out = append(out, v)
			}
		}
		for _, v := range in {
			if v == "" {
				continue
			}
			add(filepath.Clean(v))
			add(evalExistingSymlinks(filepath.Clean(v)))
		}
		return out
	}
	p.Keep = widen(p.Keep)
	p.Owned = widen(p.Owned)
	return p
}

// holdsRoot reports a folder that is, or contains, an allowed root.
func (r *Resolver) holdsRoot(real string) bool {
	for _, set := range [][]string{r.readRoots, r.writeRoots} {
		for _, root := range set {
			if foldUnder(root, real) {
				return true
			}
		}
	}
	return false
}

// writeRootOf returns the write root holding real.
func (r *Resolver) writeRootOf(real string) (string, bool) {
	for _, root := range r.writeRoots {
		if real != root && strings.HasPrefix(real, root+string(os.PathSeparator)) {
			return root, true
		}
	}
	return "", false
}

func (p *LeftoverPolicy) maxEntries() int {
	if p.MaxEntries > 0 {
		return p.MaxEntries
	}
	return defaultInspectCap
}

// classifyDir inspects one folder, returning the leftover or the reason it is
// kept. It reads only; the first thing that is not leftover ends the walk.
func (r *Resolver) classifyDir(dir string, p *LeftoverPolicy) (Leftover, string) {
	lf := Leftover{Path: dir}
	if r.holdsRoot(dir) {
		return lf, KeptRoot
	}
	if reason := p.blocked(dir); reason != "" {
		return lf, reason
	}
	var newest time.Time
	budget := p.maxEntries()
	hasSubs := false
	var walk func(d string) string
	walk = func(d string) string {
		entries, err := os.ReadDir(d)
		if err != nil {
			return KeptUnreadable
		}
		if info, err := os.Lstat(d); err == nil && info.ModTime().After(newest) {
			newest = info.ModTime()
		}
		for _, e := range entries {
			if budget--; budget < 0 {
				return KeptTooLarge
			}
			name := e.Name()
			full := filepath.Join(d, name)
			info, err := os.Lstat(full)
			if err != nil {
				return KeptUnreadable
			}
			mode := info.Mode()
			switch {
			case mode&fs.ModeSymlink != 0:
				return KeptSymlink
			case mode.IsDir():
				if protectedName(name) {
					return KeptProtectedName
				}
				lf.Dirs++
				if reason := walk(full); reason != "" {
					return reason
				}
			case mode.IsRegular():
				switch kindOf(name) {
				case kindPartial:
					return KeptPartial
				case kindOther:
					return KeptUnknownFile
				case kindSubtitle:
					hasSubs = true
					rel, _ := filepath.Rel(dir, full)
					lf.Subtitles = append(lf.Subtitles, rel)
				}
				lf.Files++
				lf.Bytes += info.Size()
				if len(lf.Sample) < 8 {
					rel, _ := filepath.Rel(dir, full)
					lf.Sample = append(lf.Sample, rel)
				}
				if info.ModTime().After(newest) {
					newest = info.ModTime()
				}
			default:
				return KeptSpecialFile
			}
		}
		return ""
	}
	if reason := walk(dir); reason != "" {
		return lf, reason
	}
	now := p.Now
	if now.IsZero() {
		now = time.Now()
	}
	if p.MinAge > 0 && now.Sub(newest) < p.MinAge {
		return lf, KeptRecent
	}
	lf.Newest = newest.UTC().Format(time.RFC3339)
	switch {
	case lf.Files == 0:
		lf.Class = LeftoverEmpty
	case hasSubs:
		lf.Class = LeftoverJunkSubtitles
	default:
		lf.Class = LeftoverJunk
	}
	return lf, ""
}

// ScanLeftovers looks for leftover folders below path (not path itself), down
// to maxDepth levels. It reads only. A folder that is leftover is reported
// whole and not entered; one that is kept is entered, since a leftover folder
// can sit inside it.
func (r *Resolver) ScanLeftovers(path string, p LeftoverPolicy, maxDepth, limit int) (LeftoverScan, error) {
	if maxDepth <= 0 {
		maxDepth = 3
	}
	if maxDepth > 4 {
		maxDepth = 4
	}
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	root, err := r.ResolveRead(path)
	if err != nil {
		return LeftoverScan{}, err
	}
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return LeftoverScan{}, fmt.Errorf("%q is not a directory", path)
	}
	out := LeftoverScan{Root: root, KeptCounts: map[string]int{}}
	examined := 0
	var scan func(dir string, depth int) bool
	scan = func(dir string, depth int) bool {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return true
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, e := range entries {
			if !e.IsDir() || protectedName(e.Name()) { // IsDir is false for symlinks
				continue
			}
			if examined++; examined > defaultScanDirsCap {
				out.Truncated = true
				return false
			}
			child := filepath.Join(dir, e.Name())
			lf, reason := r.classifyDir(child, &p)
			if reason == "" {
				if len(out.Found) >= limit {
					out.Truncated = true
					return false
				}
				out.Found = append(out.Found, lf)
				continue
			}
			if depth == 1 {
				out.KeptCounts[reason]++
				if len(out.Kept) < 50 {
					out.Kept = append(out.Kept, KeptDir{Path: child, Reason: reason})
				}
			}
			if depth < maxDepth && reason != KeptSymlink && reason != KeptUnreadable {
				if !scan(child, depth+1) {
					return false
				}
			}
		}
		return true
	}
	scan(root, 1)
	return out, nil
}

// RemoveLeftover removes one leftover folder. onlyEmpty limits it to folders
// that hold no file; withSubtitles also allows loose subtitles. The folder is
// classified again here, immediately before, and everything inside is checked
// again as it is removed: a file that appeared meanwhile stops the removal and
// stays. Only junk files and empty folders are ever unlinked, through a handle
// confined to the write root, so a symlink swapped in meanwhile cannot lead
// outside it.
func (r *Resolver) RemoveLeftover(path string, onlyEmpty, withSubtitles bool, p LeftoverPolicy) LeftoverRemoval {
	res := LeftoverRemoval{Path: path}
	skip := func(reason string) LeftoverRemoval { res.Status, res.Reason = "skipped", reason; return res }
	fail := func(err error) LeftoverRemoval { res.Status, res.Reason = "failed", err.Error(); return res }

	real, err := r.ResolveWrite(path)
	if err != nil {
		return skip(err.Error())
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return skip(err.Error())
	}
	info, err := os.Lstat(abs)
	if os.IsNotExist(err) {
		res.Status = "already_absent"
		return res
	}
	if err != nil {
		return fail(err)
	}
	if info.Mode()&fs.ModeSymlink != 0 || filepath.Clean(abs) != real {
		return skip(KeptSymlink)
	}
	if !info.IsDir() {
		return skip("not a directory")
	}
	wroot, ok := r.writeRootOf(real)
	if !ok {
		return skip("an allowed root cannot be removed")
	}
	rel, err := filepath.Rel(wroot, real)
	if err != nil {
		return skip(err.Error())
	}
	// Hidden and system folders are protected at any level of the path, not
	// only at the end: nothing inside .voxsort-trash or #recycle is touched.
	for _, part := range strings.Split(rel, string(os.PathSeparator)) {
		if protectedName(part) {
			return skip(KeptProtectedName)
		}
	}
	lf, reason := r.classifyDir(real, &p)
	if reason != "" {
		return skip(reason)
	}
	switch {
	case onlyEmpty && lf.Class != LeftoverEmpty:
		return skip("has files; remove_junk_dirs handles it")
	case lf.Class == LeftoverJunkSubtitles && !withSubtitles:
		return skip("has loose subtitles; pass include_subtitles=true to remove them too")
	}

	// Everything below is done through a handle on the write root, so no
	// name, however it is swapped meanwhile, leads outside the allowed root.
	now := p.Now
	if now.IsZero() {
		now = time.Now()
	}
	parent, err := os.OpenRoot(wroot)
	if err != nil {
		return fail(err)
	}
	defer parent.Close()
	if fi, err := parent.Lstat(rel); err != nil || !fi.IsDir() {
		return skip("changed while removing; kept")
	}
	root, err := parent.OpenRoot(rel)
	if err != nil {
		return fail(err)
	}
	defer root.Close()
	var removeTree func(sub string) error
	removeTree = func(dirRel string) error {
		f, err := root.Open(dirRel)
		if err != nil {
			return err
		}
		entries, err := f.ReadDir(-1)
		f.Close()
		if err != nil {
			return err
		}
		for _, e := range entries {
			sub := filepath.Join(dirRel, e.Name())
			fi, err := root.Lstat(sub)
			if err != nil {
				return err
			}
			switch {
			case fi.Mode().IsDir():
				if protectedName(e.Name()) {
					return fmt.Errorf("%s: protected folder appeared", sub)
				}
				if err := removeTree(sub); err != nil {
					return err
				}
				if err := root.Remove(sub); err != nil {
					return err
				}
				res.Dirs++
			case fi.Mode().IsRegular():
				k := kindOf(e.Name())
				switch {
				case onlyEmpty:
					return fmt.Errorf("%s: a file appeared in an empty folder; kept", sub)
				case k == kindPartial || k == kindOther || (k == kindSubtitle && !withSubtitles):
					return fmt.Errorf("%s: not a leftover file; kept", sub)
				case p.MinAge > 0 && now.Sub(fi.ModTime()) < p.MinAge:
					return fmt.Errorf("%s: changed recently; kept", sub)
				}
				if err := root.Remove(sub); err != nil {
					return err
				}
				res.Files++
				res.Bytes += fi.Size()
			default:
				return fmt.Errorf("%s: not a regular file; kept", sub)
			}
		}
		return nil
	}
	if err := removeTree("."); err != nil {
		return fail(err)
	}
	// Removing a folder succeeds only when it is empty.
	if err := parent.Remove(rel); err != nil {
		return fail(err)
	}
	res.Dirs++
	res.Status = "removed"
	return res
}
