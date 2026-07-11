package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/axiom-studio/openseal/pkg/kernelapi"
	"github.com/axiom-studio/openseal/pkg/runtime"
)

var standaloneInitiativeActor = runtime.ActivityActor{Type: "user", ID: "standalone-operator"}

func (s *Server) initiativeService(w http.ResponseWriter) (*runtime.InitiativeService, bool) {
	store, ok := s.store.(runtime.InitiativeStore)
	if !ok {
		s.respondError(w, http.StatusServiceUnavailable, "initiative capability is unavailable")
		return nil, false
	}
	return runtime.NewInitiativeService(store, s.store), true
}

func (s *Server) handleCreateInitiative(w http.ResponseWriter, r *http.Request) {
	service, ok := s.initiativeService(w)
	if !ok {
		return
	}
	var payload kernelapi.CreateInitiativeRequest
	if err := decodeStrictJSON(r, &payload); err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" {
		key = strings.TrimSpace(payload.IdempotencyKey)
	}
	initiative := &runtime.Initiative{
		ID: strings.TrimSpace(payload.ID), Scope: payload.Scope, Owner: payload.Owner,
		Title: strings.TrimSpace(payload.Title), Purpose: strings.TrimSpace(payload.Purpose), Status: payload.Status,
		AgentRefs: payload.AgentRefs, TeamRefs: payload.TeamRefs, ObjectiveRefs: payload.ObjectiveRefs, RunRefs: payload.RunRefs,
		Milestones: payload.Milestones, Hypotheses: payload.Hypotheses, SourceMonitors: payload.SourceMonitors,
		Deliverables: payload.Deliverables, Budget: payload.Budget, Policy: payload.Policy, Checkpoint: payload.Checkpoint,
	}
	created, event, err := service.Create(r.Context(), runtime.CreateInitiativeRequest{
		Initiative: initiative, IdempotencyKey: key, Actor: standaloneInitiativeActor, Visibility: runtime.ActivityVisibilityScope,
	})
	if err != nil {
		s.respondInitiativeError(w, err)
		return
	}
	status := http.StatusCreated
	if event == nil {
		status = http.StatusOK
	}
	s.respondJSON(w, status, created)
}

func (s *Server) handleListInitiatives(w http.ResponseWriter, r *http.Request) {
	service, ok := s.initiativeService(w)
	if !ok {
		return
	}
	filter, err := initiativeFilterFromQuery(r)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	initiatives, err := service.List(r.Context(), filter)
	if err != nil {
		s.respondInitiativeError(w, err)
		return
	}
	s.respondJSON(w, http.StatusOK, initiatives)
}

func (s *Server) handleGetInitiative(w http.ResponseWriter, r *http.Request) {
	service, ok := s.initiativeService(w)
	if !ok {
		return
	}
	scope, err := scopeFromQuery(r)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	initiative, err := service.Get(r.Context(), scope, strings.TrimSpace(r.PathValue("id")))
	if err != nil {
		s.respondInitiativeError(w, err)
		return
	}
	s.respondJSON(w, http.StatusOK, initiative)
}

func (s *Server) handlePatchInitiative(w http.ResponseWriter, r *http.Request) {
	service, ok := s.initiativeService(w)
	if !ok {
		return
	}
	scope, err := scopeFromQuery(r)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	var payload kernelapi.UpdateInitiativeRequest
	if err := decodeStrictJSON(r, &payload); err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	updated, _, err := service.Patch(r.Context(), scope, strings.TrimSpace(r.PathValue("id")), runtime.UpdateInitiativeRequest{
		ExpectedRevision: payload.ExpectedRevision, Title: payload.Title, Purpose: payload.Purpose, Status: payload.Status,
		AgentRefs: payload.AgentRefs, TeamRefs: payload.TeamRefs, ObjectiveRefs: payload.ObjectiveRefs, RunRefs: payload.RunRefs,
		Milestones: payload.Milestones, Hypotheses: payload.Hypotheses, SourceMonitors: payload.SourceMonitors,
		Deliverables: payload.Deliverables, Budget: payload.Budget, ClearBudget: payload.ClearBudget,
		Policy: payload.Policy, Checkpoint: payload.Checkpoint, Actor: standaloneInitiativeActor, Visibility: runtime.ActivityVisibilityScope,
	})
	if err != nil {
		s.respondInitiativeError(w, err)
		return
	}
	s.respondJSON(w, http.StatusOK, updated)
}

func initiativeFilterFromQuery(r *http.Request) (runtime.InitiativeFilter, error) {
	scope, err := scopeFromQuery(r)
	if err != nil {
		return runtime.InitiativeFilter{}, err
	}
	limit, err := boundedIntQuery(r, "limit", 50, 1, 100)
	if err != nil {
		return runtime.InitiativeFilter{}, err
	}
	offset, err := boundedIntQuery(r, "offset", 0, 0, 1_000_000)
	if err != nil {
		return runtime.InitiativeFilter{}, err
	}
	filter := runtime.InitiativeFilter{
		Scope: scope, ObjectiveID: strings.TrimSpace(r.URL.Query().Get("objectiveId")), Limit: limit, Offset: offset,
	}
	ownerType, ownerID := strings.TrimSpace(r.URL.Query().Get("ownerType")), strings.TrimSpace(r.URL.Query().Get("ownerId"))
	if ownerType != "" || ownerID != "" {
		owner := runtime.ObjectiveOwner{Type: runtime.OwnerType(ownerType), ID: ownerID}
		if err := owner.Validate(); err != nil {
			return runtime.InitiativeFilter{}, err
		}
		filter.Owner = &owner
	}
	for _, value := range queryValues(r, "status") {
		status := runtime.InitiativeStatus(value)
		if !isPublicInitiativeStatus(status) {
			return runtime.InitiativeFilter{}, errors.New("invalid initiative status")
		}
		filter.Statuses = append(filter.Statuses, status)
	}
	return filter, nil
}

func isPublicInitiativeStatus(status runtime.InitiativeStatus) bool {
	switch status {
	case runtime.InitiativeStatusDraft, runtime.InitiativeStatusActive, runtime.InitiativeStatusPaused,
		runtime.InitiativeStatusCompleted, runtime.InitiativeStatusFailed, runtime.InitiativeStatusCanceled, runtime.InitiativeStatusArchived:
		return true
	default:
		return false
	}
}

func (s *Server) respondInitiativeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, runtime.ErrInitiativeNotFound):
		s.respondError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, runtime.ErrInitiativeConflict), errors.Is(err, runtime.ErrInitiativeIdempotency):
		s.respondError(w, http.StatusConflict, err.Error())
	default:
		s.respondError(w, http.StatusBadRequest, err.Error())
	}
}
