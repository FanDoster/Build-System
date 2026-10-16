package api

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/FanDoster/Build-System/internal/models"
)

func (s *Server) handleAgentNotifications(w http.ResponseWriter, r *http.Request) {
	if !requireOperator(w, r) {
		return
	}
	name := r.PathValue("name")
	if !models.ValidAgentName(name) {
		writeError(w, 400, "invalid agent name")
		return
	}
	var prefs models.AgentNotificationPreferences
	var err error
	if r.Method == http.MethodPut {
		if !requireCsrf(w, r) {
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		decoder := json.NewDecoder(r.Body)
		var raw map[string]json.RawMessage
		if err := decoder.Decode(&raw); err != nil || raw == nil {
			writeError(w, 400, "expected a preference object")
			return
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			writeError(w, 400, "unexpected trailing JSON")
			return
		}
		patch := map[string]bool{}
		for key, value := range raw {
			switch key {
			case "enabled", "availability", "health", "builds":
			default:
				writeError(w, 400, "unknown notification preference")
				return
			}
			var b bool
			if string(value) == "null" || json.Unmarshal(value, &b) != nil {
				writeError(w, 400, "notification preferences must be booleans")
				return
			}
			patch[key] = b
		}
		prefs, err = s.DB.UpdateAgentNotificationPreferences(name, patch)
	} else {
		prefs, err = s.DB.AgentNotificationPreferences(name)
	}
	if err != nil {
		writeError(w, 500, "could not save or read notification preferences")
		return
	}
	writeJSON(w, 200, map[string]any{"agent": name, "telegram_notifications": prefs})
}
