package acceptance

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/internal/transcodeworker"
	"github.com/jakenesler/navigatorr/transcode"
)

// envClips names a directory of short real media clips (*.mkv). Each one is
// run through the size goal on the x265 ladder, so a pipeline change is
// checked on representative content before it is deployed.
const envClips = "NAVIGATORR_ACCEPTANCE_CLIPS"

// clipFiles lists the clips to run: every *.mkv of the clips directory, sorted;
// none when the variable is unset.
func clipFiles(t *testing.T) []string {
	t.Helper()
	dir := strings.TrimSpace(os.Getenv(envClips))
	if dir == "" {
		return nil
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.mkv"))
	if err != nil {
		t.Fatalf("listing %s: %v", envClips, err)
	}
	if len(files) == 0 {
		t.Fatalf("%s=%s holds no *.mkv clips", envClips, dir)
	}
	sort.Strings(files)
	return files
}

// copyClip puts a copy of the clip into the suite's media root.
func (s *suite) copyClip(t *testing.T, from string) string {
	t.Helper()
	to := filepath.Join(s.root, filepath.Base(from))
	in, err := os.Open(from)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	return to
}

// clipRefusals are the documented reasons a size goal may end without a
// candidate on real content: the content did not reach the goal, nothing is
// wrong with the tracks.
var clipRefusals = map[string]bool{
	"insufficient_savings":          true,
	"size_growth":                   true,
	"size_growth_exceeded":          true,
	"size_budget_exceeded":          true,
	"quality_not_met":               true,
	"no_suitable_candidate":         true,
	"already_target_codec":          true,
	"unsupported_source":            true,
	"audio_compaction_insufficient": true,
	"audio_estimate_unavailable":    true,
	"audio_only_nothing_to_convert": true,
	"audio_only_worker_unsupported": true,
}

// trackLoss reports a refusal that says streams or video properties were lost:
// on real content that is a false refusal, a finding about the pipeline.
func trackLoss(code, outcome string) bool {
	return code == "preservation_not_met" || outcome == "preservation_rejected" ||
		strings.Contains(code, "track") || strings.Contains(code, "stream") ||
		strings.Contains(code, "subtitle") || strings.Contains(code, "attachment") || strings.Contains(code, "chapter")
}

// caseClip runs one real clip through the size goal (hardware off, no
// replacement) and, when a candidate comes out, compares every kept stream of
// the candidate with the source.
func caseClip(k *kase, clip string) {
	k.needCaps("libx265", "aac", "vmaf")
	k.row.Goal = "size+x265, real clip"
	src := k.s.copyClip(k.T, clip)
	sha := k.sha(src)
	r := k.s.transcode(k.T, src, map[string]any{"mode": "size", "allow_hardware": false}, caseTimeout)
	k.fill(src, r)
	k.fillValidation(r)

	if r.outcome() != "candidate_ready" {
		code := r.reason()
		k.require(!trackLoss(code, r.outcome()), "false refusal on real content: %s", r.describe())
		k.require(r.res.Status == "completed" && clipRefusals[code] && r.candidate() == "",
			"neither a candidate nor a documented skip or refusal: %s", r.describe())
		k.require(k.sha(src) == sha, "the original changed during a refused run")
		return
	}

	var plan transcode.Plan
	k.require(decode(pick(r.res, "plan"), &plan), "the result records no plan: %s", r.describe())
	orig := k.s.probe(k.T, src)
	keptAudio := keptStreams(orig, "audio", plan.Tracks)
	keptSubs := keptStreams(orig, "subtitle", plan.Tracks)
	k.require(len(keptAudio) > 0 || countKind(orig, "audio") == 0, "the plan keeps none of the source's %d audio streams", countKind(orig, "audio"))

	// The suite's candidate checks: decodes end to end, the original's hash is
	// unchanged and recorded, HEVC video, duration, passing quality evidence.
	// The audio count is the plan's, not the source's.
	k.expectCandidate(src, sha, r, candidateWant{audioTracks: len(keptAudio)})
	k.expectNoSampleTest(r)

	cand := k.s.probe(k.T, r.candidate())
	k.compareTracks(src, r.candidate(), &plan, orig, cand, keptAudio, keptSubs)
}

// fillValidation records the validation phase's wall time: the worker's phase
// cost when it states one, else the action's own measurement.
func (k *kase) fillValidation(r *result) {
	var costs map[string]transcode.PhaseCost
	if decode(pick(r.res, "phase_costs"), &costs) {
		if c, ok := costs["validation"]; ok && c.DurationMs > 0 {
			k.row.ValidationSeconds = float64(c.DurationMs) / 1000
			return
		}
	}
	switch v := pick(r.res, "validation_duration_ms").(type) {
	case float64:
		k.row.ValidationSeconds = v / 1000
	case int64:
		k.row.ValidationSeconds = float64(v) / 1000
	case int:
		k.row.ValidationSeconds = float64(v) / 1000
	}
}

// keptStreams are the source's streams of a kind that the plan keeps, in
// source order: all of them without a track selection, else the ones the
// selection lists (by source stream index).
func keptStreams(p probeInfo, kind string, sel *transcode.TrackSelection) []probeStream {
	var out []probeStream
	for _, st := range p.Streams {
		if st.CodecType == kind && sel.Keeps(kind, st.Index) {
			out = append(out, st)
		}
	}
	return out
}

func ofKind(p probeInfo, kind string) []probeStream {
	var out []probeStream
	for _, st := range p.Streams {
		if st.CodecType == kind {
			out = append(out, st)
		}
	}
	return out
}

func lang(st probeStream) string {
	if l := strings.TrimSpace(st.Tags["language"]); l != "" {
		return l
	}
	return "und"
}

// sameDisposition compares the Matroska dispositions, a missing flag being 0.
func sameDisposition(a, b map[string]int) bool {
	for key, v := range a {
		if b[key] != v {
			return false
		}
	}
	for key, v := range b {
		if a[key] != v {
			return false
		}
	}
	return true
}

// compareTracks holds the candidate to the source stream by stream. Kept audio
// and subtitles are the plan's (the selection's streams, or all of them when
// the plan carries none); the n-th kept source stream of a kind is the n-th
// stream of that kind in the candidate.
func (k *kase) compareTracks(src, cand string, plan *transcode.Plan, orig, got probeInfo, keptAudio, keptSubs []probeStream) {
	k.T.Helper()
	gotAudio, gotSubs := ofKind(got, "audio"), ofKind(got, "subtitle")
	k.require(len(gotAudio) == len(keptAudio), "candidate has %d audio streams, the plan keeps %d", len(gotAudio), len(keptAudio))
	k.require(len(gotSubs) == len(keptSubs), "candidate has %d subtitle streams, the plan keeps %d", len(gotSubs), len(keptSubs))

	// Audio: language always; the title and dispositions of tracks that were
	// copied (the same codec in the candidate as in the source). A converted
	// track's title is the worker's rename of a title that names the source
	// format ("Japanese 5.1 FLAC" becomes "Japanese AAC 5.1"), and the source
	// title otherwise.
	for i, a := range keptAudio {
		b := gotAudio[i]
		k.require(lang(a) == lang(b), "audio %d (source stream %d) language %q became %q", i, a.Index, lang(a), lang(b))
		if a.CodecName == b.CodecName {
			k.require(a.Tags["title"] == b.Tags["title"], "copied audio %d (source stream %d) title %q became %q", i, a.Index, a.Tags["title"], b.Tags["title"])
			k.require(sameDisposition(a.Disposition, b.Disposition), "copied audio %d (source stream %d) dispositions %v became %v", i, a.Index, a.Disposition, b.Disposition)
			continue
		}
		want := transcodeworker.ConvertedAudioTitle(a.Tags["title"], a.ChannelLayout, a.Channels)
		k.require(b.Tags["title"] == want, "converted audio %d (source stream %d) title %q became %q, the conversion names it %q", i, a.Index, a.Tags["title"], b.Tags["title"], want)
	}

	// Subtitles: a copied track (the plan's action says copy) keeps language,
	// title, dispositions and every packet.
	ops := map[int]string{}
	for _, act := range plan.SubtitleActions {
		ops[act.SourceStreamIndex] = strings.ToLower(strings.TrimSpace(act.Operation))
	}
	for i, a := range keptSubs {
		b := gotSubs[i]
		k.require(lang(a) == lang(b), "subtitle %d (source stream %d) language %q became %q", i, a.Index, lang(a), lang(b))
		k.require(a.Tags["title"] == b.Tags["title"], "subtitle %d (source stream %d) title %q became %q", i, a.Index, a.Tags["title"], b.Tags["title"])
		if ops[a.Index] != "copy" {
			continue
		}
		k.require(sameDisposition(a.Disposition, b.Disposition), "copied subtitle %d (source stream %d) dispositions %v became %v", i, a.Index, a.Disposition, b.Disposition)
		want, have := k.s.streamPackets(k.T, src, a.Index), k.s.streamPackets(k.T, cand, b.Index)
		k.require(want == have, "copied subtitle %d (source stream %d) has %d packets, the source %d", i, a.Index, have, want)
	}

	// Attachments and chapters are always kept: the same files, the same count.
	srcAtt, gotAtt := attachmentSet(orig), attachmentSet(got)
	k.require(srcAtt == gotAtt, "attachments differ:\n source    %s\n candidate %s", srcAtt, gotAtt)
	sc, gc := k.s.chapters(k.T, src), k.s.chapters(k.T, cand)
	k.require(sc == gc, "candidate has %d chapters, the source %d", gc, sc)

	// Video: every picture the source decodes to is encoded, so the candidate
	// has the source's packets less those its first picture group shows no
	// picture for (an open group's leading pictures in a clip cut with a
	// stream copy). No other tolerance.
	sv, gv := k.s.videoFrames(k.T, src), k.s.videoFrames(k.T, cand)
	dropped := k.s.headDropped(k.T, src)
	k.require(gv == sv-dropped, "candidate video has %d packets, the source %d of which %d decode to no picture", gv, sv, dropped)
}

// attachmentSet is the attachments as a sorted multiset of (filename,
// mimetype, size), printed.
func attachmentSet(p probeInfo) string {
	var keys []string
	for _, st := range ofKind(p, "attachment") {
		keys = append(keys, fmt.Sprintf("%s|%s|%d", st.Tags["filename"], strings.ToLower(st.Tags["mimetype"]), st.ExtradataSize))
	}
	sort.Strings(keys)
	return strings.Join(keys, "; ")
}

// streamPackets counts one stream's packets by its absolute index.
func (s *suite) streamPackets(t *testing.T, path string, index int) int {
	t.Helper()
	out := s.run(t, s.ffprobe, "-v", "error", "-count_packets", "-select_streams", strconv.Itoa(index), "-show_entries", "stream=nb_read_packets", "-of", "csv=p=0", path)
	if strings.TrimSpace(out) == "N/A" {
		// ffprobe's count for a stream with no packets: a clip's signs
		// track can have no line in the stretch it was cut from.
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		t.Fatalf("counting packets of stream %d of %s: %q", index, filepath.Base(path), out)
	}
	return n
}

// chapters counts a file's chapters.
func (s *suite) chapters(t *testing.T, path string) int {
	t.Helper()
	out := s.run(t, s.ffprobe, "-v", "error", "-show_chapters", "-of", "json", path)
	var v struct {
		Chapters []json.RawMessage `json:"chapters"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("parsing chapters of %s: %v", filepath.Base(path), err)
	}
	return len(v.Chapters)
}
