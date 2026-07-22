package server

import (
	"net/http"
	"time"

	"github.com/axiom-studio/openseal/pkg/kernelapi"
	"github.com/axiom-studio/openseal/pkg/runtime"
)

const (
	defaultObjectiveScheduleReconcileLimit = 50
	maximumObjectiveScheduleReconcileLimit = 500
)

func (s *Server) handleReconcileObjectiveSchedules(w http.ResponseWriter, r *http.Request) {
	var payload kernelapi.ReconcileObjectiveSchedulesRequest
	if err := decodeStrictJSON(r, &payload); err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := payload.Scope.Validate(); err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	if payload.Limit < 0 || payload.Limit > maximumObjectiveScheduleReconcileLimit {
		s.respondError(w, http.StatusBadRequest, "limit must be between 1 and 500 when provided")
		return
	}
	limit := payload.Limit
	if limit == 0 {
		limit = defaultObjectiveScheduleReconcileLimit
	}
	result, err := runtime.NewObjectiveScheduler(s.store).ReconcileScope(r.Context(), payload.Scope, limit)
	if err != nil {
		s.respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.respondJSON(w, http.StatusOK, kernelapi.ObjectiveScheduleReconciliation{
		Scope: payload.Scope, ReconciledAt: time.Now().UTC(), Result: result,
	})
}
