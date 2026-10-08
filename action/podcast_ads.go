package action

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/jakenesler/navigatorr/podcast"
	"github.com/jakenesler/navigatorr/transcode"
)

func valueAdReport(r *podcast.AdMatchReport) podcast.AdMatchReport {
	if r == nil {
		return podcast.AdMatchReport{}
	}
	return *r
}
func knownRanges(s podcastSession, b podcast.Block) []podcast.Decision {
	var ranges []podcast.Decision
	lastIndex := -2
	for i := b.First; i <= b.Last; i++ {
		u := s.Transcript.Units[i]
		k, ok := s.Known[u.ID]
		if !ok {
			continue
		}
		if len(ranges) > 0 && lastIndex == i-1 && ranges[len(ranges)-1].Label == k.Label && *ranges[len(ranges)-1].Evidence == k.Evidence {
			ranges[len(ranges)-1].LastID = u.ID
		} else {
			evidence := k.Evidence
			ranges = append(ranges, podcast.Decision{FirstID: u.ID, LastID: u.ID, Label: k.Label, Reason: "verified audio and native transcript text", Evidence: &evidence})
		}
		lastIndex = i
	}
	return ranges
}
func attachKnownAds(ec *ExecutionContext, s *podcastSession) error {
	if !s.Policy.KnownAdsFirstPass {
		return nil
	}
	var report podcast.AdMatchReport
	if err := podcast.ReadJSON(getString(ec.State, "podcast_match_path"), &report); err != nil {
		return err
	}
	known, err := podcast.KnownAdUnits(s.Transcript, report, s.Policy)
	if err != nil {
		return err
	}
	s.Matches = &report
	s.Known = known
	for _, b := range s.Blocks {
		if podcastAnalysisMode(ec) == podcast.AnalysisKnownAdsOnly {
			break // No pretend complete classifications for an automatic pass.
		}
		complete := true
		for i := b.First; i <= b.Last; i++ {
			if _, ok := known[s.Transcript.Units[i].ID]; !ok {
				complete = false
				break
			}
		}
		if !complete {
			continue
		}
		c := podcast.Classification{BlockID: b.ID, BlockDigest: b.Digest, TranscriptDigest: podcast.Digest(s.Transcript), PromptVersion: podcast.PromptVersion, Model: podcast.AdAlgorithm, Reasoning: "complete acoustic match"}
		c, err = podcast.MergeKnownAdDecisions(s.Transcript, b, c, known, nil)
		if err != nil {
			return err
		}
		s.Classifications[b.ID] = c
	}
	m := podcastSummary(ec)
	m["known_ad_units"] = len(known)
	m["llm_unknown_units"] = len(s.Transcript.Units) - len(known)
	m["acoustic_algorithm"] = podcast.AdAlgorithm
	return nil
}
func (e *Engine) checkKnownAds(ctx context.Context, s podcastSession) error {
	if s.Matches == nil || len(s.Known) == 0 {
		return nil
	}
	_, ok := e.deps.Transcode.(transcode.PodcastAdExecutor)
	if !ok {
		return fmt.Errorf("ad catalog API unavailable")
	}
	catalog, err := e.PodcastAdLibrary(ctx, s.Matches.Catalog.Scope, "")
	if err != nil {
		return err
	}
	active := map[string]podcast.AdReference{}
	for _, r := range catalog.References {
		if !r.Revoked {
			active[r.ID] = r
		}
	}
	original := map[string]podcast.AdReference{}
	for _, r := range s.Matches.Catalog.References {
		original[r.ID] = r
	}
	for _, k := range s.Known {
		r, ok := active[k.Evidence.ReferenceID]
		if !ok || r != original[k.Evidence.ReferenceID] {
			return fmt.Errorf("known ad reference changed or revoked; classifications, cuts and approval cannot be used; start a new action")
		}
	}
	return nil
}
func (e *Engine) PodcastAdLibrary(ctx context.Context, scope, revoke string) (podcast.AdCatalog, error) {
	if e.deps.Config == nil {
		return podcast.AdCatalog{}, fmt.Errorf("podcast config unavailable")
	}
	unlock, err := e.lockPodcastCatalog(ctx, scope)
	if err != nil {
		return podcast.AdCatalog{}, err
	}
	defer unlock()
	e.podcastCatalogMu.Lock()
	defer e.podcastCatalogMu.Unlock()
	if e.deps.Config == nil {
		return podcast.AdCatalog{}, fmt.Errorf("podcast config unavailable")
	}
	if _, err := e.deps.Config.Podcasts.Policy(scope); err != nil {
		return podcast.AdCatalog{}, err
	}
	client, ok := e.deps.Transcode.(transcode.PodcastAdExecutor)
	if !ok {
		return podcast.AdCatalog{}, fmt.Errorf("ad catalog API unavailable")
	}
	if revoke != "" {
		if !podcast.ValidHash(revoke) {
			return podcast.AdCatalog{}, fmt.Errorf("invalid reference ID")
		}
		tombstones, err := e.podcastTombstones(scope)
		if err != nil {
			return podcast.AdCatalog{}, err
		}
		tombstones[revoke] = true
		if err := podcast.WriteJSON(e.podcastCatalogPath(scope)+".tombstones", tombstones); err != nil {
			return podcast.AdCatalog{}, err
		}
		// Persist the conservative tombstone before contacting an intermittent worker.
		// A lost acknowledgement must never leave a local publisher using this reference.
		var cached podcast.AdCatalog
		if err := podcast.ReadJSON(e.podcastCatalogPath(scope), &cached); err == nil {
			if err := cached.Validate(); err != nil {
				return cached, err
			}
			for i := range cached.References {
				if cached.References[i].ID == revoke {
					cached.References[i].Revoked = true
				}
			}
			if err := e.writePodcastCatalog(scope, cached); err != nil {
				return cached, err
			}
		} else if !os.IsNotExist(err) {
			return cached, err
		}
		if err := client.RevokeAd(ctx, scope, revoke); err != nil {
			return podcast.AdCatalog{}, err
		}
	}
	catalog, err := client.AdCatalog(ctx, scope)
	if err != nil {
		return catalog, err
	}
	if err := catalog.Validate(); err != nil {
		return catalog, err
	}
	if catalog.Scope != scope {
		return catalog, fmt.Errorf("ad catalog scope differs")
	}
	tombstones, err := e.podcastTombstones(scope)
	if err != nil {
		return catalog, err
	}
	for i := range catalog.References {
		if tombstones[catalog.References[i].ID] {
			if !catalog.References[i].Revoked {
				_ = client.RevokeAd(ctx, scope, catalog.References[i].ID)
			}
			catalog.References[i].Revoked = true
		}
	}
	// Tombstones survive stale worker responses and restarts.
	var prior podcast.AdCatalog
	if err := podcast.ReadJSON(e.podcastCatalogPath(scope), &prior); err == nil {
		if err := prior.Validate(); err != nil {
			return catalog, err
		}
		for _, old := range prior.References {
			if !old.Revoked {
				continue
			}
			found := false
			for i := range catalog.References {
				if catalog.References[i].ID == old.ID {
					catalog.References[i].Revoked = true
					found = true
				}
			}
			if !found {
				catalog.References = append(catalog.References, old)
			}
		}
	} else if !os.IsNotExist(err) {
		return catalog, err
	}
	if err := catalog.Validate(); err != nil {
		return catalog, err
	}
	return catalog, e.writePodcastCatalog(scope, catalog)
}

