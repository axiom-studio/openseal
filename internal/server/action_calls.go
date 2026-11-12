package server

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/axiom-studio/openseal/pkg/models"
)

func (s *Server) handleListActionCalls(w http.ResponseWriter, r *http.Request) {
	filter, err := actionCallFilterFromQuery(r)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	calls, err := s.store.ListActionCalls(r.Context(), filter)
	if err != nil {
		s.respondActionCallError(w, err)
		return
	}
	s.respondJSON(w, http.StatusOK, calls)
}

func (s *Server) handleGetActionCall(w http.ResponseWriter, r *http.Request) {
	scope, err := scopeFromQuery(r)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	call, err := s.store.GetActionCall(r.Context(), scope, strings.TrimSpace(r.PathValue("id")))
	if err != nil {
		s.respondActionCallError(w, err)
		return
	}
	s.respondJSON(w, http.StatusOK, call)
}

func actionCallFilterFromQuery(r *http.Request) (models.ActionFilter, error) {
	scope, err := scopeFromQuery(r)
	if err != nil {
		return models.ActionFilter{}, err
	}
	limit, err := boundedIntQuery(r, "limit", 50, 1, 100)
	if err != nil {
		return models.ActionFilter{}, err
	}
	offset, err := boundedIntQuery(r, "offset", 0, 0, 1_000_000)
	if err != nil {
		return models.ActionFilter{}, err
	}
	filter := models.ActionFilter{
		Scope: scope, RunID: strings.TrimSpace(r.URL.Query().Get("runId")), Limit: limit, Offset: offset,
	}
	for _, value := range queryValues(r, "status") {
		status := models.ActionCallStatus(value)
		if !validActionCallStatus(status) {
			return models.ActionFilter{}, fmt.Errorf("invalid action call status %q", value)
		}
		filter.Status = append(filter.Status, status)
	}
	return filter, nil
}

func validActionCallStatus(status models.ActionCallStatus) bool {
	switch status {
	case models.ActionCallStatusReady, models.ActionCallStatusWaitingApproval, models.ActionCallStatusDenied,
		models.ActionCallStatusRunning, models.ActionCallStatusSucceeded, models.ActionCallStatusFailed,
		models.ActionCallStatusCanceled, models.ActionCallStatusCompensating, models.ActionCallStatusCompensated:
		return true
	default:
		return false
	}
}

func (s *Server) respondActionCallError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, models.ErrActionNotFound):
		s.respondError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, models.ErrInvalidScope):
		s.respondError(w, http.StatusBadRequest, err.Error())
	default:
		s.logger.Errorw("action call API failed", "error", err)
		s.respondError(w, http.StatusInternalServerError, "action call operation failed")
	}
}
