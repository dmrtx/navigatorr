package fsop

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func put(t *testing.T, path string, size int, age time.Duration) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-age)
	os.Chtimes(path, old, old)
}

func ageDirs(t *testing.T, root string, age time.Duration) {
	t.Helper()
	old := time.Now().Add(-age)
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			os.Chtimes(p, old, old)
		}
		return nil
	})
}

func leftoverFixture(t *testing.T) (*Resolver, string) {
	t.Helper()
	r, root, _ := testResolver(t)
	day := 48 * time.Hour
	put(t, filepath.Join(root, "Show S01E01", "Show.nfo"), 10, day)
	put(t, filepath.Join(root, "Show S01E01", "Screens", "a.png"), 10, day)
	put(t, filepath.Join(root, "Show S01E01", "Torrent Downloaded From site.txt"), 5, day)
	os.MkdirAll(filepath.Join(root, "Empty One", "inner"), 0o755)
	put(t, filepath.Join(root, "Has Video", "ep.mkv"), 100, day)
	put(t, filepath.Join(root, "Has Video", "ep.nfo"), 1, day)
	put(t, filepath.Join(root, "Partial", "x.mkv.!qB"), 1, day)
	put(t, filepath.Join(root, "Subs", "ep.srt"), 1, day)
	put(t, filepath.Join(root, "Subs", "ep.nfo"), 1, day)
	put(t, filepath.Join(root, ".navigatorr-candidates", "c.nfo"), 1, day)
	put(t, filepath.Join(root, "recovery", "782", "x.nfo"), 1, day)
	put(t, filepath.Join(root, "Fresh", "a.nfo"), 1, 0)
	os.MkdirAll(filepath.Join(root, "Incomplete", "dead torrent"), 0o755)
	ageDirs(t, root, day)
	// Fresh was just written, so its folder is recent.
	now := time.Now()
	os.Chtimes(filepath.Join(root, "Fresh"), now, now)
	return r, root
}

func TestScanLeftoversClassifiesAndKeeps(t *testing.T) {
	r, root := leftoverFixture(t)
	p := LeftoverPolicy{Now: time.Now(), MinAge: time.Hour, Keep: []string{filepath.Join(root, "Incomplete")}}
	scan, err := r.ScanLeftovers(root, p, 3, 100)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]LeftoverClass{}
	for _, f := range scan.Found {
		rel, _ := filepath.Rel(root, f.Path)
		got[rel] = f.Class
	}
	want := map[string]LeftoverClass{
		"Show S01E01":             LeftoverJunk,
		"Empty One":               LeftoverEmpty,
		"Subs":                    LeftoverJunkSubtitles,
		"Incomplete/dead torrent": LeftoverEmpty,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: class %q, want %q (all: %v)", k, got[k], v, got)
		}
	}
	for _, never := range []string{"Has Video", "Partial", ".navigatorr-candidates", "recovery", "Fresh", "Incomplete"} {
		if _, ok := got[never]; ok {
			t.Errorf("%s must be kept, was reported", never)
		}
	}
	if scan.KeptCounts[KeptUnknownFile] != 1 || scan.KeptCounts[KeptPartial] != 1 || scan.KeptCounts[KeptRecent] != 1 || scan.KeptCounts[KeptClientFolder] != 1 {
		t.Errorf("kept counts: %v", scan.KeptCounts)
	}
}

func TestOwnedFoldersAreKept(t *testing.T) {
	r, root := leftoverFixture(t)
	// A torrent whose content is a file inside the empty folder keeps it and
	// the folder above it; a torrent owning the junk folder keeps that too.
	p := LeftoverPolicy{Now: time.Now(), MinAge: time.Hour, Owned: []string{
		filepath.Join(root, "Empty One", "inner", "file.mkv"),
		filepath.Join(root, "SHOW S01E01"), // other case on a case-folding share
	}}
	scan, _ := r.ScanLeftovers(root, p, 3, 100)
	for _, f := range scan.Found {
		rel, _ := filepath.Rel(root, f.Path)
		if rel == "Empty One" || rel == "Show S01E01" {
			t.Errorf("%s is owned by a download client but was reported", rel)
		}
	}
	rem := r.RemoveLeftover(filepath.Join(root, "Empty One"), true, false, p)
	if rem.Status != "skipped" || rem.Reason != KeptOwned {
		t.Errorf("owned removal: %+v", rem)
	}
}

