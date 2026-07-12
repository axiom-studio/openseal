package server

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/axiom-studio/openseal/pkg/runtime"
)

func (s *Server) sourceMonitorService(w http.ResponseWriter) (*runtime.SourceMonitorService, bool) {
	store, ok := s.store.(runtime.SourceMonitorStore)
	initiatives, initiativesOK := s.store.(runtime.InitiativeStore)
	if !ok || !initiativesOK {
		s.respondError(w, http.StatusServiceUnavailable, "source monitor capability is unavailable")
		return nil, false
	}
	return runtime.NewSourceMonitorService(store, initiatives, s.store, nil), true
}

func (s *Server) handleListSourceObservations(w http.ResponseWriter, r *http.Request) {
	service, ok := s.sourceMonitorService(w)
	if !ok {
		return
	}
	scope, err := scopeFromQuery(r)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	limit, offset := 50, 0
	if raw := r.URL.Query().Get("limit"); raw != "" {
		limit, err = strconv.Atoi(raw)
	}
	if err == nil {
		if raw := r.URL.Query().Get("offset"); raw != "" {
			offset, err = strconv.Atoi(raw)
		}
	}
	if err != nil || limit < 1 || limit > 100 || offset < 0 {
		s.respondError(w, http.StatusBadRequest, "observation limit must be between 1 and 100 and offset cannot be negative")
		return
	}
	initiativeID, monitorID := strings.TrimSpace(r.PathValue("id")), strings.TrimSpace(r.PathValue("monitorId"))
	if !s.sourceMonitorBelongsToInitiative(r, scope, initiativeID, monitorID) {
		s.respondError(w, http.StatusNotFound, "source monitor was not found")
		return
	}
	values, err := service.List(r.Context(), runtime.SourceObservationFilter{Scope: scope, InitiativeID: initiativeID, MonitorID: monitorID, Limit: limit, Offset: offset})
	if err != nil {
		s.respondSourceMonitorError(w, err)
		return
	}
	s.respondJSON(w, http.StatusOK, values)
}

func (s *Server) handleGetSourceMonitorCheckpoint(w http.ResponseWriter, r *http.Request) {
	service, ok := s.sourceMonitorService(w)
	if !ok {
		return
	}
	scope, err := scopeFromQuery(r)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	initiativeID, monitorID := strings.TrimSpace(r.PathValue("id")), strings.TrimSpace(r.PathValue("monitorId"))
	if !s.sourceMonitorBelongsToInitiative(r, scope, initiativeID, monitorID) {
		s.respondError(w, http.StatusNotFound, "source monitor was not found")
		return
	}
	checkpoint, err := service.GetCheckpoint(r.Context(), scope, initiativeID, monitorID)
	if err != nil {
		s.respondSourceMonitorError(w, err)
		return
	}
	s.respondJSON(w, http.StatusOK, checkpoint)
}

func (s *Server) sourceMonitorBelongsToInitiative(r *http.Request, scope runtime.Scope, initiativeID, monitorID string) bool {
	store, ok := s.store.(runtime.InitiativeStore)
	if !ok {
		return false
	}
	initiative, err := store.GetInitiative(r.Context(), scope, initiativeID)
	if err != nil || initiative == nil {
		return false
	}
	for _, monitor := range initiative.SourceMonitors {
		if monitor.ID == monitorID {
			return true
		}
	}
	return false
}

func (s *Server) respondSourceMonitorError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, runtime.ErrSourceObservationNotFound):
		s.respondError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, runtime.ErrInvalidSourceObservation):
		s.respondError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, runtime.ErrSourceMonitorCheckpoint):
		s.respondError(w, http.StatusConflict, err.Error())
	default:
		s.respondError(w, http.StatusInternalServerError, "source monitor operation failed")
	}
}
