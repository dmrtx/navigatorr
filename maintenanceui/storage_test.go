package maintenanceui

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/action"
	"github.com/jakenesler/navigatorr/fsop"
	"github.com/jakenesler/navigatorr/internal/nashelper"
	"github.com/jakenesler/navigatorr/store"
)

// fakeMeter stands in for the NAS helper: roots map one to one into /nas.
type fakeMeter struct {
	prefix     string
	volumes    []nashelper.Volume
	volumesErr error
	usage      map[string]int64
	usageErr   error
	children   map[string][]nashelper.UsageChild
	walked     []string
	// list answers List for a folder; without an entry the helper is unavailable.
	list   map[string]nashelper.ListResponse
	listed []string
}

func (f *fakeMeter) Remote(local string) (string, bool) {
	if !strings.HasPrefix(local, f.prefix) {
		return "", false
	}
	return "/nas" + strings.TrimPrefix(local, f.prefix), true
}

func (f *fakeMeter) Volumes(context.Context) (nashelper.VolumeResponse, error) {
	if f.volumesErr != nil {
		return nashelper.VolumeResponse{}, f.volumesErr
	}
	return nashelper.VolumeResponse{InstanceID: "test", Volumes: f.volumes}, nil
}

func (f *fakeMeter) Usage(_ context.Context, local string, _ nashelper.UsageProgress, _ ...nashelper.UsageOption) (nashelper.UsageResult, error) {
	f.walked = append(f.walked, local)
	if f.usageErr != nil {
		return nashelper.UsageResult{}, f.usageErr
	}
	return nashelper.UsageResult{Bytes: f.usage[local], Files: 3, Dirs: 1, Children: f.children[local]}, nil
}

func (f *fakeMeter) List(_ context.Context, local string) (nashelper.ListResponse, error) {
	f.listed = append(f.listed, local)
	if res, ok := f.list[local]; ok {
		return res, nil
	}
	return nashelper.ListResponse{}, nashelper.ErrUnavailable
}

// storageRoots makes two served roots and a UI that serves them.
func storageRoots(t *testing.T, names ...string) (*Server, []string) {
	t.Helper()
	s, _ := testUI(t)
	base := t.TempDir()
	var roots []string
	for _, name := range names {
		p := filepath.Join(base, name)
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
		roots = append(roots, p)
	}
	deps := s.engine.Deps()
	deps.Fs, _ = fsop.NewResolver(roots, nil)
	s.engine = action.NewEngine(deps)
	canonical := deps.Fs.ReadRoots()
	return s, canonical
}

func TestStorageMeasuredByTheHelperFromInsideTheNAS(t *testing.T) {
	s, roots := storageRoots(t, "Anime", "Movies")
	meter := &fakeMeter{
		prefix: filepath.Dir(roots[0]),
		volumes: []nashelper.Volume{{Root: "/nas", FSID: "vol1", FSType: "btrfs",
			TotalBytes: 1000, FreeBytes: 400, AvailBytes: 380}},
		usage: map[string]int64{roots[0]: 300, roots[1]: 200},
	}
	s.nas = meter
	notes, err := s.measureStorage(context.Background())
	if err != nil || len(notes) != 0 {
		t.Fatalf("notes %v err %v", notes, err)
	}
	view, err := s.storageView()
	if err != nil {
		t.Fatal(err)
	}
	if !view.Helper || len(view.Volumes) != 1 || len(view.Folders) != 2 {
		t.Fatalf("view %+v", view)
	}
	v := view.Volumes[0]
	if v.TotalBytes != 1000 || v.UsedBytes != 600 || v.FreeBytes != 380 || v.Source != "helper" {
		t.Fatalf("volume %+v", v)
	}
	if v.AccountedBytes != 500 || v.OtherBytes != 100 || len(v.Roots) != 2 {
		t.Fatalf("accounting %+v", v)
	}
	if view.Folders[0].Path != roots[0] || view.Folders[0].Bytes != 300 || view.Folders[0].Source != "helper" || view.Folders[0].Name != "Anime" {
		t.Fatalf("folders %+v", view.Folders)
	}
}

