package maintenanceui

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/fsop"
	"github.com/jakenesler/navigatorr/internal/nashelper"
	"github.com/jakenesler/navigatorr/store"
)

// queueMeter is a NAS helper whose walks wait on a gate and record the order
// they started in.
type queueMeter struct {
	prefix   string
	mu       sync.Mutex
	order    []string
	gates    map[string]chan struct{}
	stamps   map[string]int64
	children []nashelper.UsageChild
}

func (m *queueMeter) Remote(local string) (string, bool) {
	if !strings.HasPrefix(local, m.prefix) {
		return "", false
	}
	return "/nas" + strings.TrimPrefix(local, m.prefix), true
}
func (m *queueMeter) Volumes(context.Context) (nashelper.VolumeResponse, error) {
	return nashelper.VolumeResponse{}, nashelper.ErrUnavailable
}
func (m *queueMeter) Usage(_ context.Context, local string, _ nashelper.UsageProgress, _ ...nashelper.UsageOption) (nashelper.UsageResult, error) {
	m.mu.Lock()
	m.order = append(m.order, local)
	gate := m.gates[local]
	m.mu.Unlock()
	if gate != nil {
		<-gate
	}
	return nashelper.UsageResult{Bytes: 7, Files: 1, Dirs: 1, Stamps: m.stamps, Children: m.children}, nil
}
func (m *queueMeter) started() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.order...)
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestFolderSizesWaitInTurnAndTheNewestAskRunsFirst(t *testing.T) {
	root := t.TempDir()
	resolver, _ := fsop.NewResolver([]string{root}, nil)
	base := resolver.ReadRoots()[0]
	meter := &queueMeter{prefix: base, gates: map[string]chan struct{}{}}
	var names []string
	paths := map[string]string{}
	for i := 0; i < folderSizeSlots+3; i++ {
		name := "f" + strconv.Itoa(i)
		names = append(names, name)
		paths[name] = filepath.Join(base, name)
		if err := os.Mkdir(paths[name], 0700); err != nil {
			t.Fatal(err)
		}
		meter.gates[paths[name]] = make(chan struct{})
	}
	cache := &folderSizeCache{}
	ask := func(name string) folderSize { return cache.get(paths[name], resolver, nil, meter) }

	for _, name := range names[:folderSizeSlots] {
		ask(name)
	}
	eventually(t, "every slot to start", func() bool { return len(meter.started()) == folderSizeSlots })
	if size := ask(names[0]); size.Queued || !size.Updating || size.Status != "calculating" {
		t.Fatalf("a running measurement is not queued: %+v", size)
	}
	waiting := names[folderSizeSlots:]
	for _, name := range waiting {
		if size := ask(name); !size.Queued || size.Status != "calculating" {
			t.Fatalf("%s should wait for a slot: %+v", name, size)
		}
		time.Sleep(2 * time.Millisecond)
	}
	// The user comes back to the middle one: it is the folder on screen now. First
	// come, first served would pick the first, and no refresh on a repeated ask
	// would pick the last.
	ask(waiting[1])

	close(meter.gates[paths[names[0]]])
	eventually(t, "a slot to pass to the newest ask", func() bool { return len(meter.started()) == folderSizeSlots+1 })
	if got := meter.started()[folderSizeSlots]; got != paths[waiting[1]] {
		t.Fatalf("the slot went to %s, not the folder asked about last", got)
	}
	if size := ask(waiting[0]); !size.Queued {
		t.Fatalf("%s still waits: %+v", waiting[0], size)
	}
	if size := ask(waiting[1]); size.Queued {
		t.Fatalf("%s has started: %+v", waiting[1], size)
	}
	for _, name := range names[1:] {
		close(meter.gates[paths[name]])
	}
	for _, name := range names {
		waitReady(t, cache, paths[name], resolver, meter)
	}
}

