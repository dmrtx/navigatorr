package transcodeworker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
)

// realTrackFixture is a synthetic multi-language Matroska file: programme
// video, five audio tracks (the French one the default), three SRT subtitles
// (the Spanish one forced), a font attachment and two chapters.
type realTrackFixture struct {
	path    string
	probe   SourceProbe
	ffmpeg  string
	ffprobe string
}

type realAudioTrack struct {
	lang, codec, layout, kbps string
	channels                  int
	def                       bool
}

var realAudioTracks = []realAudioTrack{
	{"eng", "eac3", "5.1", "640k", 6, false},  // stream 1: converted at level 1
	{"fre", "aac", "stereo", "96k", 2, true},  // stream 2: the default, copied
	{"spa", "aac", "stereo", "96k", 2, false}, // stream 3: copied
	{"jpn", "eac3", "5.1", "640k", 6, false},  // stream 4: converted
	{"ger", "ac3", "5.1", "448k", 6, false},   // stream 5
}

var realSubtitles = []struct{ lang, text string }{{"eng", "Hello"}, {"fre", "Bonjour"}, {"spa", "Hola"}}

func buildRealTrackFixture(t *testing.T, ctx context.Context, dir string) *realTrackFixture {
	t.Helper()
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe unavailable")
	}
	available, err := ProbeAvailableEncoders(ctx, ffmpeg)
	if err != nil || !available["libx265"] || !available["libx264"] || !available["aac"] || !available["aac_at"] || !available["eac3"] || !available["ac3"] {
		t.Skip("libx265/libx264/AAC/AudioToolbox AAC/AC-3/E-AC-3 unavailable")
	}
	const seconds = 14
	args := []string{"-nostdin", "-v", "error", "-f", "lavfi", "-i", fmt.Sprintf("testsrc2=size=320x180:rate=24:duration=%d", seconds)}
	for _, tr := range realAudioTracks {
		args = append(args, "-f", "lavfi", "-i", layoutNoise(tr.channels, seconds))
	}
	base := 1 + len(realAudioTracks)
	for i, s := range realSubtitles {
		srt := filepath.Join(dir, fmt.Sprintf("sub%d.srt", i))
		body := fmt.Sprintf("1\n00:00:01,000 --> 00:00:03,000\n%s\n\n2\n00:00:05,000 --> 00:00:07,000\n%s\n", s.text, s.text)
		if err := os.WriteFile(srt, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		args = append(args, "-i", srt)
	}
	chapters := filepath.Join(dir, "chapters.txt")
	if err := os.WriteFile(chapters, []byte(";FFMETADATA1\n[CHAPTER]\nTIMEBASE=1/1000\nSTART=0\nEND=7000\ntitle=One\n[CHAPTER]\nTIMEBASE=1/1000\nSTART=7000\nEND=14000\ntitle=Two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	args = append(args, "-f", "ffmetadata", "-i", chapters)
	font := filepath.Join(dir, "synthetic.ttf")
	if err := os.WriteFile(font, []byte(strings.Repeat("not a real font ", 64)), 0o600); err != nil {
		t.Fatal(err)
	}
	args = append(args, "-map", "0:v")
	for i := range realAudioTracks {
		args = append(args, "-map", fmt.Sprintf("%d:a", i+1))
	}
	for i := range realSubtitles {
		args = append(args, "-map", fmt.Sprintf("%d:s", base+i))
	}
	args = append(args, "-map_chapters", fmt.Sprint(base+len(realSubtitles)), "-c:v", "libx264", "-preset", "ultrafast", "-crf", "18", "-pix_fmt", "yuv420p")
	for i, tr := range realAudioTracks {
		args = append(args, fmt.Sprintf("-c:a:%d", i), tr.codec, fmt.Sprintf("-filter:a:%d", i), "aformat=channel_layouts="+tr.layout, fmt.Sprintf("-b:a:%d", i), tr.kbps, fmt.Sprintf("-metadata:s:a:%d", i), "language="+tr.lang)
		if tr.def {
			args = append(args, fmt.Sprintf("-disposition:a:%d", i), "default")
		} else {
			args = append(args, fmt.Sprintf("-disposition:a:%d", i), "0")
		}
	}
	args = append(args, "-c:s", "srt")
	for i, s := range realSubtitles {
		disposition := "0"
		if s.lang == "spa" {
			disposition = "forced"
		}
		args = append(args, fmt.Sprintf("-metadata:s:s:%d", i), "language="+s.lang, fmt.Sprintf("-disposition:s:%d", i), disposition)
	}
	raw := filepath.Join(dir, "raw.mkv")
	args = append(args, "-attach", font, "-metadata:s:t:0", "mimetype=application/x-truetype-font", "-metadata:s:t:0", "filename=synthetic.ttf", raw)
	if out, err := exec.CommandContext(ctx, ffmpeg, args...).CombinedOutput(); err != nil {
		t.Fatalf("source fixture: %v %s", err, out)
	}
	// Tag the per-stream byte counts the way a real rip is tagged, so each
	// track states a bitrate.
	packets, err := exec.CommandContext(ctx, ffprobe, "-v", "error", "-show_entries", "packet=stream_index,size", "-of", "csv=p=0", raw).Output()
	if err != nil {
		t.Fatal(err)
	}
	bytesBy := map[int]int64{}
	for _, line := range strings.Split(strings.TrimSpace(string(packets)), "\n") {
		var idx int
		var n int64
		if _, err := fmt.Sscanf(strings.TrimSpace(line), "%d,%d", &idx, &n); err == nil {
			bytesBy[idx] += n
		}
	}
	source := filepath.Join(dir, "source.mkv")
	tagArgs := []string{"-nostdin", "-v", "error", "-y", "-i", raw, "-map", "0", "-c", "copy"}
	for i := range realAudioTracks {
		tagArgs = append(tagArgs, fmt.Sprintf("-metadata:s:a:%d", i), fmt.Sprintf("NUMBER_OF_BYTES=%d", bytesBy[i+1]))
	}
	tagArgs = append(tagArgs, source)
	if out, err := exec.CommandContext(ctx, ffmpeg, tagArgs...).CombinedOutput(); err != nil {
		t.Fatalf("tagging the fixture: %v %s", err, out)
	}
	probe, err := ProbeSourceDetails(ctx, ffprobe, source)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(streamsOfKind(probe.Streams, "audio")); got != len(realAudioTracks) {
		t.Fatalf("%d audio tracks in the fixture, want %d", got, len(realAudioTracks))
	}
	if got := len(streamsOfKind(probe.Streams, "subtitle")); got != len(realSubtitles) {
		t.Fatalf("%d subtitles in the fixture, want %d", got, len(realSubtitles))
	}
	for _, s := range streamsOfKind(probe.Streams, "audio") {
		if s.BitrateKbps <= 0 {
			t.Fatalf("the probe states no bitrate for stream %d", s.Index)
		}
	}
	return &realTrackFixture{path: source, probe: probe, ffmpeg: ffmpeg, ffprobe: ffprobe}
}

// countStreams reads what the file holds by codec type, and its chapters.
func (f *realTrackFixture) counts(t *testing.T, ctx context.Context, path string) (attachments, chapters int) {
	t.Helper()
	out, err := exec.CommandContext(ctx, f.ffprobe, "-v", "error", "-show_entries", "stream=codec_type", "-show_chapters", "-of", "json", path).Output()
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Streams []struct {
			CodecType string `json:"codec_type"`
		} `json:"streams"`
		Chapters []json.RawMessage `json:"chapters"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	for _, s := range doc.Streams {
		if s.CodecType == "attachment" {
			attachments++
		}
	}
	chapters = len(doc.Chapters)
	return attachments, chapters
}

// trackSelectionPlan is the size goal's level 1 plan with the selection of
// the languages applied the way the server applies it.
func trackSelectionPlan(t *testing.T, probe SourceProbe, languages []string, audio, subtitles *[]int) *transcode.Plan {
	t.Helper()
	plan := sizeV5TestPlan(transcode.AudioModeSizeAAC)
	plan.Preset = "ultrafast"
	subs := streamsOfKind(probe.Streams, "subtitle")
	for _, s := range subs {
		plan.SubtitleActions = append(plan.SubtitleActions, transcode.SubtitleAction{TypeIndex: s.TypeIndex, SourceStreamIndex: s.Index, SourceCodec: s.Codec, Operation: "copy", Codec: "copy"})
	}
	policy := &transcode.TrackPolicy{Languages: languages, AudioStreams: audio, SubtitleStreams: subtitles}
	sel, err := transcode.SelectTracks(policy, trackInfos(probe.Streams))
	if err != nil {
		t.Fatal(err)
	}
	if sel != nil {
		plan.Tracks, plan.ModePolicyVersion = sel, transcode.ModePolicyV6
		var kept []transcode.SubtitleAction
		for _, a := range plan.SubtitleActions {
			if sel.Keeps("subtitle", a.SourceStreamIndex) {
				kept = append(kept, a)
			}
		}
		plan.SubtitleActions = kept
	}
	windows := transcode.PlanFinalValidationWindows(probe.DurationSec, 1, 2.4, 5, 12)
	plan.QualityValidation = &transcode.QualityValidationPlan{Metric: "ssim", Samples: windows, Quality: transcode.BenchmarkQualityConfig{FinalValidation: &transcode.BenchmarkFinalValidationConfig{Mode: "sampled"}}, BenchmarkRequestDigest: "sha256:" + strings.Repeat("2", 64)}
	plan.PlanDigest, _ = transcode.DigestPlan(plan)
	return plan
}

// encode runs a plan through the worker's own path to a candidate file.
func (f *realTrackFixture) encode(t *testing.T, ctx context.Context, dir, name string, plan *transcode.Plan) (*ExecutionPlan, string) {
	t.Helper()
	if err := ValidatePlan(plan); err != nil {
		t.Fatalf("%s: plan rejected: %v", name, err)
	}
	ep, err := BuildExecutionPlan(plan, f.probe.Streams, f.probe.DurationSec)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	jobDir := filepath.Join(dir, name)
	if err := os.MkdirAll(jobDir, 0o700); err != nil {
		t.Fatal(err)
	}
	candidate := filepath.Join(jobDir, "candidate.mkv")
	if err := RunFFmpegPaths(ctx, f.ffmpeg, ep, f.path, candidate, filepath.Join(jobDir, "progress"), filepath.Join(jobDir, "ffmpeg.log")); err != nil {
		log, _ := os.ReadFile(filepath.Join(jobDir, "ffmpeg.log"))
		t.Fatalf("%s encode: %v\n%s", name, err, log)
	}
	return ep, candidate
}

// A synthetic multi-language file goes through the whole worker path with a
// track selection: probe, plan, ffmpeg, probe and the pre-publish validation.
// The output must have exactly the planned tracks, in order, with the font and
// the chapters and the default moved to the first kept audio track; an output
// with other tracks must not validate. Needs AudioToolbox, so it runs on macOS
// and skips where aac_at is absent.
func TestTrackSelectionRealEncode(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Second)
	defer cancel()
	dir := t.TempDir()
	f := buildRealTrackFixture(t, ctx, dir)
	w := NewWorker(&WorkerConfig{StateDir: dir, FFmpeg: f.ffmpeg, FFprobe: f.ffprobe})
	srcAttachments, srcChapters := f.counts(t, ctx, f.path)
	if srcAttachments != 1 || srcChapters != 2 {
		t.Fatalf("the fixture has %d attachments and %d chapters", srcAttachments, srcChapters)
	}

	// spa, eng and jpn: audio eng (also the original, the first), spa and jpn;
	// subtitles eng and spa.
	plan := trackSelectionPlan(t, f.probe, []string{"eng", "jpn", "spa"}, nil, nil)
	if plan.Tracks == nil || len(plan.Tracks.KeepAudio) != 3 || len(plan.Tracks.KeepSubtitles) != 2 {
		t.Fatalf("selection %+v", plan.Tracks)
	}
	ep, candidate := f.encode(t, ctx, dir, "selected", plan)
	if _, err := w.validateEncodedCandidateFull(ctx, candidate, ep, f.probe, 0); err != nil {
		t.Fatalf("pre-publish validation of the selected output: %v", err)
	}
	cand, err := ProbeSourceDetails(ctx, f.ffprobe, candidate)
	if err != nil {
		t.Fatal(err)
	}
	audio, subs := streamsOfKind(cand.Streams, "audio"), streamsOfKind(cand.Streams, "subtitle")
	if len(audio) != 3 || len(subs) != 2 {
		t.Fatalf("output has %d audio and %d subtitle tracks", len(audio), len(subs))
	}
	wantLang := []string{"eng", "spa", "jpn"}
	wantChannels := []int{6, 2, 6}
	for i, a := range audio {
		if a.Language != wantLang[i] || a.Channels != wantChannels[i] || a.Codec != "aac" {
			t.Errorf("audio %d: %+v", i, a)
		}
		t.Logf("audio %d: %s %s %dch profile %s default=%d", i, a.Language, a.Codec, a.Channels, a.Profile, a.Disposition["default"])
	}
	if audio[0].Profile != "LC" || audio[2].Profile != "LC" {
		t.Errorf("converted tracks are not AAC-LC: %+v %+v", audio[0], audio[2])
	}
	if audio[0].Disposition["default"] != 1 || audio[1].Disposition["default"] != 0 || audio[2].Disposition["default"] != 0 {
		t.Errorf("the default did not move to the first kept audio track: %v %v %v", audio[0].Disposition, audio[1].Disposition, audio[2].Disposition)
	}
	if subs[0].Language != "eng" || subs[1].Language != "spa" || subs[1].Disposition["forced"] != 1 || subs[0].Disposition["forced"] != 0 {
		t.Errorf("subtitles %+v", subs)
	}
	if att, ch := f.counts(t, ctx, candidate); att != srcAttachments || ch != srcChapters {
		t.Errorf("output has %d attachments and %d chapters, want %d and %d", att, ch, srcAttachments, srcChapters)
	}

	// An output that kept every track is not the planned output.
	allPlan := trackSelectionPlan(t, f.probe, nil, nil, nil)
	if allPlan.Tracks != nil {
		t.Fatal("an empty selection planned tracks")
	}
	_, everything := f.encode(t, ctx, dir, "everything", allPlan)
	if _, err := w.validateEncodedCandidateFull(ctx, everything, ep, f.probe, 0); err == nil {
		t.Error("an output with every track passed validation against the selection")
	} else {
		t.Logf("whole file against the selection: %v", err)
	}

	// So is one with the right number of tracks but another language in them:
	// Japanese and German in place of Spanish and Japanese.
	other := []int{1, 4, 5}
	wrong := trackSelectionPlan(t, f.probe, nil, &other, &[]int{6, 8})
	if wrong.Tracks == nil {
		t.Fatal("manual picks planned no selection")
	}
	_, swapped := f.encode(t, ctx, dir, "swapped", wrong)
	if _, err := w.validateEncodedCandidateFull(ctx, swapped, ep, f.probe, 0); err == nil {
		t.Error("an output with the wrong audio track passed validation against the selection")
	} else {
		t.Logf("wrong audio track against the selection: %v", err)
	}

	// The reverse holds: each of those outputs is valid against its own plan.
	wrongEP, err := BuildExecutionPlan(wrong, f.probe.Streams, f.probe.DurationSec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.validateEncodedCandidateFull(ctx, swapped, wrongEP, f.probe, 0); err != nil {
		t.Errorf("the manual pick does not validate against its own plan: %v", err)
	}
	allEP, err := BuildExecutionPlan(allPlan, f.probe.Streams, f.probe.DurationSec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.validateEncodedCandidateFull(ctx, everything, allEP, f.probe, 0); err != nil {
		t.Errorf("the whole file does not validate against the plan that keeps it: %v", err)
	}
}

// A file with no audio can still lose subtitles. The plan keeps no audio, the
// worker builds and encodes it, and validation accepts exactly the kept
// subtitle; the same plan against a source that has audio is refused.
func TestTrackSelectionRealEncodeWithoutAudio(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Second)
	defer cancel()
	dir := t.TempDir()
	f := buildRealTrackFixture(t, ctx, dir)
	silent := filepath.Join(dir, "silent.mkv")
	if out, err := exec.CommandContext(ctx, f.ffmpeg, "-nostdin", "-v", "error", "-y", "-i", f.path, "-map", "0", "-map", "-0:a", "-c", "copy", silent).CombinedOutput(); err != nil {
		t.Fatalf("remux without audio: %v %s", err, out)
	}
	probe, err := ProbeSourceDetails(ctx, f.ffprobe, silent)
	if err != nil {
		t.Fatal(err)
	}
	if len(streamsOfKind(probe.Streams, "audio")) != 0 || len(streamsOfKind(probe.Streams, "subtitle")) != 3 {
		t.Fatalf("the silent fixture has %d audio and %d subtitle streams", len(streamsOfKind(probe.Streams, "audio")), len(streamsOfKind(probe.Streams, "subtitle")))
	}
	quiet := &realTrackFixture{path: silent, probe: probe, ffmpeg: f.ffmpeg, ffprobe: f.ffprobe}
	w := NewWorker(&WorkerConfig{StateDir: dir, FFmpeg: f.ffmpeg, FFprobe: f.ffprobe})

	plan := trackSelectionPlan(t, probe, []string{"spa"}, nil, nil)
	if plan.Tracks == nil || len(plan.Tracks.KeepAudio) != 0 || len(plan.Tracks.KeepSubtitles) != 1 {
		t.Fatalf("selection %+v", plan.Tracks)
	}
	ep, candidate := quiet.encode(t, ctx, dir, "silent", plan)
	if _, err := w.validateEncodedCandidateFull(ctx, candidate, ep, probe, 0); err != nil {
		t.Fatalf("validation of the silent output: %v", err)
	}
	cand, err := ProbeSourceDetails(ctx, f.ffprobe, candidate)
	if err != nil {
		t.Fatal(err)
	}
	subs := streamsOfKind(cand.Streams, "subtitle")
	if len(streamsOfKind(cand.Streams, "audio")) != 0 || len(subs) != 1 || subs[0].Language != "spa" || subs[0].Disposition["forced"] != 1 {
		t.Fatalf("output has %d audio and subtitles %+v", len(streamsOfKind(cand.Streams, "audio")), subs)
	}
	// The same plan must not build against a source that has audio.
	if _, err := BuildExecutionPlan(plan, f.probe.Streams, f.probe.DurationSec); err == nil {
		t.Error("a plan that keeps no audio was built for a source that has audio")
	}
}
