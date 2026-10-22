package maintenanceui

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jakenesler/navigatorr/mediainspect"
)

// sourceProfileTimeout bounds one profile read, ffprobe and the decoded-frame
// check for HDR included, so the goal form never waits on a slow disk.
const sourceProfileTimeout = 6 * time.Second

// Two reads at a time keep a user clicking through files from queueing probes
// on a slow volume; a third request is refused and the form shows no profile.
var sourceProfileSlots = make(chan struct{}, 2)

type sourceProfile struct {
	Width             int    `json:"width,omitempty"`
	Height            int    `json:"height,omitempty"`
	Resolution        string `json:"resolution"`
	DynamicRange      string `json:"dynamic_range"`
	DynamicRangeClass string `json:"dynamic_range_class"`
	VideoCodec        string `json:"video_codec,omitempty"`
	BitDepth          int    `json:"bit_depth,omitempty"`
	SizeBytes         int64  `json:"size_bytes"`
}

// openSource resolves the requested path against the read allow-list the file
// browser uses and takes one of the two read slots. Nothing outside the
// allow-list is touched, and only a regular video file is accepted. The caller
// releases the slot when it is done.
func (s *Server) openSource(w http.ResponseWriter, r *http.Request) (path string, release func(), ok bool) {
	resolver := s.engine.Deps().Fs
	if resolver == nil {
		fail(w, 400, "filesystem navigation is unavailable")
		return "", nil, false
	}
	path, err := resolver.ResolveRead(r.URL.Query().Get("path"))
	if err != nil {
		fail(w, 400, err.Error())
		return "", nil, false
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || !videoPath(path) {
		fail(w, 400, "choose a video file")
		return "", nil, false
	}
	select {
	case sourceProfileSlots <- struct{}{}:
		return path, func() { <-sourceProfileSlots }, true
	default:
		fail(w, 429, "busy reading another file; try again")
		return "", nil, false
	}
}

// probeSource reads the file's streams the way a transcode job will. A file
// that cannot be read is a handled status, never a gateway error: the console
// takes 502, 503 and 504 for an unreachable server.
func (s *Server) probeSource(ctx context.Context, w http.ResponseWriter, path string, valid func(*mediainspect.DetailedReport) bool) (*mediainspect.DetailedReport, bool) {
	probe := s.probe
	if probe == nil {
		probe = mediainspect.InspectDetailed
	}
	rep, err := probe(ctx, s.engine.Deps().Ffprobe, path)
	if err != nil || !rep.Probed || !valid(&rep) {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			fail(w, 408, "reading this file took too long")
			return nil, false
		}
		fail(w, 422, "could not read this file")
		return nil, false
	}
	return &rep, true
}

// sourceProfile reads one video the way a transcode job will, and reports for
// each goal whether the job's own source gates would skip it. Nothing is
// queued or written.
func (s *Server) sourceProfile(w http.ResponseWriter, r *http.Request) {
	path, release, ok := s.openSource(w, r)
	if !ok {
		return
	}
	defer release()
	ctx, cancel := context.WithTimeout(r.Context(), sourceProfileTimeout)
	defer cancel()
	rep, ok := s.probeSource(ctx, w, path, func(rep *mediainspect.DetailedReport) bool { return len(rep.MainVideo()) == 1 })
	if !ok {
		return
	}
	goals := s.engine.SourceGoals(ctx, path, rep, s.classify)
	writeJSON(w, 200, map[string]any{"profile": describeSource(rep, goals.Dynamic), "goals": goals.Goals, "recommended": goals.Recommended})
}

func describeSource(rep *mediainspect.DetailedReport, classified *mediainspect.DynamicRange) sourceProfile {
	v := rep.MainVideo()[0]
	// Without decoded-frame evidence an HDR stream is only known to be HDR.
	d := mediainspect.ClassifyDynamicRange(rep, nil)
	if classified != nil {
		d = *classified
	}
	return sourceProfile{
		Width: v.Width, Height: v.Height, Resolution: resolutionClass(v.Width, v.Height),
		DynamicRange: dynamicRangeName(d), DynamicRangeClass: d.Class,
		VideoCodec: codecName(v.Codec), BitDepth: v.BitDepth, SizeBytes: rep.SizeBytes,
	}
}

func resolutionClass(width, height int) string {
	switch {
	case width >= 3000 || height >= 2000:
		return "4K"
	case width >= 2400 || height >= 1300:
		return "1440p"
	case width >= 1800 || height >= 1000:
		return "1080p"
	case width >= 1200 || height >= 700:
		return "720p"
	case width > 0 || height > 0:
		return "SD"
	}
	return ""
}

// dynamicRangeName is the short name a person recognises, without the
// decoder-level detail of DynamicRange.Label.
func dynamicRangeName(d mediainspect.DynamicRange) string {
	switch d.Class {
	case mediainspect.DynamicRangeSDR:
		return "SDR"
	case mediainspect.DynamicRangeHDR10:
		return "HDR10"
	case mediainspect.DynamicRangeHDR10Plus:
		return "HDR10+"
	case mediainspect.DynamicRangeHLG:
		return "HLG"
	case mediainspect.DynamicRangeDolbyVision:
		return "Dolby Vision"
	}
	return "HDR"
}

func codecName(codec string) string {
	switch c := strings.ToLower(codec); c {
	case "hevc", "h265":
		return "HEVC"
	case "h264", "avc":
		return "H.264"
	case "mpeg2video":
		return "MPEG-2"
	case "vc1":
		return "VC-1"
	default:
		return strings.ToUpper(c)
	}
}
