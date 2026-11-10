package transcodeworker

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/jakenesler/navigatorr/transcode"
)

// This file holds the candidate checks that read packets, not the probe's
// stream list: the programme video's frames, and every kept subtitle's
// events. The probe says a track is there; only its packets say it is whole.

// videoCompleteness holds the candidate's programme video to the source's
// packet scan. The encode passes every source frame through with its own
// timestamp (sourceTimestampArgs), so the candidate has as many pictures as
// the source, ending where the source's end. The container's duration cannot
// tell: it is the longest stream's, so a video that stops early hides behind
// audio that runs to the end.
//
// Only the packets a decoder cannot show may be missing: those before the
// source's first keyframe and those its container marks to be discarded.
// Without them the candidate starts later, so its span may be shorter by as
// many frames; it is never longer than the source's by more than one frame.
func videoCompleteness(src, cand sourceVideoScan) error {
	if src.Packets == 0 {
		// Nothing was scanned (a test that injects the probe only).
		return nil
	}
	if cand.Packets == 0 {
		return candidateInvalid(fmt.Errorf("%w: the converted video has no frames; the source has %d (fail closed, never publish)", ErrSourceInvalid, src.Packets))
	}
	if cand.DemuxErrors > 0 {
		return candidateInvalid(fmt.Errorf("%w: the converted video is damaged: reading it reported %d error(s) (%s) (fail closed, never publish)", ErrSourceInvalid, cand.DemuxErrors, truncateForLog(cand.FirstError, 200)))
	}
	lead := src.LeadingNonKey
	if !src.sawKey && lead == src.Packets {
		// No packet is flagged a keyframe: nothing tells which ones a
		// decoder skips, so none may be missing.
		lead = 0
	}
	least := src.Packets - lead - src.Discarded
	if cand.Packets > src.Packets || cand.Packets < least {
		return candidateInvalid(fmt.Errorf("%w: the converted video has %d frames, the source %d; frames were lost or added in the conversion (fail closed, never publish)", ErrSourceInvalid, cand.Packets, src.Packets))
	}
	if src.Discontinuous || cand.Discontinuous {
		// A timeline that restarts has no length to compare; the frames do.
		return nil
	}
	frame := src.LastFrame
	if frame <= 0 && src.Packets > 0 {
		frame = src.Seconds() / float64(src.Packets)
	}
	// A millisecond of slack absorbs the containers' timestamp rounding.
	const rounding = 0.001
	missing := float64(src.Packets - cand.Packets)
	short := src.Seconds() - cand.Seconds()
	if short > frame*(1+missing)+rounding || -short > frame+rounding {
		return candidateInvalid(fmt.Errorf("%w: the converted video ends %.3fs after its first picture, the source's %.3fs; its last frame is not where the source's is (fail closed, never publish)", ErrSourceInvalid, cand.Seconds(), src.Seconds()))
	}
	return nil
}

// validateVideoCompleteness scans the candidate's programme video and holds
// it to the source's scan, taken before the encode (or now, when the caller
// did not keep one).
func (w *Worker) validateVideoCompleteness(ctx context.Context, srcProbe SourceProbe, candidate string, candVideo *SourceStream) error {
	srcIndex := programmeVideoIndex(srcProbe.Streams)
	if srcIndex < 0 || candVideo == nil {
		return nil
	}
	var src sourceVideoScan
	if srcProbe.videoScan != nil {
		src = *srcProbe.videoScan
	} else if srcProbe.Path != "" {
		var err error
		if src, err = w.scanSourceVideoForJob(ctx, srcProbe.Path, srcIndex); err != nil {
			return fmt.Errorf("source video validation: %w", err)
		}
	}
	if src.Packets == 0 {
		return nil
	}
	cand, err := w.scanSourceVideoForJob(ctx, candidate, candVideo.Index)
	if err != nil {
		return fmt.Errorf("candidate video validation: %w", err)
	}
	return videoCompleteness(src, cand)
}