// The storage round walks every root already. What it learns about the folders
// inside is saved, so the Files tab opens with sizes instead of calculating them.
func TestStorageRoundFillsTheFolderCacheFromTheFoldersInside(t *testing.T) {
	s, roots := storageRoots(t, "Series")
	show := filepath.Join(roots[0], "Cheers")
	if err := os.MkdirAll(filepath.Join(show, "Season 1"), 0700); err != nil {
		t.Fatal(err)
	}
	s.nas = &fakeMeter{
		prefix:  filepath.Dir(roots[0]),
		volumes: []nashelper.Volume{{Root: "/nas", FSID: "v", TotalBytes: 1000, FreeBytes: 500, AvailBytes: 500}},
		usage:   map[string]int64{roots[0]: 80},
		children: map[string][]nashelper.UsageChild{roots[0]: {
			{Path: "Cheers", Bytes: 60, Files: 4, Dirs: 2},
			{Path: "Cheers/Season 1", Bytes: 50, Files: 3, Dirs: 1},
		}},
	}
	if _, err := s.measureStorage(context.Background()); err != nil {
		t.Fatal(err)
	}
	walked := len(s.nas.(*fakeMeter).walked)
	for path, want := range map[string]int64{show: 60, filepath.Join(show, "Season 1"): 50} {
		size := s.folderSizes.get(path, s.engine.Deps().Fs, s.engine.Deps().Store, s.nas)
		if size.Status != "ready" || size.Bytes == nil || *size.Bytes != want || size.Updating {
			t.Fatalf("%s opened without its size: %+v", path, size)
		}
	}
	if after := len(s.nas.(*fakeMeter).walked); after != walked {
		t.Fatalf("opening the folders walked the NAS again: %d walks", after-walked)
	}
}

func TestStorageFallsBackToTheShareWhenTheHelperCannotAnswer(t *testing.T) {
	s, roots := storageRoots(t, "Anime")
	os.WriteFile(filepath.Join(roots[0], "a.mkv"), make([]byte, 40), 0600)
	meter := &fakeMeter{
		prefix:     filepath.Dir(roots[0]),
		volumesErr: fmt.Errorf("%w: down", nashelper.ErrUnavailable),
		usageErr:   fmt.Errorf("%w: down", nashelper.ErrUnavailable),
	}
	s.nas = meter
	notes, err := s.measureStorage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) == 0 || !strings.Contains(notes[0], "helper") {
		t.Fatalf("the fallback was not reported: %v", notes)
	}
	view, _ := s.storageView()
	if len(view.Volumes) != 1 || view.Volumes[0].Source != "mount" || view.Volumes[0].TotalBytes <= 0 {
		t.Fatalf("volume %+v", view.Volumes)
	}
	if len(view.Folders) != 1 || view.Folders[0].Bytes != 40 || view.Folders[0].Source != "mount" {
		t.Fatalf("folder %+v", view.Folders)
	}
}

func TestStorageWithoutAHelperStillTracksTheVolume(t *testing.T) {
	s, roots := storageRoots(t, "Anime", "Movies")
	if _, err := s.measureStorage(context.Background()); err != nil {
		t.Fatal(err)
	}
	view, _ := s.storageView()
	if view.Helper {
		t.Fatal("no helper is configured")
	}
	// Both roots sit on one filesystem here.
	if len(view.Volumes) != 1 || len(view.Volumes[0].Roots) != len(roots) {
		t.Fatalf("volumes %+v", view.Volumes)
	}
}

func TestStorageDoesNotCountANestedRootTwice(t *testing.T) {
	s, _ := storageRoots(t, "Library")
	base := s.engine.Deps().Fs.ReadRoots()[0]
	inner := filepath.Join(base, "Podcasts")
	os.MkdirAll(inner, 0700)
	deps := s.engine.Deps()
	deps.Fs, _ = fsop.NewResolver([]string{base, inner}, nil)
	s.engine = action.NewEngine(deps)
	roots := deps.Fs.ReadRoots()
	s.nas = &fakeMeter{prefix: filepath.Dir(base), volumes: []nashelper.Volume{{Root: "/nas", FSID: "v", TotalBytes: 1000, FreeBytes: 500, AvailBytes: 500}},
		usage: map[string]int64{roots[0]: 400, roots[1]: 150}}
	if _, err := s.measureStorage(context.Background()); err != nil {
		t.Fatal(err)
	}
	view, _ := s.storageView()
	var nested int
	for _, f := range view.Folders {
		if f.Inside != "" {
			nested++
			if f.Path != roots[1] || f.Inside != roots[0] {
				t.Fatalf("nested %+v", f)
			}
		}
	}
	if nested != 1 || view.Volumes[0].AccountedBytes != 400 || view.Volumes[0].OtherBytes != 100 {
		t.Fatalf("nested %d volume %+v", nested, view.Volumes[0])
	}
}

func TestStorageHidesARootThatWasRemovedFromTheConfiguration(t *testing.T) {
	s, roots := storageRoots(t, "Anime")
	st := s.engine.Deps().Store
	now := time.Now().UTC()
	st.AddStorageSamples([]store.StorageSample{
		{TakenAt: now, Scope: "folder", Path: roots[0], Volume: roots[0], UsedBytes: 5, Source: "helper"},
		{TakenAt: now, Scope: "folder", Path: "/gone/Old", Volume: "/gone/Old", UsedBytes: 9, Source: "helper"},
	})
	view, _ := s.storageView()
	if len(view.Folders) != 1 || view.Folders[0].Path != roots[0] {
		t.Fatalf("folders %+v", view.Folders)
	}
}

