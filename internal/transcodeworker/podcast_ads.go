package transcodeworker

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jakenesler/navigatorr/podcast"
	"github.com/jakenesler/navigatorr/podcast/acoustic"
)

type adRecording struct {
	TextDigest           string `json:"text_digest"`
	Algorithm            string `json:"algorithm"`
	Scope                string `json:"scope"`
	Label                string `json:"label"`
	SourceHash           string `json:"source_hash"`
	TranscriptDigest     string `json:"transcript_digest"`
	CutsDigest           string `json:"cuts_digest"`
	ClassificationDigest string `json:"classification_digest"`
	FirstID              string `json:"first_id"`
	LastID               string `json:"last_id"`
	StartMS              int64  `json:"start_ms"`
	EndMS                int64  `json:"end_ms"`
	PCM                  []byte `json:"pcm"`
}

func (w *Worker) adDir(scope string) string {
	return filepath.Join(w.cfg.StateDir, "_podcast-ad-library", strings.TrimPrefix(podcast.Digest(scope), "sha256:"))
}
func adFile(dir, id string) string {
	return filepath.Join(dir, strings.TrimPrefix(id, "sha256:")+".json")
}
func recordingReference(r adRecording) podcast.AdReference {
	digest := podcast.Digest(r)
	return podcast.AdReference{TextDigest: r.TextDigest, ID: digest, Label: r.Label, Digest: digest, DurationMS: r.EndMS - r.StartMS, SourceHash: r.SourceHash, FirstID: r.FirstID, LastID: r.LastID, CutsDigest: r.CutsDigest}
}
func (w *Worker) adCatalog(scope string) (podcast.AdCatalog, error) {
	c := podcast.AdCatalog{Algorithm: podcast.AdAlgorithm, Scope: scope, References: []podcast.AdReference{}}
	if err := c.Validate(); err != nil {
		return c, err
	}
	dir := w.adDir(scope)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return c, err
	}
	lock, err := acquireJobLock(dir)
	if err != nil {
		return c, err
	}
	defer lock.Unlock()
	return readAdCatalog(dir, scope)
}
func readAdCatalog(dir, scope string) (podcast.AdCatalog, error) {
	c := podcast.AdCatalog{Algorithm: podcast.AdAlgorithm, Scope: scope, References: []podcast.AdReference{}}
	entries, err := filepath.Glob(filepath.Join(dir, "*.meta"))
	if err != nil {
		return c, err
	}
	sort.Strings(entries)
	if len(entries) > podcast.MaxAdReferences {
		return c, fmt.Errorf("ad library capacity exceeded")
	}
	for _, file := range entries {
		var ref podcast.AdReference
		if err := podcast.ReadJSON(file, &ref); err != nil {
			return c, err
		}
		if filepath.Base(file) != strings.TrimPrefix(ref.ID, "sha256:")+".json.meta" || ref.Revoked {
			return c, fmt.Errorf("immutable ad metadata changed")
		}
		if _, err := os.Stat(adFile(dir, ref.ID) + ".revoked"); err == nil {
			ref.Revoked = true
		} else if !os.IsNotExist(err) {
			return c, err
		}
		c.References = append(c.References, ref)
	}
	return c, c.Validate()
}
func (w *Worker) revokeAd(scope, id string) error {
	if !podcast.ValidHash(id) {
		return fmt.Errorf("invalid reference ID")
	}
	dir := w.adDir(scope)
	lock, err := acquireJobLock(dir)
	if err != nil {
		return err
	}
	defer lock.Unlock()
	if _, err := os.Stat(adFile(dir, id)); err != nil {
		return err
	}
	return podcast.WriteJSON(adFile(dir, id)+".revoked", map[string]any{"reference_id": id, "revoked": true})
}

