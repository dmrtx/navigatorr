package maintenanceui

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jakenesler/navigatorr/fsop"
	"github.com/jakenesler/navigatorr/internal/nashelper"
	"github.com/jakenesler/navigatorr/store"
)

const (
	// storageEvery is how often the NAS and its folders are measured. Space moves
	// slowly; the point is a trend, not a live gauge.
	storageEvery = 6 * time.Hour
	// storageManualGap keeps a refresh button from re-walking the library back to
	// back.
	storageManualGap = 2 * time.Minute
	// storageKeep is how long samples are kept.
	storageKeep = 800 * 24 * time.Hour
	// storageBudget bounds one measurement of every folder.
	storageBudget = 45 * time.Minute
	// storageFolderBudget bounds the helper's walk of one root, so a single slow
	// folder cannot use up the round.
	storageFolderBudget = 15 * time.Minute
	// storageWindowSlack is how much older than a window's start the sample it
	// is measured from may be.
	storageWindowSlack = 2 * 24 * time.Hour
	// storageDays is how many days of history the page gets, one point per day.
	storageDays = 365
)

// nasMeter is what the NAS helper offers for sizes. Nil means no helper is
// configured, and every size is read through the mount.
type nasMeter interface {
	Remote(local string) (string, bool)
	Volumes(ctx context.Context) (nashelper.VolumeResponse, error)
	Usage(ctx context.Context, local string, progress nashelper.UsageProgress, opts ...nashelper.UsageOption) (nashelper.UsageResult, error)
}

func (s *Server) nasMeter() nasMeter {
	if s.nas != nil {
		return s.nas
	}
	if c := s.engine.Deps().NASHelper; c != nil {
		return c
	}
	return nil
}

// storageState is the measurement in progress, if any, and how the last one went.
type storageState struct {
	mu  sync.Mutex
	ctx context.Context
	// running is every measurement started, so shutdown can wait for them.
	running   sync.WaitGroup
	measuring bool
	startedAt time.Time
	lastError string
	// notes are things the last measurement could not do, in plain words.
	notes []string
}

type volumeReading struct {
	// Key names the volume by what the helper or the mount calls it (the
	// filesystem id, or the device), which does not change when roots are added,
	// removed or fail to read.
	Key   string
	Roots []string
	Total int64
	Free  int64
	Avail int64
	// Source is "helper" or "mount".
	Source string
}

type folderReading struct {
	Path   string
	Volume string
	Bytes  int64
	Source string
}

// readVolumes groups the served roots by the volume they live on and reports
// each volume's space. The NAS helper answers from inside the NAS; without it
// the share's own numbers are used, which on a network mount are the volume's.
func readVolumes(ctx context.Context, roots []string, meter nasMeter) ([]*volumeReading, []string) {
	var notes []string
	var remote []nashelper.Volume
	if meter != nil {
		resp, err := meter.Volumes(ctx)
		if err != nil {
			notes = append(notes, "The NAS helper could not report volume space; the share was asked instead.")
		} else {
			remote = resp.Volumes
		}
	}
	byKey := map[string]*volumeReading{}
	var out []*volumeReading
	for _, root := range roots {
		var key string
		var total, free, avail int64
		source := ""
		if meter != nil {
			if path, ok := meter.Remote(root); ok {
				var best *nashelper.Volume
				for i := range remote {
					v := &remote[i]
					if withinOrEqual(v.Root, path) && (best == nil || len(v.Root) > len(best.Root)) {
						best = v
					}
				}
				if best != nil {
					key, total, free, avail, source = "fsid:"+best.FSID, best.TotalBytes, best.FreeBytes, best.AvailBytes, "helper"
				}
			}
		}
		if source == "" {
			space, err := fsop.SpaceOf(root)
			if err != nil {
				notes = append(notes, fmt.Sprintf("The space under %s could not be read.", root))
				continue
			}
			key, total, free, avail, source = fmt.Sprintf("dev:%d", space.Device), int64(space.Total), int64(space.Free), int64(space.Avail), "mount"
		}
		v := byKey[key]
		if v == nil {
			v = &volumeReading{Key: key, Total: total, Free: free, Avail: avail, Source: source}
			byKey[key] = v
			out = append(out, v)
		}
		v.Roots = append(v.Roots, root)
	}
	return out, notes
}

func withinOrEqual(root, path string) bool {
	root = strings.TrimSuffix(root, "/")
	return path == root || strings.HasPrefix(path, root+"/")
}