// subtitlePackets is one subtitle stream's packets: how many, and how many
// hold more than two bytes. A mov_text sample of two bytes is its text length
// alone, zero: an empty sample the MP4 muxer writes to fill the gap between
// two lines, which a conversion drops.
type subtitlePackets struct {
	packets int64
	text    int64
}

// movTextEmptySampleBytes is the size of a mov_text sample with no text.
const movTextEmptySampleBytes = 2

// scanSubtitlePackets reads every subtitle packet of a file without decoding
// it and counts them by stream index.
func scanSubtitlePackets(ctx context.Context, ffprobePath, path string) (map[int]subtitlePackets, error) {
	out := map[int]subtitlePackets{}
	var parseErr error
	stdout := &lineWriter{line: func(line string) {
		if strings.TrimSpace(line) == "" || parseErr != nil {
			return
		}
		index, size := -1, int64(-1)
		for _, f := range strings.Split(line, "|") {
			key, value, _ := strings.Cut(f, "=")
			switch key {
			case "stream_index":
				if n, err := strconv.Atoi(value); err == nil {
					index = n
				}
			case "size":
				if n, err := strconv.ParseInt(value, 10, 64); err == nil {
					size = n
				}
			}
		}
		if index < 0 || size < 0 {
			parseErr = fmt.Errorf("unexpected ffprobe subtitle packet output %q", truncateForLog(line, 120))
			return
		}
		c := out[index]
		c.packets++
		if size > movTextEmptySampleBytes {
			c.text++
		}
		out[index] = c
	}}
	var stderr strings.Builder
	cmd := exec.CommandContext(ctx, ffprobePath, "-v", "error", "-select_streams", "s",
		"-show_entries", "packet=stream_index,size", "-of", "compact=p=0", path)
	cmd.Stdout, cmd.Stderr = stdout, &stderr
	err := runCharged(ctx, cmd)
	stdout.flush()
	if err != nil {
		return nil, fmt.Errorf("ffprobe subtitle packet scan failed: %w (stderr: %s)", err, truncateForLog(strings.TrimSpace(stderr.String()), 300))
	}
	if parseErr != nil {
		return nil, parseErr
	}
	return out, nil
}

// measuresSubtitlePackets reports whether the kept subtitles' packets are
// counted: whenever the output keeps any, unless a test injected the probe
// without the scan.
func (w *Worker) measuresSubtitlePackets(plan *transcode.Plan, src SourceProbe) bool {
	return len(plannedStreams(plan, src.Streams, "subtitle")) > 0 && (w.scanSubtitles != nil || !w.probeInjected())
}

// subtitlePacketsForJob uses the injected subtitle scan when present.
func (w *Worker) subtitlePacketsForJob(ctx context.Context, path string) (map[int]subtitlePackets, error) {
	if w.scanSubtitles != nil {
		return w.scanSubtitles(ctx, path)
	}
	return scanSubtitlePackets(ctx, w.ffprobePath, path)
}

// validateSubtitlePackets counts the candidate's subtitle packets and holds
// each kept track to the source's (see subtitleEvents).
func (w *Worker) validateSubtitlePackets(ctx context.Context, plan *transcode.Plan, srcProbe SourceProbe, candidate string, candProbe SourceProbe) error {
	if !w.measuresSubtitlePackets(plan, srcProbe) {
		return nil
	}
	src := srcProbe.subtitlePackets
	if src == nil {
		if srcProbe.Path == "" {
			return fmt.Errorf("source subtitle validation: the source was not scanned and its path is unknown (fail closed, never publish)")
		}
		var err error
		if src, err = w.subtitlePacketsForJob(ctx, srcProbe.Path); err != nil {
			return fmt.Errorf("source subtitle validation: %w", err)
		}
	}
	cand, err := w.subtitlePacketsForJob(ctx, candidate)
	if err != nil {
		return fmt.Errorf("candidate subtitle validation: %w", err)
	}
	return subtitleEvents(plan, srcProbe.Streams, candProbe.Streams, src, cand)
}