func TestRemoveLeftoverModes(t *testing.T) {
	r, root := leftoverFixture(t)
	p := LeftoverPolicy{Now: time.Now(), MinAge: time.Hour}

	if rem := r.RemoveLeftover(filepath.Join(root, "Show S01E01"), true, false, p); rem.Status != "skipped" {
		t.Errorf("empty-only mode removed a folder with files: %+v", rem)
	}
	if rem := r.RemoveLeftover(filepath.Join(root, "Empty One"), true, false, p); rem.Status != "removed" || rem.Dirs != 2 {
		t.Errorf("empty folder: %+v", rem)
	}
	rem := r.RemoveLeftover(filepath.Join(root, "Show S01E01"), false, false, p)
	if rem.Status != "removed" || rem.Files != 3 || rem.Bytes != 25 {
		t.Errorf("junk folder: %+v", rem)
	}
	if rem := r.RemoveLeftover(filepath.Join(root, "Subs"), false, false, p); rem.Status != "skipped" {
		t.Errorf("subtitles removed without asking: %+v", rem)
	}
	if rem := r.RemoveLeftover(filepath.Join(root, "Subs"), false, true, p); rem.Status != "removed" {
		t.Errorf("subtitles with include_subtitles: %+v", rem)
	}
	if rem := r.RemoveLeftover(filepath.Join(root, "Empty One"), true, false, p); rem.Status != "already_absent" {
		t.Errorf("second removal should be a no-op: %+v", rem)
	}
	for _, keep := range []string{"Has Video/ep.mkv", "Partial/x.mkv.!qB", ".navigatorr-candidates/c.nfo", "recovery/782/x.nfo", "Fresh/a.nfo"} {
		if _, err := os.Stat(filepath.Join(root, keep)); err != nil {
			t.Errorf("%s was removed: %v", keep, err)
		}
	}
	for _, refused := range []string{"Has Video", "Partial", ".navigatorr-candidates", "recovery", "Fresh"} {
		if rem := r.RemoveLeftover(filepath.Join(root, refused), false, true, p); rem.Status != "skipped" {
			t.Errorf("%s: %+v", refused, rem)
		}
	}
}

