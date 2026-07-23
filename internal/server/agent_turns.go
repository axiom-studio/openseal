package server

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/axiom-studio/openseal/pkg/kernelapi"
	"github.com/axiom-studio/openseal/pkg/runtime"
)

func (s *Server) handleListAgentTurns(w http.ResponseWriter, r *http.Request) {
	filter, err := agentTurnFilterFromQuery(r)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	turns, err := runtime.NewAgentTurnService(s.store, s.store).ListTurns(r.Context(), filter)
	if err != nil {
		s.respondAgentTurnError(w, err)
		return
	}
	records := make([]*kernelapi.AgentTurnRecord, 0, len(turns))
	for _, turn := range turns {
		records = append(records, kernelapi.ProjectAgentTurn(turn))
	}
	s.respondJSON(w, http.StatusOK, records)
}

func (s *Server) handleGetAgentTurn(w http.ResponseWriter, r *http.Request) {
	scope, err := scopeFromQuery(r)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	turn, err := runtime.NewAgentTurnService(s.store, s.store).GetTurn(
		r.Context(), scope, strings.TrimSpace(r.PathValue("id")),
	)
	if err != nil {
		s.respondAgentTurnError(w, err)
		return
	}
	s.respondJSON(w, http.StatusOK, kernelapi.ProjectAgentTurn(turn))
}

func agentTurnFilterFromQuery(r *http.Request) (runtime.AgentTurnFilter, error) {
	scope, err := scopeFromQuery(r)
	if err != nil {
		return runtime.AgentTurnFilter{}, err
	}
	runID := strings.TrimSpace(r.URL.Query().Get("runId"))
	if runID == "" {
		return runtime.AgentTurnFilter{}, errors.New("runId is required")
	}
	limit, err := boundedIntQuery(r, "limit", 50, 1, 100)
	if err != nil {
		return runtime.AgentTurnFilter{}, err
	}
	afterSequence, err := boundedIntQuery(r, "afterSequence", 0, 0, 1_000_000_000)
	if err != nil {
		return runtime.AgentTurnFilter{}, err
	}
	filter := runtime.AgentTurnFilter{
		Scope: scope, RunID: runID, AfterSequence: int64(afterSequence), Limit: limit,
	}
	for _, value := range queryValues(r, "status") {
		status := runtime.AgentTurnStatus(value)
		if !validAgentTurnStatus(status) {
			return runtime.AgentTurnFilter{}, fmt.Errorf("invalid agent turn status %q", value)
		}
		filter.Statuses = append(filter.Statuses, status)
	}
	return filter, nil
}

func validAgentTurnStatus(status runtime.AgentTurnStatus) bool {
	switch status {
	case runtime.AgentTurnStatusRunning, runtime.AgentTurnStatusCompleted,
		runtime.AgentTurnStatusFailed, runtime.AgentTurnStatusCanceled:
		return true
	default:
		return false
	}
}

func (s *Server) respondAgentTurnError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, runtime.ErrTurnNotFound), errors.Is(err, runtime.ErrRunNotFound):
		s.respondError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, runtime.ErrInvalidScope):
		s.respondError(w, http.StatusBadRequest, err.Error())
	default:
		s.logger.Errorw("agent turn API failed", "error", err)
		s.respondError(w, http.StatusInternalServerError, "agent turn operation failed")
	}
}