// subtitleEvents holds every kept subtitle to the source track's events. A
// copied track carries every packet unchanged, so the counts are equal. A
// converted track (mov_text to SubRip, the one conversion) has one packet per
// line of text: the source's empty gap samples have no counterpart, so the
// candidate's packets equal the source's samples that hold text. A track whose
// source had packets never comes out with none.
func subtitleEvents(plan *transcode.Plan, src, cand []SourceStream, srcPackets, candPackets map[int]subtitlePackets) error {
	srcSubs, candSubs := plannedStreams(plan, src, "subtitle"), streamsOfKind(cand, "subtitle")
	if len(srcSubs) != len(candSubs) {
		return candidateInvalid(fmt.Errorf("%w: local candidate subtitle count %d != %s %d (fail closed, never publish)", ErrSourceInvalid, len(candSubs), plannedLabel(plan), len(srcSubs)))
	}
	for i := range srcSubs {
		a, b := srcSubs[i], candSubs[i]
		act, ok := subtitleActionFor(plan, a)
		if !ok {
			return candidateInvalid(fmt.Errorf("%w: subtitle stream %d has no planned action (fail closed, never publish)", ErrSourceInvalid, i))
		}
		x, y := srcPackets[a.Index], candPackets[b.Index]
		want := x.packets
		if norm(act.Operation) != "copy" && norm(a.Codec) == "mov_text" {
			want = x.text
		}
		if x.packets > 0 && y.packets == 0 {
			return candidateInvalid(fmt.Errorf("%w: subtitle stream %d came out empty; the source track has %d lines (fail closed, never publish)", ErrSourceInvalid, i, want))
		}
		if y.packets != want {
			what := "copied"
			if norm(act.Operation) != "copy" {
				what = "converted"
			}
			return candidateInvalid(fmt.Errorf("%w: %s subtitle stream %d has %d lines, the source track %d; lines were lost or added (fail closed, never publish)", ErrSourceInvalid, what, i, y.packets, want))
		}
	}
	return nil
}

// subtitleActionFor is the plan's action for a source subtitle. A legacy
// worker-shim plan carries none; its actions are derived as the execution
// plan derived them.
func subtitleActionFor(plan *transcode.Plan, s SourceStream) (transcode.SubtitleAction, bool) {
	for _, a := range plan.SubtitleActions {
		if a.TypeIndex == s.TypeIndex {
			return a, true
		}
	}
	if plan.RecipeVersion == "legacy-worker-shim" {
		return legacyHevcVTSubtitleAction(s)
	}
	return transcode.SubtitleAction{}, false
}

// attachmentKey identifies an attachment by what it carries: its name, its
// type and its size.
type attachmentKey struct {
	filename, mimetype string
	size               int64
}

// attachmentsPreserved holds the candidate's attachments (fonts, mostly) to
// the source's: the same files, by name, type and size, however many of each.
func attachmentsPreserved(src, cand []SourceStream) error {
	count := map[attachmentKey]int{}
	var order []attachmentKey // the first difference is reported, in file order
	for _, s := range streamsOfKind(src, "attachment") {
		k := attachmentKey{s.Filename, norm(s.MimeType), s.AttachmentBytes}
		if count[k] == 0 {
			order = append(order, k)
		}
		count[k]++
	}
	for _, s := range streamsOfKind(cand, "attachment") {
		k := attachmentKey{s.Filename, norm(s.MimeType), s.AttachmentBytes}
		if _, ok := count[k]; !ok {
			order = append(order, k)
		}
		count[k]--
	}
	for _, k := range order {
		n := count[k]
		if n > 0 {
			return candidateInvalid(fmt.Errorf("%w: attachment %q (%s, %d bytes) is missing from the converted file (fail closed, never publish)", ErrSourceInvalid, k.filename, k.mimetype, k.size))
		}
		if n < 0 {
			return candidateInvalid(fmt.Errorf("%w: the converted file has an attachment %q (%s, %d bytes) the source does not (fail closed, never publish)", ErrSourceInvalid, k.filename, k.mimetype, k.size))
		}
	}
	return nil
}
