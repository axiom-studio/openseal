package server

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/axiom-studio/openseal/pkg/runtime"
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

func actionCallFilterFromQuery(r *http.Request) (runtime.ActionFilter, error) {
	scope, err := scopeFromQuery(r)
	if err != nil {
		return runtime.ActionFilter{}, err
	}
	limit, err := boundedIntQuery(r, "limit", 50, 1, 100)
	if err != nil {
		return runtime.ActionFilter{}, err
	}
	offset, err := boundedIntQuery(r, "offset", 0, 0, 1_000_000)
	if err != nil {
		return runtime.ActionFilter{}, err
	}
	filter := runtime.ActionFilter{
		Scope: scope, RunID: strings.TrimSpace(r.URL.Query().Get("runId")), Limit: limit, Offset: offset,
	}
	for _, value := range queryValues(r, "status") {
		status := runtime.ActionCallStatus(value)
		if !validActionCallStatus(status) {
			return runtime.ActionFilter{}, fmt.Errorf("invalid action call status %q", value)
		}
		filter.Status = append(filter.Status, status)
	}
	return filter, nil
}

func validActionCallStatus(status runtime.ActionCallStatus) bool {
	switch status {
	case runtime.ActionCallStatusReady, runtime.ActionCallStatusWaitingApproval, runtime.ActionCallStatusDenied,
		runtime.ActionCallStatusRunning, runtime.ActionCallStatusSucceeded, runtime.ActionCallStatusFailed,
		runtime.ActionCallStatusCanceled, runtime.ActionCallStatusCompensating, runtime.ActionCallStatusCompensated:
		return true
	default:
		return false
	}
}

func (s *Server) respondActionCallError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, runtime.ErrActionNotFound):
		s.respondError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, runtime.ErrInvalidScope):
		s.respondError(w, http.StatusBadRequest, err.Error())
	default:
		s.logger.Errorw("action call API failed", "error", err)
		s.respondError(w, http.StatusInternalServerError, "action call operation failed")
	}
}