func TestRemoveLeftoverRefusesRootsSymlinksAndOutside(t *testing.T) {
	r, root, outside := testResolver(t)
	put(t, filepath.Join(outside, "other", "a.nfo"), 1, 48*time.Hour)
	ageDirs(t, outside, 48*time.Hour)
	if err := os.Symlink(filepath.Join(outside, "other"), filepath.Join(root, "link")); err != nil {
		t.Skip("symlinks unavailable")
	}
	os.MkdirAll(filepath.Join(root, "real"), 0o755)
	if err := os.Symlink(filepath.Join(root, "real"), filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	ageDirs(t, root, 48*time.Hour)
	p := LeftoverPolicy{Now: time.Now(), MinAge: time.Hour}

	for name, path := range map[string]string{
		"root":        root,
		"outside":     filepath.Join(outside, "other"),
		"symlink":     filepath.Join(root, "link"),
		"via symlink": filepath.Join(root, "alias"),
	} {
		if rem := r.RemoveLeftover(path, false, true, p); rem.Status == "removed" {
			t.Errorf("%s was removed: %+v", name, rem)
		}
	}
	if _, err := os.Stat(filepath.Join(outside, "other", "a.nfo")); err != nil {
		t.Errorf("file behind a symlink was touched: %v", err)
	}
	if _, err := os.Stat(root); err != nil {
		t.Errorf("root gone: %v", err)
	}
}

func TestRemoveLeftoverStopsWhenAFileAppears(t *testing.T) {
	r, root := leftoverFixture(t)
	p := LeftoverPolicy{Now: time.Now(), MinAge: time.Hour}
	dir := filepath.Join(root, "Show S01E01")
	// The folder looked like junk when listed; a video shows up in a
	// subfolder before removal. The classification inside RemoveLeftover
	// refuses, and the video stays.
	put(t, filepath.Join(dir, "Screens", "late.mkv"), 50, 48*time.Hour)
	ageDirs(t, root, 48*time.Hour)
	rem := r.RemoveLeftover(dir, false, false, p)
	if rem.Status != "skipped" {
		t.Errorf("%+v", rem)
	}
	if _, err := os.Stat(filepath.Join(dir, "Screens", "late.mkv")); err != nil {
		t.Errorf("video removed: %v", err)
	}
}

func TestScanLeftoversLimitAndBudget(t *testing.T) {
	r, root, _ := testResolver(t)
	for _, n := range []string{"a", "b", "c"} {
		os.MkdirAll(filepath.Join(root, n), 0o755)
	}
	ageDirs(t, root, 48*time.Hour)
	scan, err := r.ScanLeftovers(root, LeftoverPolicy{Now: time.Now(), MinAge: time.Hour}, 1, 2)
	if err != nil || len(scan.Found) != 2 || !scan.Truncated {
		t.Errorf("limit: %+v %v", scan, err)
	}
	if _, err := r.ScanLeftovers(filepath.Join(root, "missing"), LeftoverPolicy{}, 1, 2); err == nil {
		t.Error("missing scan root accepted")
	}
	small := LeftoverPolicy{Now: time.Now(), MinAge: time.Hour, MaxEntries: 1}
	put(t, filepath.Join(root, "big", "1.nfo"), 1, 48*time.Hour)
	put(t, filepath.Join(root, "big", "2.nfo"), 1, 48*time.Hour)
	ageDirs(t, root, 48*time.Hour)
	scan, _ = r.ScanLeftovers(root, small, 1, 10)
	if scan.KeptCounts[KeptTooLarge] != 1 {
		t.Errorf("budget: %v", scan.KeptCounts)
	}
}

func TestKeepAndOwnedFollowSymlinks(t *testing.T) {
	base := t.TempDir()
	pool := filepath.Join(base, "pool", "downloads")
	os.MkdirAll(filepath.Join(pool, "movies"), 0o755)
	link := filepath.Join(base, "dl")
	if err := os.Symlink(pool, link); err != nil {
		t.Skip("symlinks unavailable")
	}
	r, err := NewResolver([]string{link}, []string{link})
	if err != nil {
		t.Fatal(err)
	}
	ageDirs(t, pool, 48*time.Hour)
	real, _ := filepath.EvalSymlinks(filepath.Join(link, "movies"))
	// The client names its folder through the link; the resolver's roots are
	// resolved, so only the widened policy matches.
	raw := LeftoverPolicy{Now: time.Now(), MinAge: time.Hour, Keep: []string{filepath.Join(link, "movies")}}
	if rem := r.RemoveLeftover(real, true, false, raw.WithResolvedLinks()); rem.Status != "skipped" || rem.Reason != KeptClientFolder {
		t.Errorf("keep path through a symlink: %+v", rem)
	}
	own := LeftoverPolicy{Now: time.Now(), MinAge: time.Hour, Owned: []string{filepath.Join(link, "movies")}}
	if rem := r.RemoveLeftover(real, true, false, own.WithResolvedLinks()); rem.Status != "skipped" || rem.Reason != KeptOwned {
		t.Errorf("owned path through a symlink: %+v", rem)
	}
	scan, _ := r.ScanLeftovers(link, raw.WithResolvedLinks(), 2, 10)
	if len(scan.Found) != 0 {
		t.Errorf("kept folder listed: %+v", scan.Found)
	}
}

func TestRemoveLeftoverRefusesProtectedAncestors(t *testing.T) {
	r, root, _ := testResolver(t)
	for _, f := range []string{".voxsort-trash/Movie/a.nfo", "#recycle/x/a.nfo", "recovery/782/a.nfo", "@eaDir/x/a.nfo"} {
		put(t, filepath.Join(root, f), 1, 48*time.Hour)
	}
	ageDirs(t, root, 48*time.Hour)
	p := LeftoverPolicy{Now: time.Now(), MinAge: time.Hour}
	for _, f := range []string{".voxsort-trash/Movie", "#recycle/x", "recovery/782", "@eaDir/x"} {
		if rem := r.RemoveLeftover(filepath.Join(root, f), false, true, p); rem.Status != "skipped" || rem.Reason != KeptProtectedName {
			t.Errorf("%s: %+v", f, rem)
		}
		if _, err := os.Stat(filepath.Join(root, f, "a.nfo")); err != nil {
			t.Errorf("%s was touched: %v", f, err)
		}
	}
}

func TestRemoveLeftoverKeepsAFolderHoldingARoot(t *testing.T) {
	base := t.TempDir()
	inner := filepath.Join(base, "incoming", "new")
	os.MkdirAll(inner, 0o755)
	ageDirs(t, base, 48*time.Hour)
	r, err := NewResolver([]string{inner}, []string{base})
	if err != nil {
		t.Fatal(err)
	}
	p := LeftoverPolicy{Now: time.Now(), MinAge: time.Hour}
	if rem := r.RemoveLeftover(filepath.Join(base, "incoming"), true, false, p); rem.Status != "skipped" || rem.Reason != KeptRoot {
		t.Errorf("%+v", rem)
	}
	if _, err := os.Stat(inner); err != nil {
		t.Errorf("a root was removed: %v", err)
	}
}

func TestRemoveLeftoverDetectsAFolderSwappedForASymlink(t *testing.T) {
	r, root, _ := testResolver(t)
	day := 48 * time.Hour
	put(t, filepath.Join(root, "Old", "a.nfo"), 1, day)
	put(t, filepath.Join(root, "Peli", "info.nfo"), 1, day)
	os.MkdirAll(filepath.Join(root, "Peli", "Subs"), 0o755)
	ageDirs(t, root, day)
	beforeOpenHook = func() {
		os.RemoveAll(filepath.Join(root, "Old"))
		os.Symlink("Peli", filepath.Join(root, "Old"))
	}
	defer func() { beforeOpenHook = nil }()
	rem := r.RemoveLeftover(filepath.Join(root, "Old"), false, false, LeftoverPolicy{Now: time.Now(), MinAge: time.Hour})
	if rem.Status == "removed" {
		t.Errorf("a swapped folder was removed: %+v", rem)
	}
	for _, f := range []string{"Peli/info.nfo", "Peli/Subs"} {
		if _, err := os.Stat(filepath.Join(root, f)); err != nil {
			t.Errorf("%s was removed through the swapped symlink: %v", f, err)
		}
	}
}

func TestWriteRootOfPicksTheLongestRoot(t *testing.T) {
	base := t.TempDir()
	inner := filepath.Join(base, ".dl")
	os.MkdirAll(filepath.Join(inner, "Old"), 0o755)
	ageDirs(t, base, 48*time.Hour)
	r, err := NewResolver([]string{base, inner}, []string{base, inner})
	if err != nil {
		t.Fatal(err)
	}
	if rem := r.RemoveLeftover(filepath.Join(inner, "Old"), true, false, LeftoverPolicy{Now: time.Now(), MinAge: time.Hour}); rem.Status != "removed" {
		t.Errorf("%+v", rem)
	}
}