func (e *Engine) podcastCatalogPath(scope string) string {
	dir := e.deps.Config.Podcasts.LocalCatalogDir
	if dir == "" {
		dir = filepath.Join(e.deps.Config.Podcasts.ArtifactDir, "_ad-catalog")
	}
	return filepath.Join(dir, fmt.Sprintf("%x", sha256.Sum256([]byte(scope)))+".json")
}

func (e *Engine) podcastTombstones(scope string) (map[string]bool, error) {
	m := map[string]bool{}
	if err := podcast.ReadJSON(e.podcastCatalogPath(scope)+".tombstones", &m); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	for id, v := range m {
		if !v || !podcast.ValidHash(id) {
			return nil, fmt.Errorf("invalid ad tombstone")
		}
	}
	return m, nil
}

func (e *Engine) writePodcastCatalog(scope string, catalog podcast.AdCatalog) error {
	file := e.podcastCatalogPath(scope)
	if err := podcast.WriteJSON(file, catalog); err != nil {
		return err
	}
	if e.deps.Config.Podcasts.LocalCatalogDir != "" {
		return os.Chmod(file, 0644)
	}
	return nil
}

// The consumer holds this same stable inode through the final audio swap.
func (e *Engine) lockPodcastCatalog(ctx context.Context, scope string) (func(), error) {
	file := e.podcastCatalogPath(scope) + ".lock"
	if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
		return nil, fmt.Errorf("create ad catalog lock directory: %w", err)
	}
	f, err := os.OpenFile(file, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0666)
	if err != nil {
		return nil, fmt.Errorf("open ad catalog lock: %w", err)
	}
	for {
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			// SMB implements flock as a mandatory byte-range lock. A chmod on
			// another descriptor can fail while the consumer holds that lock.
			// Update shared permissions only after we own the same stable inode.
			if e.deps.Config.Podcasts.LocalCatalogDir != "" {
				if err := f.Chmod(0666); err != nil {
					syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
					f.Close()
					return nil, fmt.Errorf("set shared ad catalog lock permissions: %w", err)
				}
			}
			return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
		} else if err != syscall.EWOULDBLOCK && err != syscall.EACCES {
			// SMB can report an overlapping nonblocking byte-range lock as
			// EACCES. Opening the file and chmod errors remain fatal; only this
			// flock contention waits, bounded by the request context below.
			f.Close()
			return nil, fmt.Errorf("acquire ad catalog flock: %w", err)
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}

// PodcastLocalLibrary reads current policy and a durable metadata mirror only.
// Waveforms stay on the consumer host; this never wakes or contacts the worker.
func (e *Engine) PodcastLocalLibrary(scope string) (map[string]any, error) {
	e.podcastCatalogMu.Lock()
	defer e.podcastCatalogMu.Unlock()
	if e.deps.Config == nil {
		return nil, fmt.Errorf("podcast config unavailable")
	}
	policy, err := e.deps.Config.Podcasts.Policy(scope)
	if err != nil {
		return nil, err
	}
	if !policy.KnownAdsFirstPass {
		return nil, fmt.Errorf("known-ad matching disabled for profile")
	}
	var catalog podcast.AdCatalog
	if err := podcast.ReadJSON(e.podcastCatalogPath(scope), &catalog); err != nil {
		return nil, fmt.Errorf("local catalog not synchronized; list podcast_ad_library while the worker is available")
	}
	if err := catalog.Validate(); err != nil {
		return nil, err
	}
	if catalog.Scope != scope {
		return nil, fmt.Errorf("ad catalog scope differs")
	}
	tombstones, err := e.podcastTombstones(scope)
	if err != nil {
		return nil, err
	}
	for i := range catalog.References {
		if tombstones[catalog.References[i].ID] {
			catalog.References[i].Revoked = true
		}
	}
	return map[string]any{"policy": policy, "policy_digest": podcast.Digest(policy), "catalog": catalog, "catalog_digest": podcast.Digest(catalog)}, nil
}
