package action

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jakenesler/navigatorr/store"
)

// The encode history. EncodeSamples is the one builder of EncodeSample from
// the transcode actions on record: the maintenance page's wait estimates and
// the node availability windows (worker_window.go) both read it.

// EncodeRecord is what a sample is read from: a transcode action with its
// decoded outputs and state (a key found in the outputs wins over the state).
type EncodeRecord struct {
	ID, ActionName, Status, UpdatedAt string
	Outputs, State                    map[string]any
}

func (r EncodeRecord) value(key string) any {
	if v, ok := r.Outputs[key]; ok {
		return v
	}
	return r.State[key]
}

// EncodeShape is what an encode's duration depends on, read from a record.
type EncodeShape struct {
	Worker, Class, Family string
	DurationSec           float64
}

func encodeMap(v any) map[string]any { m, _ := v.(map[string]any); return m }
func encodeStr(v any) string         { s, _ := v.(string); return s }
func encodeNum(v any) float64        { f, _ := v.(float64); return f }

// RecordEncodeShape reads a transcode job's worker, height class, encoder
// family and video length from the trimmed snapshot. The family comes from the
// encoder the plan chose: the size budget's encoder choice, else the search's
// winner, which the snapshot keeps; an audio-only plan copies the video. A job
// that skipped the transcode has none.
func RecordEncodeShape(r EncodeRecord) (EncodeShape, bool) {
	if r.ActionName != "transcode_media" || r.value("skip_transcode") == true {
		return EncodeShape{}, false
	}
	original := encodeMap(r.value("original"))
	width, height := 0, 0
	if w, h, ok := strings.Cut(encodeStr(original["resolution"]), "x"); ok {
		width, _ = strconv.Atoi(w)
		height, _ = strconv.Atoi(h)
	}
	family := encodeFamilyOf(r)
	s := EncodeShape{Worker: encodeStr(r.State["worker"]), Class: ResolutionClass(width, height), Family: family, DurationSec: encodeNum(original["duration_sec"])}
	return s, s.Worker != "" && s.Class != "" && s.Family != "" && s.DurationSec > 0
}

// encodeFamilyOf is the encoder family a record's plan chose, "" while none
// is known: an audio-only plan copies the video, else the size budget's
// encoder choice, the search's winner or the quality ladder names the codec.
func encodeFamilyOf(r EncodeRecord) string {
	if r.value("size_audio_only") == true {
		return EncodeFamilyCopy
	}
	codec := encodeStr(encodeMap(encodeMap(r.value("size_budget"))["encoder_choice"])["video_codec"])
	if codec == "" {
		codec = encodeStr(encodeMap(encodeMap(r.value("benchmark_decision"))["winner"])["video_codec"])
	}
	if codec == "" {
		// A size goal without a budget runs the quality ladder instead.
		codec = encodeStr(encodeMap(r.value("quality_ladder"))["video_codec"])
	}
	return EncodeFamily(codec)
}

// encodeSampleOf is the sample a finished transcode record gives.
func encodeSampleOf(r EncodeRecord) (EncodeSample, bool) {
	if r.ActionName != "transcode_media" || r.Status != store.ActionStatusCompleted {
		return EncodeSample{}, false
	}
	ms := encodeNum(r.value("encode_duration_ms"))
	shape, ok := RecordEncodeShape(r)
	if ms <= 0 || !ok {
		return EncodeSample{}, false
	}
	return EncodeSample{Worker: shape.Worker, Class: shape.Class, Family: shape.Family, DurationSec: shape.DurationSec, EncodeSec: ms / 1000}, true
}

// EncodeSamples are the finished encodes of the history, newest first.
func EncodeSamples(records []EncodeRecord) []EncodeSample {
	done := []EncodeRecord{}
	for _, r := range records {
		if r.ActionName == "transcode_media" && r.Status == store.ActionStatusCompleted {
			done = append(done, r)
		}
	}
	sort.Slice(done, func(i, j int) bool { return encodeRecordNewer(done[i], done[j]) })
	var out []EncodeSample
	for _, r := range done {
		if s, ok := encodeSampleOf(r); ok {
			out = append(out, s)
		}
	}
	return out
}

