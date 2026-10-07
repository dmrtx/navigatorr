package maintenanceui

import (
	"net/http"
	"strings"
)

func (s *Server) admitCommand(w http.ResponseWriter, r *http.Request, inputs map[string]any, key string) {
	if !reconfigureKey.MatchString(key) {
		fail(w, 400, "a valid submission key is required")
		return
	}
	id, _ := inputs["id"].(string)
	inst, err := s.engine.Deps().Store.GetActionInstanceIfExists(id)
	if err != nil || inst == nil || !allowedActions[inst.ActionName] {
		fail(w, 403, "job is unavailable")
		return
	}
	result, err := s.engine.QueueMaintenanceCommand(r.Context(), inputs, key)
	if err != nil {
		fail(w, 409, err.Error())
		return
	}
	writeJSON(w, 202, map[string]string{"id": id, "command_id": result.ID, "status": result.Status})
}

func (s *Server) commandStatus(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	inst, err := s.engine.Deps().Store.GetActionInstanceIfExists(id)
	if err != nil || inst == nil || inst.ActionName != "maintenance_command" {
		fail(w, 404, "command not found")
		return
	}
	result, err := s.engine.Status(r.Context(), id)
	if err != nil {
		fail(w, 500, "read command")
		return
	}
	writeJSON(w, 200, map[string]any{"command_id": id, "id": decodeOperationJSON(inst.InputsJSON)["id"], "status": result.Status, "error": result.Error, "kind": decodeOperationJSON(inst.InputsJSON)["kind"], "waiting_reason": result.WaitingReason})
}
