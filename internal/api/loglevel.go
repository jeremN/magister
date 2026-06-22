package api

import (
	"encoding/json"
	"net/http"

	"concentus/internal/config"
)

// handleGetLogLevel reports the daemon's current live log threshold.
func (s *Server) handleGetLogLevel(w http.ResponseWriter, r *http.Request) {
	if s.LogLevel == nil {
		writeError(w, http.StatusServiceUnavailable, "log level control unavailable")
		return
	}
	writeJSON(w, http.StatusOK, logLevelResponse{Level: config.LevelString(s.LogLevel.Level())})
}

// handleSetLogLevel changes the daemon's live log threshold. The new level takes
// effect immediately for every logger built on the shared handler.
func (s *Server) handleSetLogLevel(w http.ResponseWriter, r *http.Request) {
	if s.LogLevel == nil {
		writeError(w, http.StatusServiceUnavailable, "log level control unavailable")
		return
	}
	var req logLevelRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	lvl, err := config.ParseLogLevel(req.Level)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.LogLevel.Set(lvl)
	writeJSON(w, http.StatusOK, logLevelResponse{Level: config.LevelString(lvl)})
}
