package maintenanceui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/fsop"
	"github.com/jakenesler/navigatorr/internal/nashelper"
)

// queueMeter is a NAS helper whose walks wait on a gate and record the order
// they started in.
type queueMeter struct {
	prefix string
	mu     sync.Mutex
	order  []string
	gates  map[string]chan struct{}
	stamps map[string]int64
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
	return nashelper.UsageResult{Bytes: 7, Files: 1, Dirs: 1, Stamps: m.stamps}, nil
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
	names := []string{"a", "b", "c", "d", "e"}
	meter := &queueMeter{prefix: base, gates: map[string]chan struct{}{}}
	paths := map[string]string{}
	for _, name := range names {
		paths[name] = filepath.Join(base, name)
		if err := os.Mkdir(paths[name], 0700); err != nil {
			t.Fatal(err)
		}
		meter.gates[paths[name]] = make(chan struct{})
	}
	cache := &folderSizeCache{}
	ask := func(name string) folderSize { return cache.get(paths[name], resolver, nil, meter) }

	ask("a")
	ask("b")
	eventually(t, "both slots to start", func() bool { return len(meter.started()) == 2 })
	if size := ask("a"); size.Queued || !size.Updating || size.Status != "calculating" {
		t.Fatalf("a running measurement is not queued: %+v", size)
	}
	for _, name := range []string{"c", "d", "e"} {
		if size := ask(name); !size.Queued || size.Status != "calculating" {
			t.Fatalf("%s should wait for a slot: %+v", name, size)
		}
		time.Sleep(2 * time.Millisecond)
	}
	// The user comes back to c: it is the folder on screen now.
	ask("c")

	close(meter.gates[paths["a"]])
	eventually(t, "a slot to pass to the newest ask", func() bool { return len(meter.started()) == 3 })
	if got := meter.started()[2]; got != paths["c"] {
		t.Fatalf("the slot went to %s, not the folder asked about last", got)
	}
	if size := ask("c"); size.Queued {
		t.Fatalf("c has started: %+v", size)
	}
	if size := ask("d"); !size.Queued {
		t.Fatalf("d still waits: %+v", size)
	}
	for _, name := range names[1:] {
		close(meter.gates[paths[name]])
	}
	for _, name := range names {
		waitReady(t, cache, paths[name], resolver, meter)
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
