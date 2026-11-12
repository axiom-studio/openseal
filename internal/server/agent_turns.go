package server

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/axiom-studio/openseal/pkg/kernelapi"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/activity"
)

func (s *Server) handleListAgentTurns(w http.ResponseWriter, r *http.Request) {
	filter, err := agentTurnFilterFromQuery(r)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	turns, err := activity.NewAgentTurnService(s.store, s.store).ListTurns(r.Context(), filter)
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
	turn, err := activity.NewAgentTurnService(s.store, s.store).GetTurn(
		r.Context(), scope, strings.TrimSpace(r.PathValue("id")),
	)
	if err != nil {
		s.respondAgentTurnError(w, err)
		return
	}
	s.respondJSON(w, http.StatusOK, kernelapi.ProjectAgentTurn(turn))
}

func agentTurnFilterFromQuery(r *http.Request) (models.AgentTurnFilter, error) {
	scope, err := scopeFromQuery(r)
	if err != nil {
		return models.AgentTurnFilter{}, err
	}
	runID := strings.TrimSpace(r.URL.Query().Get("runId"))
	if runID == "" {
		return models.AgentTurnFilter{}, errors.New("runId is required")
	}
	limit, err := boundedIntQuery(r, "limit", 50, 1, 100)
	if err != nil {
		return models.AgentTurnFilter{}, err
	}
	afterSequence, err := boundedIntQuery(r, "afterSequence", 0, 0, 1_000_000_000)
	if err != nil {
		return models.AgentTurnFilter{}, err
	}
	filter := models.AgentTurnFilter{
		Scope: scope, RunID: runID, AfterSequence: int64(afterSequence), Limit: limit,
	}
	for _, value := range queryValues(r, "status") {
		status := models.AgentTurnStatus(value)
		if !validAgentTurnStatus(status) {
			return models.AgentTurnFilter{}, fmt.Errorf("invalid agent turn status %q", value)
		}
		filter.Statuses = append(filter.Statuses, status)
	}
	return filter, nil
}

func validAgentTurnStatus(status models.AgentTurnStatus) bool {
	switch status {
	case models.AgentTurnStatusRunning, models.AgentTurnStatusCompleted,
		models.AgentTurnStatusFailed, models.AgentTurnStatusCanceled:
		return true
	default:
		return false
	}
}

func (s *Server) respondAgentTurnError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, models.ErrTurnNotFound), errors.Is(err, models.ErrRunNotFound):
		s.respondError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, models.ErrInvalidScope):
		s.respondError(w, http.StatusBadRequest, err.Error())
	default:
		s.logger.Errorw("agent turn API failed", "error", err)
		s.respondError(w, http.StatusInternalServerError, "agent turn operation failed")
	}
}
