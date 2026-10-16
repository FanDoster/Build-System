package api

import "net/http"

// handleTelegramStatus exposes only the readiness of the dedicated group route.
// The callback owns verification; keys, paths and recipients never enter JSON.
func (s *Server) handleTelegramStatus(w http.ResponseWriter, r *http.Request) {
	if !requireOperator(w, r) {
		return
	}
	configured := s.TelegramConfigured != nil && s.TelegramConfigured()
	writeJSON(w, http.StatusOK, map[string]bool{"configured": configured})
}