func encodeRecordNewer(a, b EncodeRecord) bool {
	if a.UpdatedAt == b.UpdatedAt {
		return a.ID > b.ID
	}
	return a.UpdatedAt > b.UpdatedAt
}

// encodeSampleTTL is how long the engine keeps the samples it read: the
// history only grows as encodes finish, and a window's decision does not need
// the last one.
const encodeSampleTTL = 5 * time.Minute

// encodeHistory is the engine's cache of the encode samples.
type encodeHistory struct {
	mu      sync.Mutex
	at      time.Time
	samples []EncodeSample
	// memo is the sample (or the lack of one) each finished action gave, by
	// revision, so a refresh decodes only the actions written since.
	memo map[string]encodeMemo
}

type encodeMemo struct {
	rev       int64
	updatedAt string
	sample    EncodeSample
	ok        bool
}

// encodeSamples returns the newest-first samples, read from the store at most
// every encodeSampleTTL. Without a store there are none. A read that fails
// gives none for this call (no estimate, which the windows treat as not
// fitting) and is not remembered: the next call reads again.
func (e *Engine) encodeSamples() []EncodeSample {
	h := &e.history
	h.mu.Lock()
	defer h.mu.Unlock()
	now := e.now()
	if !h.at.IsZero() && now.Sub(h.at) < encodeSampleTTL {
		return h.samples
	}
	var samples []EncodeSample
	var err error
	switch {
	case e.loadEncodeSamples != nil:
		samples, err = e.loadEncodeSamples()
	case e.deps.Store == nil:
	default:
		samples, err = e.readEncodeSamples(h)
	}
	if err != nil {
		return nil
	}
	h.at, h.samples = now, samples
	return samples
}

// readEncodeSamples builds the samples from the store's action snapshot (the
// one the maintenance page reads, itself kept by revision).
func (e *Engine) readEncodeSamples(h *encodeHistory) ([]EncodeSample, error) {
	instances, err := e.deps.Store.ListMaintenanceActionSnapshot()
	if err != nil {
		return nil, err
	}
	if h.memo == nil {
		h.memo = map[string]encodeMemo{}
	}
	seen := make(map[string]bool, len(instances))
	type ref struct{ id, updatedAt string }
	var refs []ref
	for _, inst := range instances {
		if inst.ActionName != "transcode_media" || inst.Status != store.ActionStatusCompleted {
			continue
		}
		seen[inst.ID] = true
		m, have := h.memo[inst.ID]
		if !have || inst.Rev == 0 || m.rev != inst.Rev || m.updatedAt != inst.UpdatedAt {
			rec := EncodeRecord{ID: inst.ID, ActionName: inst.ActionName, Status: inst.Status, UpdatedAt: inst.UpdatedAt,
				Outputs: decodeJSONObject(inst.OutputsJSON), State: decodeJSONObject(inst.StateJSON)}
			// Only the sample is kept, not the decoded documents.
			s, ok := encodeSampleOf(rec)
			m = encodeMemo{rev: inst.Rev, updatedAt: inst.UpdatedAt, sample: s, ok: ok}
			h.memo[inst.ID] = m
		}
		if m.ok {
			refs = append(refs, ref{inst.ID, inst.UpdatedAt})
		}
	}
	for id := range h.memo {
		if !seen[id] {
			delete(h.memo, id)
		}
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].updatedAt == refs[j].updatedAt {
			return refs[i].id > refs[j].id
		}
		return refs[i].updatedAt > refs[j].updatedAt
	})
	out := make([]EncodeSample, 0, len(refs))
	for _, r := range refs {
		out = append(out, h.memo[r.id].sample)
	}
	return out, nil
}

func decodeJSONObject(raw string) map[string]any {
	var out map[string]any
	_ = json.Unmarshal([]byte(raw), &out)
	return out
}
