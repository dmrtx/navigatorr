package maintenanceui

import (
	"context"
	"net/http"
	"strings"

	"github.com/jakenesler/navigatorr/action"
	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
)

// trackView is one audio or subtitle stream of a file, as the picker lists it.
// Keep is what the automatic selection does with it for the languages asked
// about, so a job submitted without picking by hand keeps exactly the checked
// tracks.
type trackView struct {
	Index         int    `json:"index"`
	Language      string `json:"language"`
	Title         string `json:"title,omitempty"`
	Codec         string `json:"codec"`
	Channels      int    `json:"channels,omitempty"`
	ChannelLayout string `json:"channel_layout,omitempty"`
	Default       bool   `json:"default,omitempty"`
	Forced        bool   `json:"forced,omitempty"`
	// Original marks the audio track the file was made in; it is always kept.
	Original bool `json:"original,omitempty"`
	Keep     bool `json:"keep"`
}

type sourceTracksView struct {
	// Languages is the list the selection followed: the one asked for, else the
	// saved setting. Empty keeps every track.
	Languages []string    `json:"languages"`
	Audio     []trackView `json:"audio"`
	Subtitles []trackView `json:"subtitles"`
}

// sourceTracks lists a video's audio and subtitle streams with what the size
// and compact goals would keep of them. keep_languages (a comma separated list,
// possibly empty) stands in for the saved setting, as it does on a job. The
// path is read the way source-profile reads it, from the same allow-list;
// nothing is queued or written, and no path is returned.
func (s *Server) sourceTracks(w http.ResponseWriter, r *http.Request) {
	languages := s.engine.KeepLanguages()
	if q := r.URL.Query(); q.Has("keep_languages") {
		var codes []string
		for _, code := range strings.Split(q.Get("keep_languages"), ",") {
			if code = strings.TrimSpace(code); code != "" {
				codes = append(codes, code)
			}
		}
		list, err := transcode.LanguageList(codes)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "code": "invalid_params", "field": "keep_languages"})
			return
		}
		languages = list
	}
	path, release, ok := s.openSource(w, r)
	if !ok {
		return
	}
	defer release()
	// The profile's budget: a volume that was asleep for the profile read is
	// awake now, but a file read on its own may have to wake it.
	ctx, cancel := context.WithTimeout(r.Context(), sourceProfileTimeout)
	defer cancel()
	rep, ok := s.probeSource(ctx, w, path, func(rep *mediainspect.DetailedReport) bool { return len(rep.MainVideo()) > 0 })
	if !ok {
		return
	}
	// The job's own rule, over the tracks the job reads the same way, so the
	// boxes ticked here are what an untouched submit keeps. Nothing to remove
	// (or no languages) is no selection: every track stays.
	infos := action.TrackInfosOf(rep)
	selection, err := transcode.SelectTracks(&transcode.TrackPolicy{Languages: languages}, infos)
	if err != nil {
		fail(w, 500, "could not choose the tracks")
		return
	}
	original, hasOriginal := transcode.OriginalAudioTrack(infos)
	list := func(kind string, streams []mediainspect.DetailedStream) []trackView {
		out := make([]trackView, 0, len(streams))
		for _, st := range streams {
			info := transcode.TrackInfo{Language: st.Language}
			out = append(out, trackView{
				Index: st.Index, Language: info.LanguageLabel(), Title: st.Title, Codec: st.Codec,
				Channels: st.Channels, ChannelLayout: st.ChannelLayout,
				Default: st.Disposition["default"] > 0, Forced: st.Disposition["forced"] > 0,
				Original: hasOriginal && kind == "audio" && st.Index == original.Index,
				Keep:     selection.Keeps(kind, st.Index),
			})
		}
		return out
	}
	writeJSON(w, 200, sourceTracksView{Languages: languages, Audio: list("audio", rep.Audio), Subtitles: list("subtitle", rep.Subtitles)})
}
