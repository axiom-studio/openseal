package server

import (
	"net/http"
	"time"

	"github.com/axiom-studio/openseal/pkg/kernelapi"
	"github.com/axiom-studio/openseal/pkg/runtime"
)

const (
	defaultRunbookScheduleReconcileLimit = 50
	maximumRunbookScheduleReconcileLimit = 500
)

func (s *Server) handleReconcileRunbookSchedules(w http.ResponseWriter, r *http.Request) {
	var payload kernelapi.ReconcileRunbookSchedulesRequest
	if err := decodeStrictJSON(r, &payload); err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := payload.Scope.Validate(); err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	if payload.Limit < 0 || payload.Limit > maximumRunbookScheduleReconcileLimit {
		s.respondError(w, http.StatusBadRequest, "limit must be between 1 and 500 when provided")
		return
	}
	limit := payload.Limit
	if limit == 0 {
		limit = defaultRunbookScheduleReconcileLimit
	}
	result, err := runtime.NewRunbookScheduler(s.store).ReconcileScope(r.Context(), payload.Scope, limit)
	if err != nil {
		s.respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.respondJSON(w, http.StatusOK, kernelapi.RunbookScheduleReconciliation{
		Scope: payload.Scope, ReconciledAt: time.Now().UTC(), Result: result,
	})
}
