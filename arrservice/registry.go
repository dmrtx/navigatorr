package arrservice

import (
	"fmt"
	"sort"
	"time"

	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/resilience"
	"github.com/jakenesler/navigatorr/snapshot"
)

// Registry holds all configured services.
type Registry struct {
	services  map[string]*Service
	Pool      *resilience.ServicePool
	Snapshots *snapshot.Store
}

// NewRegistry creates a registry from config.
func NewRegistry(cfg *config.Config) *Registry {
	maxPerSvc := 3
	maxMedia := 2
	if cfg != nil {
		if cfg.Concurrency.MaxAPISimultaneous > 0 {
			maxPerSvc = cfg.Concurrency.MaxAPISimultaneous
		}
		if cfg.Concurrency.MaxInspectSimultaneous > 0 {
			maxMedia = cfg.Concurrency.MaxInspectSimultaneous
		}
	}
	pool := resilience.NewServicePool(maxPerSvc, maxMedia)
	snaps := snapshot.NewStore(5 * time.Minute)

	r := &Registry{
		services:  make(map[string]*Service),
		Pool:      pool,
		Snapshots: snaps,
	}
	if cfg != nil {
		for name, svcCfg := range cfg.Services {
			svc := NewService(name, svcCfg)
			svc.Pool = pool
			svc.Snapshots = snaps
			r.services[name] = svc
		}
	}
	return r
}

// Get returns a service by name.
func (r *Registry) Get(name string) (*Service, error) {
	svc, ok := r.services[name]
	if !ok {
		return nil, fmt.Errorf("service %q not found", name)
	}
	return svc, nil
}

// List returns all service names sorted.
func (r *Registry) List() []string {
	names := make([]string, 0, len(r.services))
	for name := range r.services {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// All returns all services.
func (r *Registry) All() map[string]*Service {
	return r.services
}