func TestStorageTrendsReportChangesOnlyWhenASampleOldEnoughExists(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	sample := func(daysAgo int, hour int, used int64) store.StorageSample {
		return store.StorageSample{TakenAt: now.AddDate(0, 0, -daysAgo).Add(time.Duration(hour-12) * time.Hour), UsedBytes: used}
	}
	series := []store.StorageSample{sample(31, 12, 900), sample(8, 12, 800), sample(3, 6, 700), sample(3, 18, 690), sample(0, 12, 650)}
	trend := trendOf(series, now)
	if trend.Change7d == nil || *trend.Change7d != -150 {
		t.Fatalf("7d %v (compared with the newest sample at least a week old)", trend.Change7d)
	}
	if trend.Change30d == nil || *trend.Change30d != -250 {
		t.Fatalf("30d %v", trend.Change30d)
	}
	if trend.ChangeAll == nil || *trend.ChangeAll != -250 {
		t.Fatalf("all %v", trend.ChangeAll)
	}
	// Two samples on the same day are one point: the later one.
	if len(trend.Points) != 4 || trend.Points[2].Bytes != 690 {
		t.Fatalf("points %+v", trend.Points)
	}
	young := trendOf(series[3:], now)
	if young.Change7d != nil || young.Change30d != nil || young.ChangeAll == nil {
		t.Fatalf("a history shorter than the window must not invent a change: %+v", young)
	}
	holes := trendOf([]store.StorageSample{sample(40, 12, 900), sample(10, 12, 800), sample(0, 12, 650)}, now)
	if holes.Change7d != nil || holes.Change30d != nil || holes.ChangeAll == nil || *holes.ChangeAll != -250 {
		t.Fatalf("a hole in the history must not be passed off as a window: %+v", holes)
	}
	if empty := trendOf(nil, now); len(empty.Points) != 0 || empty.ChangeAll != nil {
		t.Fatal(empty)
	}
}

func TestStorageEndpointIsAuthenticatedAndMeasuresInTheBackground(t *testing.T) {
	s, roots := storageRoots(t, "Anime")
	h := s.Handler()
	s.nas = &fakeMeter{prefix: filepath.Dir(roots[0]), volumes: []nashelper.Volume{{Root: "/nas", FSID: "v", TotalBytes: 100, FreeBytes: 60, AvailBytes: 60}}, usage: map[string]int64{roots[0]: 25}}
	if request(h, "GET", "/api/maintenance/storage", "", false).Code != 401 {
		t.Fatal("storage statistics are not authenticated")
	}
	if request(h, "POST", "/api/maintenance/storage", "", false).Code != 401 {
		t.Fatal("a measurement can be started without signing in")
	}
	w := request(h, "POST", "/api/maintenance/storage", "{}", true)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	deadline := time.Now().Add(3 * time.Second)
	var view storageView
	for time.Now().Before(deadline) {
		w = request(h, "GET", "/api/maintenance/storage", "", true)
		view = storageView{}
		if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
			t.Fatal(err, w.Body.String())
		}
		if !view.Measuring && len(view.Folders) > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if view.Measuring || len(view.Volumes) != 1 || view.Volumes[0].UsedBytes != 40 || view.Folders[0].Bytes != 25 {
		t.Fatalf("%s", w.Body.String())
	}
	// A second request right away does not start another walk.
	before := len(s.nas.(*fakeMeter).walked)
	request(h, "POST", "/api/maintenance/storage", "{}", true)
	time.Sleep(50 * time.Millisecond)
	if after := len(s.nas.(*fakeMeter).walked); after != before {
		t.Fatalf("a refresh within %v walked the library again", storageManualGap)
	}
}

