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
	attempts      map[string]time.Time
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
		retryInterval: defaultRetryInterval,
		fetchTimeout:  defaultFetchTimeout,
		now:           time.Now,
	}
}

// LoadAll fetches and parses specs for all configured services.
func (s *Store) LoadAll(ctx context.Context) {
	for name, svc := range s.cfg.Services {
		if svc.OpenAPIURL == "" {
			continue
		}
		s.recordAttempt(name)
		if err := s.load(ctx, name, svc.OpenAPIURL); err != nil {
			internal.Errorf("loading spec for %s: %v", name, err)
		} else {
			idx := s.GetIndex(name)
			if idx != nil {
				internal.Logf("loaded %s: %d endpoints", name, idx.Count())
			}
		}
	}
}

func (s *Store) recordAttempt(name string) {
	s.mu.Lock()
	s.attempts[name] = s.now()
	s.mu.Unlock()
}

// EnsureLoaded makes a lazy load attempt for a configured service that has no
// index yet. Attempts are rate-limited per service, each fetch is bounded by a
// timeout, and the lock is never held while fetching. It is a no-op for a
// loaded or unknown service.
func (s *Store) EnsureLoaded(ctx context.Context, name string) {
	s.ensure(ctx, name, false)
}

// EnsureAll runs EnsureLoaded for every configured service, concurrently.
func (s *Store) EnsureAll(ctx context.Context) {
	var wg sync.WaitGroup
	for name, svc := range s.cfg.Services {
		if svc.OpenAPIURL == "" {
			continue
		}
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			s.ensure(ctx, name, false)
		}(name)
	}
	wg.Wait()
}

// ensure loads the service when it has no index. force skips the per-service
// rate limit (used by the startup retry loop, which has its own backoff).
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
	now := s.now()
	if last, tried := s.attempts[name]; tried && !force && now.Sub(last) < s.retryInterval {
		s.mu.Unlock()
		return false
	}
	s.attempts[name] = now
	timeout := s.fetchTimeout
	s.mu.Unlock()

	fctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := s.load(fctx, name, svc.OpenAPIURL); err != nil {
		internal.Errorf("lazy load of spec for %s: %v", name, err)
		return false
	}
	if idx := s.GetIndex(name); idx != nil {
		internal.Logf("loaded %s: %d endpoints", name, idx.Count())
	}
	return true
}

// RetryMissing keeps retrying, with exponential backoff (starting at initial,
// doubling, capped at 1 minute), the services that still have no index until
// all are loaded, the total duration has passed or ctx ends. It is meant to
// run in a goroutine after LoadAll.
func (s *Store) RetryMissing(ctx context.Context, initial, total time.Duration) {
	deadline := time.Now().Add(total)
	delay := initial
	for {
		missing := false
		for name, svc := range s.cfg.Services {
			if svc.OpenAPIURL != "" && s.GetIndex(name) == nil {
				missing = true
			}
		}
		if !missing || time.Now().After(deadline) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		for name, svc := range s.cfg.Services {
			if svc.OpenAPIURL != "" {
				s.ensure(ctx, name, true)
			}
		}
		if delay *= 2; delay > time.Minute {
			delay = time.Minute
		}
	}
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