// A folder with nothing to show goes before one that only needs checking again,
// even when the check was asked for later.
func TestFolderNeverMeasuredGoesBeforeOneThatOnlyNeedsChecking(t *testing.T) {
	root := t.TempDir()
	resolver, _ := fsop.NewResolver([]string{root}, nil)
	base := resolver.ReadRoots()[0]
	meter := &queueMeter{prefix: base, gates: map[string]chan struct{}{}}
	mk := func(name string) string {
		p := filepath.Join(base, name)
		if err := os.Mkdir(p, 0700); err != nil {
			t.Fatal(err)
		}
		return p
	}
	known, unknown := mk("known"), mk("unknown")
	cache := &folderSizeCache{}
	waitReady(t, cache, known, resolver, meter)
	cache.mu.Lock()
	entry := cache.entries[known]
	entry.CheckedAt = time.Now().Add(-time.Hour)
	entry.Size.MeasuredAt = time.Now().Add(-time.Hour)
	cache.entries[known] = entry
	cache.mu.Unlock()
	meter.mu.Lock()
	meter.order = nil
	meter.mu.Unlock()

	var blockers []string
	for i := 0; i < folderSizeSlots; i++ {
		p := mk("blocker" + strconv.Itoa(i))
		meter.mu.Lock()
		meter.gates[p] = make(chan struct{})
		meter.mu.Unlock()
		blockers = append(blockers, p)
		cache.get(p, resolver, nil, meter)
	}
	eventually(t, "the blockers to take every slot", func() bool { return len(meter.started()) == folderSizeSlots })
	cache.get(unknown, resolver, nil, meter)
	time.Sleep(2 * time.Millisecond)
	cache.get(known, resolver, nil, meter) // asked last, but only a re-check

	meter.mu.Lock()
	close(meter.gates[blockers[0]])
	meter.mu.Unlock()
	// Neither waiting folder is gated, so both finish at once: the order they
	// started in is what counts.
	eventually(t, "a slot to pass on", func() bool { return len(meter.started()) >= folderSizeSlots+2 })
	if got := meter.started()[folderSizeSlots]; got != unknown {
		t.Fatalf("the slot went to %s, not the folder with no size yet", got)
	}
	for _, p := range blockers[1:] {
		meter.mu.Lock()
		close(meter.gates[p])
		meter.mu.Unlock()
	}
}

// One walk of a folder sizes the folders inside it too.
func TestAWalkSizesTheFoldersInsideSoOpeningThemNeedsNoWalk(t *testing.T) {
	root := t.TempDir()
	resolver, _ := fsop.NewResolver([]string{root}, nil)
	base := resolver.ReadRoots()[0]
	series := filepath.Join(base, "Cheers")
	season := filepath.Join(series, "Season 1")
	if err := os.MkdirAll(season, 0700); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(season)
	meter := &queueMeter{prefix: base, children: []nashelper.UsageChild{
		{Path: "Season 1", Bytes: 5, Files: 2, Stamps: map[string]int64{".": info.ModTime().Unix()}},
		{Path: "../escape", Bytes: 9},
	}}
	st, err := store.Open(filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cache := &folderSizeCache{}
	eventually(t, "the series to be measured", func() bool {
		size := cache.get(series, resolver, st, meter)
		return size.Status == "ready" && !size.Updating
	})
	if len(meter.started()) != 1 {
		t.Fatalf("the series walk: %v", meter.started())
	}
	size := cache.get(season, resolver, st, meter)
	if size.Status != "ready" || size.Bytes == nil || *size.Bytes != 5 {
		t.Fatalf("the season was not taken from the series walk: %+v", size)
	}
	cache.mu.Lock()
	entry := cache.entries[season]
	cache.mu.Unlock()
	if len(meter.started()) != 1 || entry.Source != "helper" || len(entry.Directories) != 1 {
		t.Fatalf("asking for the season walked again or lost its stamps: %v %+v", meter.started(), entry)
	}
	if raw, _ := st.FolderSizeSnapshot(filepath.Join(base, "escape")); raw != "" {
		t.Fatalf("a path outside the folder was adopted: %s", raw)
	}
}

func TestHelperSizesWithStampsAreWalkedAgainOnlyWhenADirectoryChanged(t *testing.T) {
	root := t.TempDir()
	resolver, _ := fsop.NewResolver([]string{root}, nil)
	base := resolver.ReadRoots()[0]
	folder := filepath.Join(base, "Season 1")
	if err := os.Mkdir(folder, 0700); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(folder)
	meter := &queueMeter{prefix: base, stamps: map[string]int64{".": info.ModTime().Unix()}}
	cache := &folderSizeCache{}
	waitReady(t, cache, folder, resolver, meter)
	if len(meter.started()) != 1 {
		t.Fatalf("first measurement: %v", meter.started())
	}
	age := func(by time.Duration) {
		cache.mu.Lock()
		entry := cache.entries[folder]
		entry.CheckedAt = time.Now().Add(-by)
		cache.entries[folder] = entry
		cache.mu.Unlock()
	}
	// Past the 10 minutes an unstamped helper size waits, but nothing changed.
	cache.mu.Lock()
	entry := cache.entries[folder]
	entry.Size.MeasuredAt = time.Now().Add(-20 * time.Minute)
	cache.entries[folder] = entry
	cache.mu.Unlock()
	age(time.Minute)
	waitReady(t, cache, folder, resolver, meter)
	if len(meter.started()) != 1 {
		t.Fatalf("an unchanged folder was walked again: %v", meter.started())
	}
	// An episode arrives: the directory's modification time moves.
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(folder, later, later); err != nil {
		t.Fatal(err)
	}
	age(time.Minute)
	eventually(t, "the changed folder to be walked again", func() bool {
		cache.get(folder, resolver, nil, meter)
		return len(meter.started()) == 2
	})
}
