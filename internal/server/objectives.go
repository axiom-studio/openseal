package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/axiom-studio/openseal/pkg/kernelapi"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
)

func (s *Server) handleCreateObjective(w http.ResponseWriter, r *http.Request) {
	var payload kernelapi.CreateObjectiveRequest
	if err := decodeStrictJSON(r, &payload); err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" {
		key = strings.TrimSpace(payload.IdempotencyKey)
	}
	result, err := portfolio.NewPortfolioService(s.store).CreateObjectiveIdempotent(r.Context(), portfolio.CreateObjectiveRequest{
		Scope: payload.Scope, Owner: payload.Owner, Title: strings.TrimSpace(payload.Title), Goal: strings.TrimSpace(payload.Goal),
		Status: payload.Status, Priority: payload.Priority, Budget: payload.Budget,
		Constraints: payload.Constraints, SuccessCriteria: payload.SuccessCriteria, IdempotencyKey: key,
	})
	if err != nil {
		s.respondObjectiveError(w, err)
		return
	}
	status := http.StatusCreated
	if !result.Created {
		status = http.StatusOK
	}
	s.respondJSON(w, status, result.Objective)
}

func (s *Server) handleListObjectives(w http.ResponseWriter, r *http.Request) {
	filter, err := objectiveFilterFromQuery(r)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	objectives, err := portfolio.NewPortfolioService(s.store).ListObjectives(r.Context(), filter)
	if err != nil {
		s.respondObjectiveError(w, err)
		return
	}
	s.respondJSON(w, http.StatusOK, objectives)
}

func (s *Server) handleGetObjective(w http.ResponseWriter, r *http.Request) {
	scope, err := scopeFromQuery(r)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	service := portfolio.NewPortfolioService(s.store)
	objective, err := service.GetObjective(r.Context(), scope, strings.TrimSpace(r.PathValue("id")))
	if err != nil {
		s.respondObjectiveError(w, err)
		return
	}
	runs, err := service.ListAgentRuns(r.Context(), models.AgentRunFilter{Scope: scope, ObjectiveID: objective.ID, Limit: 100})
	if err != nil {
		s.respondObjectiveError(w, err)
		return
	}
	runbooks, err := s.store.ListRunbookActivations(r.Context(), models.RunbookActivationFilter{Scope: scope, ObjectiveID: objective.ID, Limit: 100})
	if err != nil {
		s.respondObjectiveError(w, err)
		return
	}
	s.respondJSON(w, http.StatusOK, kernelapi.ObjectiveDetail{Objective: objective, Runbooks: runbooks, Runs: runs})
}

func (s *Server) handleUpdateObjective(w http.ResponseWriter, r *http.Request) {
	scope, err := scopeFromQuery(r)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	var payload kernelapi.UpdateObjectiveRequest
	if err := decodeStrictJSON(r, &payload); err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	objective, err := portfolio.NewPortfolioService(s.store).UpdateObjective(r.Context(), scope, strings.TrimSpace(r.PathValue("id")), portfolio.UpdateObjectiveRequest{
		ExpectedRevision: payload.ExpectedRevision, Title: payload.Title, Goal: payload.Goal, Status: payload.Status,
		Priority: payload.Priority, Budget: payload.Budget,
		Constraints: payload.Constraints, SuccessCriteria: payload.SuccessCriteria, ProgressSummary: payload.ProgressSummary,
	})
	if err != nil {
		s.respondObjectiveError(w, err)
		return
	}
	s.respondJSON(w, http.StatusOK, objective)
}

func objectiveFilterFromQuery(r *http.Request) (models.ObjectiveFilter, error) {
	scope, err := scopeFromQuery(r)
	if err != nil {
		return models.ObjectiveFilter{}, err
	}
	limit, err := boundedIntQuery(r, "limit", 50, 1, 100)
	if err != nil {
		return models.ObjectiveFilter{}, err
	}
	offset, err := boundedIntQuery(r, "offset", 0, 0, 1_000_000)
	if err != nil {
		return models.ObjectiveFilter{}, err
	}
	filter := models.ObjectiveFilter{Scope: scope, Limit: limit, Offset: offset}
	ownerType, ownerID := strings.TrimSpace(r.URL.Query().Get("ownerType")), strings.TrimSpace(r.URL.Query().Get("ownerId"))
	if ownerType != "" || ownerID != "" {
		owner := models.ObjectiveOwner{Type: models.OwnerType(ownerType), ID: ownerID}
		if err := owner.Validate(); err != nil {
			return models.ObjectiveFilter{}, err
		}
		filter.Owner = &owner
	}
	for _, value := range queryValues(r, "status") {
		status := models.ObjectiveStatus(value)
		if !isPublicObjectiveStatus(status) {
			return models.ObjectiveFilter{}, errors.New("invalid objective status")
		}
		filter.Statuses = append(filter.Statuses, status)
	}
	return filter, nil
}

func isPublicObjectiveStatus(status models.ObjectiveStatus) bool {
	switch status {
	case models.ObjectiveStatusDraft, models.ObjectiveStatusActive, models.ObjectiveStatusPaused,
		models.ObjectiveStatusSatisfied, models.ObjectiveStatusFailed, models.ObjectiveStatusRetired:
		return true
	default:
		return false
	}
}

func (s *Server) respondObjectiveError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, models.ErrObjectiveNotFound):
		s.respondError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, models.ErrRevisionConflict), errors.Is(err, models.ErrObjectiveIdempotency), errors.Is(err, models.ErrBudgetExhausted):
		s.respondError(w, http.StatusConflict, err.Error())
	case errors.Is(err, portfolio.ErrInvalidObjectiveTransition):
		s.respondError(w, http.StatusConflict, err.Error())
	default:
		s.respondError(w, http.StatusBadRequest, err.Error())
	}
}