// Caller holds the catalog lock. A manifest can outlive later additions, but
// never a tombstone or changed immutable recording.
func checkAdReferences(dir string, c podcast.AdCatalog, ids map[string]bool) error {
	if err := c.Validate(); err != nil {
		return err
	}
	for _, ref := range c.References {
		if ids != nil && !ids[ref.ID] {
			continue
		}
		if ref.Revoked {
			return fmt.Errorf("ad reference revoked: %s", ref.ID)
		}
		if _, err := os.Stat(adFile(dir, ref.ID) + ".revoked"); err == nil {
			return fmt.Errorf("ad reference revoked: %s; start a new cleaning action", ref.ID)
		} else if !os.IsNotExist(err) {
			return err
		}
		var actual podcast.AdReference
		if err := podcast.ReadJSON(adFile(dir, ref.ID)+".meta", &actual); err != nil {
			return err
		}
		if actual != ref {
			return fmt.Errorf("ad reference identity mismatch")
		}

	}
	return nil
}
func (w *Worker) decodeAdPCM(ctx context.Context, dir, input string) (*os.File, error) {
	path := filepath.Join(dir, "podcast-ads.pcm")
	if err := w.podcastCommand(ctx, dir, w.ffmpegPath, "-nostdin", "-v", "error", "-xerror", "-y", "-i", input, "-map", "0:a:0", "-ac", "1", "-ar", "8000", "-f", "s16le", "-fs", strconv.FormatInt(acoustic.MaxSamples*2+2, 10), path); err != nil {
		os.Remove(path)
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil || info.Size() < 512 || info.Size()%2 != 0 || info.Size() > acoustic.MaxSamples*2 {
		f.Close()
		os.Remove(path)
		return nil, fmt.Errorf("decoded PCM outside bounded duration")
	}
	return f, nil
}
func (w *Worker) matchAds(ctx context.Context, dir, input, source string, c podcast.AdCatalog) (podcast.AdMatchReport, error) {
	started := time.Now()
	r := podcast.AdMatchReport{Algorithm: podcast.AdAlgorithm, SourceHash: source, Catalog: c, Matches: []podcast.AdMatch{}}
	catalogDir := w.adDir(c.Scope)
	lock, err := acquireJobLock(catalogDir)
	if err != nil {
		return r, err
	}
	active := c
	active.References = nil
	for _, ref := range c.References {
		if !ref.Revoked {
			active.References = append(active.References, ref)
		}
	}
	err = checkAdReferences(catalogDir, active, nil)
	lock.Unlock()
	if err != nil {
		return r, err
	}
	f, err := w.decodeAdPCM(ctx, dir, input)
	if err != nil {
		return r, err
	}
	defer func() { f.Close(); os.Remove(f.Name()) }()
	info, _ := f.Stat()
	r.DurationMS = info.Size() / 2 * 1000 / acoustic.Rate
	r.DecodeSeconds = time.Since(started).Seconds()
	searchStarted := time.Now()
	if len(active.References) > 0 {
		pcm, err := acoustic.Prepare(ctx, f, info.Size()/2)
		if err != nil {
			return r, err
		}
		for _, ref := range active.References {
			var recording adRecording
			if err := podcast.ReadJSON(adFile(catalogDir, ref.ID), &recording); err != nil {
				return r, err
			}
			if recording.Scope != c.Scope || recording.Algorithm != c.Algorithm || recordingReference(recording) != ref {
				return r, fmt.Errorf("ad recording digest differs")
			}
			samples := make([]int16, len(recording.PCM)/2)
			for i := range samples {
				samples[i] = int16(binary.LittleEndian.Uint16(recording.PCM[i*2:]))
			}
			matches, err := pcm.Find(ctx, samples)
			if err != nil {
				return r, err
			}
			for _, m := range matches {
				start := m.StartSample * 1000 / acoustic.Rate
				r.Matches = append(r.Matches, podcast.AdMatch{ReferenceID: ref.ID, Label: ref.Label, StartMS: start, EndMS: start + ref.DurationMS, MinCorrelation: m.MinCorrelation})
			}
		}
	}
	sort.Slice(r.Matches, func(i, j int) bool {
		if r.Matches[i].StartMS == r.Matches[j].StartMS {
			return r.Matches[i].ReferenceID < r.Matches[j].ReferenceID
		}
		return r.Matches[i].StartMS < r.Matches[j].StartMS
	})
	r.SearchSeconds = time.Since(searchStarted).Seconds()
	r.WallSeconds = time.Since(started).Seconds()
	return r, r.Validate()
}
func (w *Worker) learnAds(ctx context.Context, dir, file, input string, t podcast.Transcript, task *podcast.Task) (int, error) {
	if task.Learning == nil {
		return 0, nil
	}
	seeds, err := podcast.AdSeeds(t, *task.Learning, *task.Cuts)
	if err != nil {
		return 0, err
	}
	if len(seeds) == 0 {
		return 0, nil
	}
	f, err := w.decodeAdPCM(ctx, dir, input)
	if err != nil {
		return 0, err
	}
	defer func() { f.Close(); os.Remove(f.Name()) }()
	pcmInfo, err := f.Stat()
	if err != nil {
		return 0, err
	}
	pcmDurationMS := pcmInfo.Size() / 2 * 1000 / acoustic.Rate
	index := map[string]podcast.Unit{}
	positions := map[string]int{}
	for i, u := range t.Units {
		index[u.ID] = u
		positions[u.ID] = i
	}
	var recordings []adRecording
	pcmBudget := 16 << 20
	for _, s := range seeds {
		first, last := index[s.FirstID], index[s.LastID]
		endMS := min(last.EndMS, t.DurationMS, pcmDurationMS)
		if endMS-first.StartMS < 8000 {
			continue
		}
		if len(recordings) >= podcast.MaxAdReferences || int((endMS-first.StartMS)*acoustic.Rate/1000)*2 > pcmBudget {
			break
		}
		samples, err := acoustic.ReadSamples(f, first.StartMS*acoustic.Rate/1000, int((endMS-first.StartMS)*acoustic.Rate/1000))
		if err != nil {
			return 0, err
		}
		if !acoustic.Informative(samples) {
			continue
		}
		raw := make([]byte, len(samples)*2)
		pcmBudget -= len(raw)
		for i, v := range samples {
			binary.LittleEndian.PutUint16(raw[i*2:], uint16(v))
		}
		recordings = append(recordings, adRecording{TextDigest: podcast.NativeAdTextDigest(t.Units[positions[s.FirstID] : positions[s.LastID]+1]), Algorithm: podcast.AdAlgorithm, Scope: task.Learning.Scope, Label: s.Label, SourceHash: t.SourceHash, TranscriptDigest: podcast.Digest(t), CutsDigest: podcast.Digest(task.Cuts), ClassificationDigest: task.Cuts.ClassificationDigest, FirstID: s.FirstID, LastID: s.LastID, StartMS: first.StartMS, EndMS: endMS, PCM: raw})
	}
	// Expensive waveform dedup runs outside both cancellation and library locks.
	for attempt := 0; attempt < 5; attempt++ {
		snapshot, err := w.adCatalog(task.Learning.Scope)
		if err != nil {
			return 0, err
		}
		catalogDir := w.adDir(task.Learning.Scope)
		var fresh []adRecording
		for _, r := range recordings {
			ref := recordingReference(r)
			found := false
			for _, old := range snapshot.References {
				if old.ID == ref.ID {
					found = true
					break
				}
				if old.TextDigest != ref.TextDigest || old.DurationMS-ref.DurationMS > 2000 || ref.DurationMS-old.DurationMS > 2000 {
					continue
				}
				var prior adRecording
				if err := podcast.ReadJSON(adFile(catalogDir, old.ID), &prior); err != nil {
					return 0, err
				}
				if podcast.Digest(prior) != old.Digest {
					return 0, fmt.Errorf("ad recording changed")
				}
				same, err := acoustic.SameRecording(ctx, r.PCM, prior.PCM)
				if err != nil {
					return 0, err
				}
				if same {
					found = true
					break
				}
			}
			if !found {
				for _, prior := range fresh {
					if r.TextDigest != prior.TextDigest {
						continue
					}
					same, err := acoustic.SameRecording(ctx, r.PCM, prior.PCM)
					if err != nil {
						return 0, err
					}
					if same {
						found = true
						break
					}
				}
			}
			if !found {
				fresh = append(fresh, r)
			}
		}
		count := 0
		changed := false
		err = w.guardPodcastPublication(ctx, dir, file, func() error {
			lock, err := acquireJobLock(catalogDir)
			if err != nil {
				return err
			}
			defer lock.Unlock()
			latest, err := readAdCatalog(catalogDir, task.Learning.Scope)
			if err != nil {
				return err
			}
			if podcast.Digest(latest) != podcast.Digest(snapshot) {
				changed = true
				return nil
			}
			for _, r := range fresh {
				if err := ctx.Err(); err != nil {
					return err
				}
				if len(latest.References) >= podcast.MaxAdReferences {
					break
				}
				ref := recordingReference(r)
				path := adFile(catalogDir, ref.ID)
				if existing, err := os.ReadFile(path); err == nil {
					var previous adRecording
					if err := json.Unmarshal(existing, &previous); err != nil || podcast.Digest(previous) != ref.Digest {
						return fmt.Errorf("immutable ad recording collision")
					}
				} else if !os.IsNotExist(err) {
					return err
				} else if err := podcast.WriteJSON(path, r); err != nil {
					return err
				}
				if err := podcast.WriteJSON(path+".meta", ref); err != nil {
					return err
				}
				latest.References = append(latest.References, ref)
				count++
			}
			return nil
		})
		if err != nil {
			return count, err
		}
		if !changed {
			return count, nil
		}
	}
	return 0, fmt.Errorf("ad catalog changed repeatedly; retry the existing render with its ASR checkpoint")
}

func (w *Worker) adEvidenceReport(task *podcast.Task, source string) (*podcast.AdMatchReport, error) {
	if task.MatchJobID == "" {
		return nil, nil
	}
	job, err := LoadJob(filepath.Join(w.cfg.StateDir, task.MatchJobID, "job.json"))
	if err != nil {
		return nil, err
	}
	if job.Status != "completed" || job.Podcast == nil || job.Plan == nil || job.Plan.Podcast == nil || job.Plan.Podcast.Operation != "match_ads" || job.Podcast.SourceHash != "sha256:"+strings.TrimPrefix(source, "sha256:") {
		return nil, fmt.Errorf("matching job evidence differs")
	}
	var r podcast.AdMatchReport
	if err := podcast.ReadJSON(filepath.Join(w.cfg.StateDir, task.MatchJobID, "podcast-matches.json"), &r); err != nil {
		return nil, err
	}
	if err := r.Validate(); err != nil {
		return nil, err
	}
	if podcast.Digest(r) != job.Podcast.MatchDigest || r.SourceHash != job.Podcast.SourceHash || podcast.Digest(r.Catalog) != podcast.Digest(job.Plan.Podcast.Catalog) {
		return nil, fmt.Errorf("matching report identity differs")
	}
	return &r, nil
}
func (w *Worker) verifyAdDecisionEvidence(task *podcast.Task, t podcast.Transcript, source string) error {
	report, err := w.adEvidenceReport(task, source)
	if err != nil {
		return err
	}
	known := map[string]podcast.AdKnownUnit{}
	if report != nil {
		if task.Learning == nil || report.Catalog.Scope != task.Learning.Scope {
			return fmt.Errorf("matching scope/policy missing")
		}
		known, err = podcast.KnownAdUnits(t, *report, task.Learning.Policy)
		if err != nil {
			return err
		}
	}
	if task.Learning != nil {
		index := map[string]int{}
		for i, u := range t.Units {
			index[u.ID] = i
		}
		for _, c := range task.Learning.Classifications {
			for _, d := range c.Decisions {
				if d.Evidence == nil {
					continue
				}
				first, ok := index[d.FirstID]
				last, ok2 := index[d.LastID]
				if !ok || !ok2 || last < first {
					return fmt.Errorf("evidence unit bounds invalid")
				}
				for i := first; i <= last; i++ {
					k, ok := known[t.Units[i].ID]
					if !ok || k.Label != d.Label || k.Evidence != *d.Evidence {
						return fmt.Errorf("unverified acoustic decision")
					}
				}
			}
		}
	}
	return nil
}

// Tombstones serialize with the actual atomic publication, including recovery
// of an EncodeComplete checkpoint. Revoke cannot race the last verification.
func (w *Worker) guardPodcastAdPublication(ctx context.Context, dir, file string, publish func() error) error {
	return w.guardPodcastPublication(ctx, dir, file, func() error {
		job, err := LoadJob(file)
		if err != nil {
			return err
		}
		release, err := w.lockAdEvidence(job)
		if err != nil {
			return err
		}
		defer release()
		return publish()
	})
}

// Always called after acquiring the job lock, then the library lock, so both
// atomic publication and terminal checkpoint recovery serialize with revoke.
func (w *Worker) lockAdEvidence(job *JobRecord) (func(), error) {
	noop := func() {}
	if job.Plan == nil || job.Plan.Podcast == nil {
		return noop, nil
	}
	task := job.Plan.Podcast
	report, err := w.adEvidenceReport(task, job.SourceSHA256)
	if err != nil {
		return nil, err
	}
	if report == nil {
		return noop, nil
	}
	ids := map[string]bool{}
	if task.Learning != nil {
		for _, c := range task.Learning.Classifications {
			for _, d := range c.Decisions {
				if d.Evidence != nil {
					ids[d.Evidence.ReferenceID] = true
				}
			}
		}
	}
	catalogDir := w.adDir(report.Catalog.Scope)
	lock, err := acquireJobLock(catalogDir)
	if err != nil {
		return nil, err
	}
	if err := checkAdReferences(catalogDir, report.Catalog, ids); err != nil {
		lock.Unlock()
		return nil, err
	}
	return func() { lock.Unlock() }, nil
}
func (s *Server) handleAdLibrary(w http.ResponseWriter, r *http.Request) {
	scope := r.URL.Query().Get("scope")
	if scope == "" || len(scope) > 128 {
		http.Error(w, "invalid scope", 400)
		return
	}
	switch r.Method {
	case http.MethodGet:
		c, err := s.worker.adCatalog(scope)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		writeHTTPJSON(w, 200, c)
	case http.MethodPost:
		if err := s.worker.revokeAd(scope, r.URL.Query().Get("revoke")); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		writeHTTPJSON(w, 200, map[string]bool{"revoked": true})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}
