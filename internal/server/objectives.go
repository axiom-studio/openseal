package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/axiom-studio/openseal/pkg/kernelapi"
	"github.com/axiom-studio/openseal/pkg/runtime"
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
	result, err := runtime.NewPortfolioService(s.store).CreateObjectiveIdempotent(r.Context(), runtime.CreateObjectiveRequest{
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
	objectives, err := runtime.NewPortfolioService(s.store).ListObjectives(r.Context(), filter)
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
	service := runtime.NewPortfolioService(s.store)
	objective, err := service.GetObjective(r.Context(), scope, strings.TrimSpace(r.PathValue("id")))
	if err != nil {
		s.respondObjectiveError(w, err)
		return
	}
	runs, err := service.ListAgentRuns(r.Context(), runtime.AgentRunFilter{Scope: scope, ObjectiveID: objective.ID, Limit: 100})
	if err != nil {
		s.respondObjectiveError(w, err)
		return
	}
	s.respondJSON(w, http.StatusOK, kernelapi.ObjectiveDetail{Objective: objective, Runs: runs})
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
	objective, err := runtime.NewPortfolioService(s.store).UpdateObjective(r.Context(), scope, strings.TrimSpace(r.PathValue("id")), runtime.UpdateObjectiveRequest{
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

func objectiveFilterFromQuery(r *http.Request) (runtime.ObjectiveFilter, error) {
	scope, err := scopeFromQuery(r)
	if err != nil {
		return runtime.ObjectiveFilter{}, err
	}
	limit, err := boundedIntQuery(r, "limit", 50, 1, 100)
	if err != nil {
		return runtime.ObjectiveFilter{}, err
	}
	offset, err := boundedIntQuery(r, "offset", 0, 0, 1_000_000)
	if err != nil {
		return runtime.ObjectiveFilter{}, err
	}
	filter := runtime.ObjectiveFilter{Scope: scope, Limit: limit, Offset: offset}
	ownerType, ownerID := strings.TrimSpace(r.URL.Query().Get("ownerType")), strings.TrimSpace(r.URL.Query().Get("ownerId"))
	if ownerType != "" || ownerID != "" {
		owner := runtime.ObjectiveOwner{Type: runtime.OwnerType(ownerType), ID: ownerID}
		if err := owner.Validate(); err != nil {
			return runtime.ObjectiveFilter{}, err
		}
		filter.Owner = &owner
	}
	for _, value := range queryValues(r, "status") {
		status := runtime.ObjectiveStatus(value)
		if !isPublicObjectiveStatus(status) {
			return runtime.ObjectiveFilter{}, errors.New("invalid objective status")
		}
		filter.Statuses = append(filter.Statuses, status)
	}
	return filter, nil
}

func isPublicObjectiveStatus(status runtime.ObjectiveStatus) bool {
	switch status {
	case runtime.ObjectiveStatusDraft, runtime.ObjectiveStatusActive, runtime.ObjectiveStatusPaused,
		runtime.ObjectiveStatusSatisfied, runtime.ObjectiveStatusFailed, runtime.ObjectiveStatusRetired:
		return true
	default:
		return false
	}
}

func (s *Server) respondObjectiveError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, runtime.ErrObjectiveNotFound):
		s.respondError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, runtime.ErrRevisionConflict), errors.Is(err, runtime.ErrObjectiveIdempotency), errors.Is(err, runtime.ErrBudgetExhausted):
		s.respondError(w, http.StatusConflict, err.Error())
	case errors.Is(err, runtime.ErrInvalidObjectiveTransition):
		s.respondError(w, http.StatusConflict, err.Error())
	default:
		s.respondError(w, http.StatusBadRequest, err.Error())
	}
}
