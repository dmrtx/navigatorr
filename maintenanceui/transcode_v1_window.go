package maintenanceui

import (
	"errors"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/jakenesler/navigatorr/action"
	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcodeapi"
)

const hintWindows = "GET " + transcodeapi.BasePath + "/windows lists the windows"

// maxWindowMarginMinutes bounds the margin an operator may ask for: a day.
const maxWindowMarginMinutes = 1440

// windowView is the API form of a node's window.
type windowView struct {
	Worker        string     `json:"worker"`
	Until         string     `json:"until,omitempty"`
	MarginMinutes int        `json:"margin_minutes"`
	MediaTypes    []string   `json:"media_types"`
	ClosesAt      *time.Time `json:"closes_at,omitempty"`
	State         string     `json:"state"`
	Revision      int64      `json:"revision"`
	UpdatedAt     string     `json:"updated_at,omitempty"`
}

func windowViewOf(st action.NodeWindowStatus) windowView {
	v := windowView{
		Worker: st.Worker, Until: st.Until, MarginMinutes: st.MarginSec / 60,
		MediaTypes: st.MediaTypes, ClosesAt: st.ClosesAt, State: "open",
		Revision: st.Revision, UpdatedAt: st.UpdatedAt,
	}
	if v.MediaTypes == nil {
		v.MediaTypes = []string{}
	}
	if st.Closed {
		v.State = "closed"
	}
	return v
}

// windowFail answers an engine error of a window call.
func windowFail(w http.ResponseWriter, err error) {
	var stale *store.StaleWorkerWindowError
	var unknown *transcode.WorkerNotConfiguredError
	switch {
	case errors.As(err, &stale):
		transcodeapi.WriteError(w, 409, "stale_window_revision", err.Error(), "GET "+transcodeapi.BasePath+"/workers/{name}/window gives the current revision")
	case errors.As(err, &unknown):
		transcodeapi.WriteError(w, 404, "unknown_worker", "no such worker", hintWorkers)
	default:
		log.Printf("transcode window: %v", err)
		transcodeapi.WriteError(w, 500, "internal", "the request could not be completed", "")
	}
}

func (s *Server) transcodeWindowReadHandlers(h transcodeapi.Handlers) {
	const b = transcodeapi.BasePath
	h["GET "+b+"/windows"] = func(w http.ResponseWriter, r *http.Request) {
		all, err := s.engine.NodeWindows()
		if err != nil {
			windowFail(w, err)
			return
		}
		out := make([]windowView, 0, len(all))
		for _, st := range all {
			out = append(out, windowViewOf(st))
		}
		transcodeapi.WriteJSON(w, 200, map[string]any{"windows": out})
	}
	h["GET "+b+"/workers/{name}/window"] = func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if !s.knownWorker(w, name) {
			return
		}
		st, ok, err := s.engine.NodeWindow(name)
		if err != nil {
			windowFail(w, err)
			return
		}
		if !ok {
			transcodeapi.WriteError(w, 404, "no_window", "this worker has no availability window", hintWindows)
			return
		}
		transcodeapi.WriteJSON(w, 200, windowViewOf(st))
	}
}

func (s *Server) transcodeWindowWriteHandlers(h transcodeapi.Handlers) {
	const b = transcodeapi.BasePath
	h["PUT "+b+"/workers/{name}/window"] = func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if !s.knownWorker(w, name) {
			return
		}
		var body struct {
			Until         string   `json:"until"`
			MarginMinutes *int     `json:"margin_minutes"`
			MediaTypes    []string `json:"media_types"`
			Revision      *int64   `json:"revision"`
		}
		if !v1Body(w, r, &body, true) {
			return
		}
		win := store.WorkerWindow{Worker: name, Until: body.Until}
		if body.MarginMinutes != nil {
			if *body.MarginMinutes < 0 || *body.MarginMinutes > maxWindowMarginMinutes {
				v1Bad(w, "margin_minutes must be between 0 and 1440", "")
				return
			}
			win.MarginSec = *body.MarginMinutes * 60
		}
		if body.Until != "" {
			if _, err := time.Parse(time.RFC3339, body.Until); err != nil {
				v1Bad(w, "until must be an RFC3339 time such as 2026-11-21T06:00:00Z", "")
				return
			}
		}
		types, err := store.NormalizeWorkerWindowMediaTypes(body.MediaTypes)
		if err != nil {
			v1Bad(w, "media_types: "+err.Error(), "")
			return
		}
		win.MediaTypes = types
		if body.Revision != nil && *body.Revision < 0 {
			v1Bad(w, "revision must not be negative", "")
			return
		}
		saved, err := s.engine.SetNodeWindow(win, body.Revision)
		if err != nil {
			windowFail(w, err)
			return
		}
		st, ok, err := s.engine.NodeWindow(name)
		if err != nil || !ok {
			// stored a moment ago: answer with what was written
			st = action.NodeWindowStatus{WorkerWindow: saved}
		}
		transcodeapi.WriteJSON(w, 200, windowViewOf(st))
	}
	h["DELETE "+b+"/workers/{name}/window"] = func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if !s.knownWorker(w, name) {
			return
		}
		if !v1Body(w, r, &noBody{}, false) {
			return
		}
		var expected *int64
		if q := r.URL.Query().Get("revision"); q != "" {
			n, err := strconv.ParseInt(q, 10, 64)
			if err != nil || n < 0 {
				v1Bad(w, "revision must be a non-negative integer", "")
				return
			}
			expected = &n
		}
		had, err := s.engine.DeleteNodeWindow(name, expected)
		if err != nil {
			windowFail(w, err)
			return
		}
		transcodeapi.WriteJSON(w, 200, map[string]any{"worker": name, "had_window": had})
	}
}