// readFolder sizes a served root. The NAS helper walks it from inside the NAS;
// without it, the size comes from the folder cache, and only a complete
// measurement counts.
func (s *Server) readFolder(ctx context.Context, root string, meter nasMeter) (bytes int64, source string, skipped int64, ok bool) {
	if meter != nil {
		if _, mapped := meter.Remote(root); mapped {
			helperCtx, cancel := context.WithTimeout(ctx, storageFolderBudget)
			// The walk also sizes the folders two levels down, which fills the folder
			// cache: the Files tab then finds every series and season measured.
			res, err := meter.Usage(helperCtx, root, nil, nashelper.UsageChildren(nashelper.MaxUsageChildDepth))
			cancel()
			if err == nil {
				s.folderSizes.adopt(s.engine.Deps().Store, root, res)
				return res.Bytes, "helper", res.Skipped, true
			}
			if !nashelper.ShouldFallBack(err) {
				return 0, "", 0, false
			}
		}
	}
	resolver := s.engine.Deps().Fs
	if resolver == nil {
		return 0, "", 0, false
	}
	// Walking a library over the share is slow and often incomplete; a root that
	// cannot be finished in this time is left out rather than recorded low.
	walk, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	size := measureFolderSize(walk, root, resolver, 300_000)
	if size.Status != "ready" || size.Bytes == nil {
		return 0, "", 0, false
	}
	return *size.Bytes, "mount", 0, true
}

// measureStorage measures the volumes and the served roots and records them.
func (s *Server) measureStorage(ctx context.Context) (notes []string, err error) {
	resolver := s.engine.Deps().Fs
	st := s.engine.Deps().Store
	if resolver == nil || st == nil {
		return nil, errors.New("storage statistics need the filesystem and the store")
	}
	roots := resolver.ReadRoots()
	meter := s.nasMeter()
	volumes, notes := readVolumes(ctx, roots, meter)
	volumeOf := map[string]string{}
	for _, v := range volumes {
		for _, r := range v.Roots {
			volumeOf[r] = v.Key
		}
	}
	taken := time.Now().UTC()
	var samples []store.StorageSample
	for _, v := range volumes {
		samples = append(samples, store.StorageSample{
			TakenAt: taken, Scope: "volume", Path: v.Key, Volume: v.Key,
			UsedBytes: max(0, v.Total-v.Free), TotalBytes: v.Total, FreeBytes: v.Avail, Source: v.Source,
		})
	}
	missed, unread := 0, 0
	for _, root := range roots {
		// Out of time: keep what was measured instead of throwing the round away,
		// or the next one would start from nothing.
		if ctx.Err() != nil {
			missed++
			continue
		}
		bytes, source, skipped, ok := s.readFolder(ctx, root, meter)
		if !ok {
			missed++
			continue
		}
		if skipped > 0 {
			unread++
		}
		samples = append(samples, store.StorageSample{
			TakenAt: taken, Scope: "folder", Path: root, Volume: volumeOf[root], UsedBytes: bytes, Source: source,
		})
	}
	if missed > 0 {
		notes = append(notes, fmt.Sprintf("%d of %d folders could not be sized completely. Folder sizes need the NAS helper for libraries this large.", missed, len(roots)))
	}
	if unread > 0 {
		notes = append(notes, fmt.Sprintf("%d folders had entries the NAS helper could not read, so their sizes may be low.", unread))
	}
	if len(samples) == 0 {
		return notes, errors.New("nothing could be measured")
	}
	if err := st.AddStorageSamples(samples); err != nil {
		return notes, err
	}
	_ = st.PruneStorageSamples(taken.Add(-storageKeep))
	return notes, nil
}

// startStorageMeasurement begins a measurement in the background unless one is
// running or the last one started moments ago. It reports whether one started.
func (s *Server) startStorageMeasurement(force bool) bool {
	s.storage.mu.Lock()
	defer s.storage.mu.Unlock()
	if s.storage.measuring {
		return false
	}
	if force && !s.storage.startedAt.IsZero() && time.Since(s.storage.startedAt) < storageManualGap {
		return false
	}
	parent := s.storage.ctx
	if parent == nil {
		parent = context.Background()
	}
	s.storage.measuring = true
	s.storage.startedAt = time.Now()
	s.storage.running.Add(1)
	go func() {
		defer s.storage.running.Done()
		ctx, cancel := context.WithTimeout(parent, storageBudget)
		defer cancel()
		notes, err := s.measureStorage(ctx)
		s.storage.mu.Lock()
		s.storage.measuring = false
		s.storage.notes = notes
		s.storage.lastError = ""
		if err != nil {
			s.storage.lastError = err.Error()
			log.Printf("storage statistics: %v", err)
		}
		s.storage.mu.Unlock()
	}()
	return true
}

