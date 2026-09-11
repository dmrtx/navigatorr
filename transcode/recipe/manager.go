package recipe

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type cacheMetadata struct {
	Source        string   `json:"source"`
	Channel       string   `json:"channel,omitempty"`
	Revision      string   `json:"revision,omitempty"`
	ActiveVersion string   `json:"active_version"`
	ActiveDigest  string   `json:"active_digest"`
	LastKnownGood string   `json:"last_known_good"`
	History       []string `json:"history"`
}

type Manager struct {
	active      atomic.Pointer[Snapshot]
	mu          sync.Mutex
	provider    Provider
	cacheDir    string
	source      string
	channel     string
	revision    string
	lastChecked time.Time
	lastError   string
}

func NewManager(provider Provider, cacheDir, channel, revision string) (*Manager, error) {
	if provider == nil {
		provider = BuiltinProvider{}
	}
	embedded, err := Parse(EmbeddedBytes())
	if err != nil {
		return nil, fmt.Errorf("embedded recipes invalid: %w", err)
	}
	m := &Manager{provider: provider, cacheDir: cacheDir, source: provider.Name(), channel: channel, revision: revision}
	m.active.Store(embedded)
	if cacheDir != "" {
		_ = m.restoreActive()
	}
	return m, nil
}
func (m *Manager) Snapshot() *Snapshot {
	s := m.active.Load()
	if s == nil {
		return nil
	}
	// Re-parse the immutable raw bytes so callers cannot mutate maps owned by the
	// active snapshot while a hot reload is happening.
	cp, err := Parse(s.Raw)
	if err != nil {
		return nil
	}
	return cp
}
func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.active.Load()
	st := Status{Source: m.source, Channel: m.channel, Revision: m.revision, LastCheckedAt: m.lastChecked, LastUpdateError: m.lastError}
	if s != nil {
		st.ActiveVersion = s.Identity.Version
		st.ActiveDigest = s.Identity.Digest
		st.LastKnownGood = s.Identity.Version
	}
	return st
}
func (m *Manager) Reload(ctx context.Context) (Status, error) { return m.Update(ctx) }
func (m *Manager) Update(ctx context.Context) (Status, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastChecked = time.Now().UTC()
	cand, err := m.provider.Fetch(ctx)
	if err != nil {
		m.lastError = err.Error()
		return m.statusLocked(), err
	}
	snap, err := Parse(cand.Data)
	if err != nil {
		m.lastError = err.Error()
		return m.statusLocked(), err
	}
	if cand.Manifest != nil && cand.Manifest.BundleVersion != snap.Identity.Version {
		err = fmt.Errorf("manifest version does not match parsed recipe")
		m.lastError = err.Error()
		return m.statusLocked(), err
	}
	previous := m.active.Load()
	if err := m.cacheSnapshot(snap); err != nil {
		m.lastError = err.Error()
		return m.statusLocked(), err
	}
	// Commit metadata before the single atomic pointer swap. If metadata cannot
	// be persisted, no job can observe the candidate bundle as active.
	if err := m.writeMetadata(snap, previous); err != nil {
		m.lastError = err.Error()
		return m.statusLocked(), err
	}
	m.active.Store(snap)
	m.pruneCache(3)
	m.lastError = ""
	return m.statusLocked(), nil
}
func (m *Manager) Rollback() (Status, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cacheDir == "" {
		return m.statusLocked(), fmt.Errorf("recipe cache is disabled")
	}
	md, err := m.readMetadata()
	if err != nil {
		return m.statusLocked(), err
	}
	cur := m.active.Load()
	var snap *Snapshot
	for i := len(md.History) - 1; i >= 0; i-- {
		target := md.History[i]
		if cur != nil && target == cur.Identity.Version {
			continue
		}
		b, readErr := os.ReadFile(filepath.Join(m.cacheDir, target, "bundle.yaml"))
		if readErr != nil {
			continue
		}
		candidate, parseErr := Parse(b)
		if parseErr != nil {
			continue
		}
		snap = candidate
		break
	}
	if snap == nil {
		return m.statusLocked(), fmt.Errorf("no previous valid cached recipe bundle available")
	}
	previous := cur
	if err := m.writeMetadata(snap, previous); err != nil {
		return m.statusLocked(), err
	}
	m.active.Store(snap)
	m.lastError = ""
	return m.statusLocked(), nil
}
func (m *Manager) statusLocked() Status {
	s := m.active.Load()
	st := Status{Source: m.source, Channel: m.channel, Revision: m.revision, LastCheckedAt: m.lastChecked, LastUpdateError: m.lastError}
	if s != nil {
		st.ActiveVersion = s.Identity.Version
		st.ActiveDigest = s.Identity.Digest
		st.LastKnownGood = s.Identity.Version
	}
	return st
}
func (m *Manager) cacheSnapshot(s *Snapshot) error {
	if m.cacheDir == "" {
		return nil
	}
	dir := filepath.Join(m.cacheDir, s.Identity.Version)
	bundlePath := filepath.Join(dir, "bundle.yaml")
	if existing, err := os.ReadFile(bundlePath); err == nil {
		old, parseErr := Parse(existing)
		if parseErr != nil {
			return fmt.Errorf("cached recipe version %s is corrupt: %w", s.Identity.Version, parseErr)
		}
		if old.Identity.Digest != s.Identity.Digest {
			return fmt.Errorf("recipe version %s already exists with a different digest (immutable version collision)", s.Identity.Version)
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	return writeAtomic(bundlePath, s.Raw, 0644)
}
func (m *Manager) metadataPath() string { return filepath.Join(m.cacheDir, "active.json") }
func (m *Manager) readMetadata() (cacheMetadata, error) {
	var md cacheMetadata
	b, err := os.ReadFile(m.metadataPath())
	if err != nil {
		return md, err
	}
	err = json.Unmarshal(b, &md)
	return md, err
}
func (m *Manager) writeMetadata(active, previous *Snapshot) error {
	if m.cacheDir == "" {
		return nil
	}
	_ = os.MkdirAll(m.cacheDir, 0755)
	md := cacheMetadata{Source: m.source, Channel: m.channel, Revision: m.revision, ActiveVersion: active.Identity.Version, ActiveDigest: active.Identity.Digest, LastKnownGood: active.Identity.Version}
	if old, err := m.readMetadata(); err == nil {
		md.History = append(md.History, old.History...)
	}
	if previous != nil {
		md.History = append(md.History, previous.Identity.Version)
	}
	md.History = append(md.History, active.Identity.Version)
	md.History = dedupe(md.History)
	b, _ := json.MarshalIndent(md, "", "  ")
	return writeAtomic(m.metadataPath(), b, 0644)
}
func (m *Manager) restoreActive() error {
	md, err := m.readMetadata()
	if err != nil {
		return err
	}
	// A cache is only reusable by the same configured recipe source identity.
	if md.Source != m.source || md.Channel != m.channel || md.Revision != m.revision {
		return fmt.Errorf("cached recipe source does not match current configuration")
	}
	b, err := os.ReadFile(filepath.Join(m.cacheDir, md.ActiveVersion, "bundle.yaml"))
	if err != nil {
		return err
	}
	snap, err := Parse(b)
	if err != nil {
		return err
	}
	if snap.Identity.Digest != md.ActiveDigest {
		return fmt.Errorf("cached recipe digest mismatch")
	}
	m.active.Store(snap)
	return nil
}
func (m *Manager) pruneCache(keep int) {
	entries, err := os.ReadDir(m.cacheDir)
	if err != nil || keep < 1 {
		return
	}
	var dirs []os.DirEntry
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e)
		}
	}
	sort.Slice(dirs, func(i, j int) bool {
		ai, _ := dirs[i].Info()
		aj, _ := dirs[j].Info()
		return ai.ModTime().Before(aj.ModTime())
	})
	active := ""
	if s := m.active.Load(); s != nil {
		active = s.Identity.Version
	}
	remaining := len(dirs)
	for _, e := range dirs {
		if remaining <= keep {
			break
		}
		if e.Name() == active {
			continue
		}
		if os.RemoveAll(filepath.Join(m.cacheDir, e.Name())) == nil {
			remaining--
		}
	}
}
func writeAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".recipe-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Chmod(tmp, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// StartAutoRefresh periodically checks the configured provider. Failed refreshes
// keep the current immutable snapshot active and are reflected in Status().
func (m *Manager) StartAutoRefresh(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_, _ = m.Update(ctx)
			}
		}
	}()
}
