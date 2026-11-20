package openapi

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/internal"
)

// Store manages OpenAPI specs for all services.
type Store struct {
	cfg     *config.Config
	cache   *Cache
	indices map[string]*Index
	mu      sync.RWMutex

	// Lazy loading: a configured service with no index is fetched again on
	// demand, at most once per retryInterval and per service.
	attempts      map[string]time.Time // last failed attempt per service
	inflight      map[string]chan struct{}
	retryInterval time.Duration
	fetchTimeout  time.Duration
	now           func() time.Time
}

const (
	defaultRetryInterval = 30 * time.Second
	defaultFetchTimeout  = 10 * time.Second
)

// NewStore creates a new spec store.
func NewStore(cfg *config.Config) *Store {
	home, _ := os.UserHomeDir()
	cacheDir := filepath.Join(home, ".cache", "navigatorr")

	return &Store{
		cfg:     cfg,
		cache:   NewCache(cacheDir),
		indices: make(map[string]*Index),

		attempts:      make(map[string]time.Time),
		inflight:      make(map[string]chan struct{}),
		retryInterval: defaultRetryInterval,
		fetchTimeout:  defaultFetchTimeout,
		now:           time.Now,
	}
}

// LoadAll fetches and parses specs for all configured services. A failure is
// recorded like any other failed attempt, so the lazy retry window starts then.
func (s *Store) LoadAll(ctx context.Context) {
	for name, svc := range s.cfg.Services {
		if svc.OpenAPIURL == "" {
			continue
		}
		s.ensure(ctx, name, true)
	}
}

// EnsureLoaded makes a lazy load attempt for a configured service that has no
// index yet and waits for it (or for a load already in flight), bounded by
// ctx. Failed attempts are rate-limited per service, each fetch is bounded by
// a timeout, and the lock is never held while fetching. It is a no-op for a
// loaded or unknown service.
func (s *Store) EnsureLoaded(ctx context.Context, name string) {
	s.ensure(ctx, name, false)
}

// EnsureAll runs EnsureLoaded for every configured service, concurrently.
func (s *Store) EnsureAll(ctx context.Context) {
	s.ensureEach(ctx, false)
}

func (s *Store) ensureEach(ctx context.Context, force bool) {
	var wg sync.WaitGroup
	for name, svc := range s.cfg.Services {
		if svc.OpenAPIURL == "" {
			continue
		}
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			s.ensure(ctx, name, force)
		}(name)
	}
	wg.Wait()
}

// ensure loads the service when it has no index and reports whether it has
// one afterwards. At most one load per service runs at a time: a caller that
// finds one in flight waits for it, even with force. The load runs on a
// context detached from the caller (with the fetch timeout), so a caller that
// gives up only stops waiting; the load still finishes. Only a failed load
// starts the rate-limit window; force skips the window (used by the startup
// retry loop, which has its own backoff).
func (s *Store) ensure(ctx context.Context, name string, force bool) bool {
	svc, ok := s.cfg.Services[name]
	if !ok || svc.OpenAPIURL == "" {
		return false
	}

	s.mu.Lock()
	if s.indices[name] != nil {
		s.mu.Unlock()
		return true
	}
	done, running := s.inflight[name]
	if !running {
		if last, failed := s.attempts[name]; failed && !force && s.now().Sub(last) < s.retryInterval {
			s.mu.Unlock()
			return false
		}
		done = make(chan struct{})
		s.inflight[name] = done
		timeout := s.fetchTimeout
		go func() {
			lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
			defer cancel()
			err := s.load(lctx, name, svc.OpenAPIURL)
			s.mu.Lock()
			if err != nil {
				s.attempts[name] = s.now()
			} else {
				delete(s.attempts, name)
			}
			delete(s.inflight, name)
			s.mu.Unlock()
			close(done)
			if err != nil {
				internal.Errorf("loading spec for %s: %v", name, err)
			} else if idx := s.GetIndex(name); idx != nil {
				internal.Logf("loaded %s: %d endpoints", name, idx.Count())
			}
		}()
	}
	s.mu.Unlock()

	select {
	case <-done:
	case <-ctx.Done():
	}
	return s.GetIndex(name) != nil
}

// RetryMissing keeps retrying, with exponential backoff (starting at initial,
// 1 s when not positive, doubling, capped at 1 minute), the services that
// still have no index until all are loaded, the total duration has passed or
// ctx ends. Each round loads the missing services concurrently. It is meant to
// run in a goroutine after LoadAll.
func (s *Store) RetryMissing(ctx context.Context, initial, total time.Duration) {
	deadline := time.Now().Add(total)
	delay := initial
	if delay <= 0 {
		delay = time.Second
	}
	for {
		if !s.anyMissing() || !time.Now().Before(deadline) {
			return
		}
		wait := delay
		if left := time.Until(deadline); wait > left {
			wait = left
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if !time.Now().Before(deadline) {
			return
		}
		s.ensureEach(ctx, true)
		if delay *= 2; delay > time.Minute {
			delay = time.Minute
		}
	}
}

func (s *Store) anyMissing() bool {
	for name, svc := range s.cfg.Services {
		if svc.OpenAPIURL != "" && s.GetIndex(name) == nil {
			return true
		}
	}
	return false
}

func (s *Store) load(ctx context.Context, name, url string) error {
	data, err := Fetch(ctx, url, s.cache)
	if err != nil {
		return err
	}

	idx, err := Parse(ctx, name, data)
	if err != nil {
		return err
	}

	s.mu.Lock()
	s.indices[name] = idx
	s.mu.Unlock()

	return nil
}

// GetIndex returns the index for a service.
func (s *Store) GetIndex(name string) *Index {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.indices[name]
}

// LoadedServices returns the names of services whose OpenAPI specs have been loaded.
func (s *Store) LoadedServices() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	names := make([]string, 0, len(s.indices))
	for name := range s.indices {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Search searches across all or a specific service.
func (s *Store) Search(query, serviceName string) []EndpointSummary {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var cands []scored
	ntok := 0

	names := make([]string, 0, len(s.indices))
	for name := range s.indices {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		if serviceName != "" && name != serviceName {
			continue
		}
		c, n := s.indices[name].rank(query)
		cands = append(cands, c...)
		ntok = n
	}
	// Tiers and ordering are decided across services, not per service.
	return selectRanked(cands, ntok)
}

// Refresh re-fetches the spec for a specific service.
func (s *Store) Refresh(ctx context.Context, name string) error {
	svc, ok := s.cfg.Services[name]
	if !ok {
		return fmt.Errorf("service %q not configured", name)
	}
	if svc.OpenAPIURL == "" {
		return fmt.Errorf("no OpenAPI URL for %s", name)
	}

	// Invalidate cache
	s.cache.Invalidate(svc.OpenAPIURL)

	return s.load(ctx, name, svc.OpenAPIURL)
}

// RefreshAll re-fetches specs for all services.
func (s *Store) RefreshAll(ctx context.Context) map[string]error {
	errors := make(map[string]error)
	for name, svc := range s.cfg.Services {
		if svc.OpenAPIURL == "" {
			continue
		}
		s.cache.Invalidate(svc.OpenAPIURL)
		if err := s.load(ctx, name, svc.OpenAPIURL); err != nil {
			errors[name] = err
		}
	}
	if len(errors) == 0 {
		return nil
	}
	return errors
}