// SetStorageContext ties background measurements to ctx, so they stop with the
// server. It is called before the UI starts serving.
func (s *Server) SetStorageContext(ctx context.Context) {
	s.storage.mu.Lock()
	s.storage.ctx = ctx
	s.storage.mu.Unlock()
}

// WaitStorage returns once no measurement is running; call it after the context
// given to SetStorageContext ends and before the store closes.
func (s *Server) WaitStorage() { s.storage.running.Wait() }

// RunStorageSampler measures storage shortly after start when the last sample is
// old, then whenever it has aged past storageEvery, until ctx ends. A round that
// saved nothing counts as an attempt, so a failing one is retried every
// storageEvery, not every check.
func (s *Server) RunStorageSampler(ctx context.Context) {
	wait := time.Minute
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = 30 * time.Minute
		st := s.engine.Deps().Store
		if st == nil {
			return
		}
		last, err := st.LastStorageSampleTime()
		if err != nil {
			log.Printf("storage statistics: %v", err)
			continue
		}
		s.storage.mu.Lock()
		if attempt := s.storage.startedAt; attempt.After(last) {
			last = attempt
		}
		s.storage.mu.Unlock()
		if time.Since(last) >= storageEvery {
			s.startStorageMeasurement(false)
		}
	}
}

type storagePoint struct {
	T     string `json:"t"`
	Bytes int64  `json:"bytes"`
}

// storageTrend is a series with how much it moved over the common windows. A
// change is only given when a sample old enough exists; it is never guessed.
type storageTrend struct {
	Points    []storagePoint `json:"points"`
	Change7d  *int64         `json:"change_7d,omitempty"`
	Change30d *int64         `json:"change_30d,omitempty"`
	// ChangeAll is the change since the first sample.
	ChangeAll *int64 `json:"change_all,omitempty"`
}

type storageVolumeView struct {
	ID         string   `json:"id"`
	Roots      []string `json:"roots"`
	TotalBytes int64    `json:"total_bytes"`
	UsedBytes  int64    `json:"used_bytes"`
	FreeBytes  int64    `json:"free_bytes"`
	// AccountedBytes is what the served folders hold; OtherBytes is the rest of
	// the used space, which the console cannot see into.
	AccountedBytes int64        `json:"accounted_bytes"`
	OtherBytes     int64        `json:"other_bytes"`
	Source         string       `json:"source"`
	MeasuredAt     string       `json:"measured_at"`
	Trend          storageTrend `json:"trend"`
}

type storageFolderView struct {
	Path   string `json:"path"`
	Name   string `json:"name"`
	Volume string `json:"volume"`
	// Inside names the served root this one sits within, whose size already
	// counts it.
	Inside     string       `json:"inside,omitempty"`
	Bytes      int64        `json:"bytes"`
	Source     string       `json:"source"`
	MeasuredAt string       `json:"measured_at"`
	Trend      storageTrend `json:"trend"`
}

type storageView struct {
	Helper     bool                `json:"helper"`
	Measuring  bool                `json:"measuring"`
	Error      string              `json:"error,omitempty"`
	Notes      []string            `json:"notes,omitempty"`
	MeasuredAt string              `json:"measured_at,omitempty"`
	Since      string              `json:"since,omitempty"`
	Volumes    []storageVolumeView `json:"volumes"`
	Folders    []storageFolderView `json:"folders"`
}

func trendOf(series []store.StorageSample, now time.Time) storageTrend {
	trend := storageTrend{Points: []storagePoint{}}
	if len(series) == 0 {
		return trend
	}
	// One point per day: the last sample of that day.
	lastDay := ""
	for _, sample := range series {
		day := sample.TakenAt.UTC().Format("2006-01-02")
		point := storagePoint{T: sample.TakenAt.UTC().Format(time.RFC3339), Bytes: sample.UsedBytes}
		if day == lastDay {
			trend.Points[len(trend.Points)-1] = point
		} else {
			trend.Points = append(trend.Points, point)
			lastDay = day
		}
	}
	if len(trend.Points) > storageDays {
		trend.Points = trend.Points[len(trend.Points)-storageDays:]
	}
	latest := series[len(series)-1]
	// Samples that stopped long ago say nothing about the last week or month.
	if now.Sub(latest.TakenAt) > storageWindowSlack {
		return trend
	}
	change := func(window time.Duration) *int64 {
		cutoff := now.Add(-window)
		var base *store.StorageSample
		for i := range series {
			if series[i].TakenAt.After(cutoff) {
				break
			}
			base = &series[i]
		}
		// The base is the newest sample at least a window old. If the samples
		// stop for longer than a few days around the window's start, the figure
		// would describe another span than its label, so it is left out.
		if base == nil || cutoff.Sub(base.TakenAt) > storageWindowSlack {
			return nil
		}
		d := latest.UsedBytes - base.UsedBytes
		return &d
	}
	trend.Change7d = change(7 * 24 * time.Hour)
	trend.Change30d = change(30 * 24 * time.Hour)
	if len(series) > 1 {
		d := latest.UsedBytes - series[0].UsedBytes
		trend.ChangeAll = &d
	}
	return trend
}

