package maintenanceui

import "net/http"

// keepOriginal is the owner's "Keep original" for a finished encode that is
// waiting to replace its original: the smaller copy is dropped, the original is
// not touched and the file leaves the ready-to-replace list. It repeats
// harmlessly. A file in a batch, or one whose replacement has started, is
// refused with the reason.
func (s *Server) keepOriginal(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID string `json:"id"`
	}
	if decode(w, r, &body) != nil || body.ID == "" {
		fail(w, 400, "provide a job ID")
		return
	}
	if !s.engine.AllowDestructive() {
		fail(w, 403, "removing files is not enabled on this server")
		return
	}
	if _, err := s.engine.KeepOriginal(r.Context(), body.ID); err != nil {
		fail(w, 409, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"id": body.ID})
}