func waitReady(t *testing.T, c *folderSizeCache, path string, resolver *fsop.Resolver, meter nasMeter) folderSize {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		size := c.get(path, resolver, nil, meter)
		if size.Status == "ready" && !size.Updating {
			return size
		}
		if time.Now().After(deadline) {
			t.Fatal("size never became ready", size)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestFileFolderSizesAreMeasuredByTheHelperWhenItAnswers(t *testing.T) {
	root := t.TempDir()
	resolver, _ := fsop.NewResolver([]string{root}, nil)
	folder := filepath.Join(resolver.ReadRoots()[0], "Season 1")
	os.Mkdir(folder, 0700)
	os.WriteFile(filepath.Join(folder, "episode.mkv"), make([]byte, 12), 0600)
	meter := &fakeMeter{prefix: resolver.ReadRoots()[0], usage: map[string]int64{folder: 999}}
	cache := &folderSizeCache{}
	size := waitReady(t, cache, folder, resolver, meter)
	if *size.Bytes != 999 || len(meter.walked) != 1 {
		t.Fatalf("the NAS was not asked: %+v walked %v", size, meter.walked)
	}
	cache.mu.Lock()
	entry := cache.entries[folder]
	cache.mu.Unlock()
	if entry.Source != "helper" || len(entry.Directories) != 0 {
		t.Fatalf("snapshot %+v", entry)
	}
	// A helper size has no directory stamps to watch, so it is repeated after
	// a short while instead of half an hour.
	if helperRemeasure(entry, 30*time.Minute) >= 30*time.Minute {
		t.Fatal("helper sizes would stay stale for half an hour")
	}
}

func TestFileFolderSizesFallBackToTheShareWhenTheHelperIsDown(t *testing.T) {
	root := t.TempDir()
	resolver, _ := fsop.NewResolver([]string{root}, nil)
	folder := filepath.Join(resolver.ReadRoots()[0], "Season 1")
	os.Mkdir(folder, 0700)
	os.WriteFile(filepath.Join(folder, "episode.mkv"), make([]byte, 12), 0600)
	meter := &fakeMeter{prefix: resolver.ReadRoots()[0], usageErr: fmt.Errorf("%w: down", nashelper.ErrUnavailable)}
	size := waitReady(t, &folderSizeCache{}, folder, resolver, meter)
	if *size.Bytes != 12 {
		t.Fatalf("the share was not walked: %+v", size)
	}
}

func TestStorageTrendsStayEmptyWhenSamplingHasStopped(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	old := func(daysAgo int, used int64) store.StorageSample {
		return store.StorageSample{TakenAt: now.AddDate(0, 0, -daysAgo), UsedBytes: used}
	}
	trend := trendOf([]store.StorageSample{old(40, 900), old(20, 800), old(8, 700)}, now)
	if trend.Change7d != nil || trend.Change30d != nil || trend.ChangeAll != nil || len(trend.Points) != 3 {
		t.Fatalf("old samples must not be reported as a recent change: %+v", trend)
	}
}

func TestStorageKeepsTheRoundThatRanOutOfTime(t *testing.T) {
	s, roots := storageRoots(t, "Anime", "Movies")
	s.nas = &fakeMeter{
		prefix:  filepath.Dir(roots[0]),
		volumes: []nashelper.Volume{{Root: "/nas", FSID: "vol1", TotalBytes: 1000, FreeBytes: 400, AvailBytes: 400}},
		usage:   map[string]int64{roots[0]: 300, roots[1]: 200},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	notes, err := s.measureStorage(ctx)
	if err != nil {
		t.Fatalf("the volumes were read and must be kept: %v", err)
	}
	if len(notes) == 0 || !strings.Contains(notes[len(notes)-1], "2 of 2") {
		t.Fatalf("the folders left out were not reported: %v", notes)
	}
	view, _ := s.storageView()
	if len(view.Volumes) != 1 || len(view.Folders) != 0 {
		t.Fatalf("view %+v", view)
	}
}

func TestStorageNothingMeasuredIsAnError(t *testing.T) {
	s, roots := storageRoots(t, "Anime")
	s.nas = &fakeMeter{prefix: filepath.Dir(roots[0]), volumesErr: fmt.Errorf("%w: down", nashelper.ErrUnavailable), usageErr: fmt.Errorf("%w: down", nashelper.ErrUnavailable)}
	if err := os.RemoveAll(roots[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := s.measureStorage(context.Background()); err == nil {
		t.Fatal("a round that measured nothing must say so")
	}
}

func TestStorageVolumeKeepsItsSeriesWhenRootsChange(t *testing.T) {
	s, roots := storageRoots(t, "Anime", "Movies")
	meter := &fakeMeter{
		prefix:  filepath.Dir(roots[0]),
		volumes: []nashelper.Volume{{Root: "/nas", FSID: "vol1", TotalBytes: 1000, FreeBytes: 400, AvailBytes: 400}},
		usage:   map[string]int64{roots[0]: 300, roots[1]: 200},
	}
	s.nas = meter
	if _, err := s.measureStorage(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The volume now reports through a different first root: still one volume.
	v2, err := fsop.NewResolver([]string{roots[1], roots[0]}, nil)
	if err != nil {
		t.Fatal(err)
	}
	deps := s.engine.Deps()
	deps.Fs = v2
	s.engine = action.NewEngine(deps)
	time.Sleep(1100 * time.Millisecond)
	if _, err := s.measureStorage(context.Background()); err != nil {
		t.Fatal(err)
	}
	view, _ := s.storageView()
	if len(view.Volumes) != 1 || view.Volumes[0].ID != "fsid:vol1" || len(view.Volumes[0].Roots) != 2 {
		t.Fatalf("volumes %+v", view.Volumes)
	}
}