func (s *Server) storageView() (storageView, error) {
	view := storageView{Volumes: []storageVolumeView{}, Folders: []storageFolderView{}}
	st := s.engine.Deps().Store
	if st == nil {
		return view, errors.New("the store is unavailable")
	}
	now := time.Now().UTC()
	samples, err := st.StorageSamples(now.Add(-storageKeep))
	if err != nil {
		return view, err
	}
	type seriesKey struct{ scope, path string }
	series := map[seriesKey][]store.StorageSample{}
	var keys []seriesKey
	for _, sample := range samples {
		k := seriesKey{sample.Scope, sample.Path}
		if _, seen := series[k]; !seen {
			keys = append(keys, k)
		}
		series[k] = append(series[k], sample)
	}
	if len(samples) > 0 {
		view.Since = samples[0].TakenAt.UTC().Format(time.RFC3339)
		view.MeasuredAt = samples[len(samples)-1].TakenAt.UTC().Format(time.RFC3339)
	}
	// Only the configured roots are shown: a root that was removed from the
	// configuration would otherwise stay on the page for years.
	current := map[string]bool{}
	if resolver := s.engine.Deps().Fs; resolver != nil {
		for _, r := range resolver.ReadRoots() {
			current[r] = true
		}
	}
	// A volume is shown when the newest round measured it. Its key is the
	// filesystem or device, so a root added, removed or unreadable one time
	// cannot leave a second, stale card behind.
	var newestVolume time.Time
	for _, k := range keys {
		if k.scope == "volume" {
			if last := series[k][len(series[k])-1].TakenAt; last.After(newestVolume) {
				newestVolume = last
			}
		}
	}
	roots := map[string][]string{}
	for _, k := range keys {
		if k.scope == "volume" && !series[k][len(series[k])-1].TakenAt.Equal(newestVolume) {
			continue
		}
		if k.scope == "folder" && !current[k.path] {
			continue
		}
		list := series[k]
		latest := list[len(list)-1]
		trend := trendOf(list, now)
		switch k.scope {
		case "volume":
			view.Volumes = append(view.Volumes, storageVolumeView{
				ID: k.path, Roots: []string{}, TotalBytes: latest.TotalBytes, UsedBytes: latest.UsedBytes, FreeBytes: latest.FreeBytes,
				Source: latest.Source, MeasuredAt: latest.TakenAt.UTC().Format(time.RFC3339), Trend: trend,
			})
		case "folder":
			roots[latest.Volume] = append(roots[latest.Volume], k.path)
			view.Folders = append(view.Folders, storageFolderView{
				Path: k.path, Name: filepath.Base(k.path), Volume: latest.Volume, Bytes: latest.UsedBytes,
				Source: latest.Source, MeasuredAt: latest.TakenAt.UTC().Format(time.RFC3339), Trend: trend,
			})
		}
	}
	for i := range view.Folders {
		f := &view.Folders[i]
		for _, other := range view.Folders {
			if other.Path != f.Path && withinOrEqual(other.Path, f.Path) {
				f.Inside = other.Path
				break
			}
		}
	}
	sort.SliceStable(view.Folders, func(i, j int) bool { return view.Folders[i].Bytes > view.Folders[j].Bytes })
	for i := range view.Volumes {
		v := &view.Volumes[i]
		v.Roots = append(v.Roots, roots[v.ID]...)
		for _, f := range view.Folders {
			if f.Volume == v.ID && f.Inside == "" {
				v.AccountedBytes += f.Bytes
			}
		}
		v.OtherBytes = max(0, v.UsedBytes-v.AccountedBytes)
		sort.Strings(v.Roots)
	}
	sort.SliceStable(view.Volumes, func(i, j int) bool { return view.Volumes[i].TotalBytes > view.Volumes[j].TotalBytes })
	view.Helper = s.nasMeter() != nil
	s.storage.mu.Lock()
	view.Measuring = s.storage.measuring
	view.Error = s.storage.lastError
	view.Notes = append([]string(nil), s.storage.notes...)
	s.storage.mu.Unlock()
	return view, nil
}

// storageStats serves the storage statistics. A POST asks for a fresh measurement,
// which runs in the background; the page polls the GET until it ends.
func (s *Server) storageStats(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		s.startStorageMeasurement(true)
	}
	view, err := s.storageView()
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, view)
}
