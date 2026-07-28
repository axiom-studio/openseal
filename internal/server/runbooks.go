package server

import (
	"errors"
	"net/http"
	"strings"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/axiom-studio/openseal/pkg/runtime"
)

func (s *Server) handleListRunbooks(w http.ResponseWriter, r *http.Request) {
	filter, err := runbookFilterFromQuery(r)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	values, err := s.store.ListRunbookActivations(r.Context(), filter)
	if err != nil {
		s.respondRunbookError(w, err)
		return
	}
	s.respondJSON(w, http.StatusOK, values)
}

func (s *Server) handleGetRunbook(w http.ResponseWriter, r *http.Request) {
	scope, err := scopeFromQuery(r)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	registry, err := s.agentRegistry()
	if err != nil {
		s.respondError(w, http.StatusNotImplemented, err.Error())
		return
	}
	value, err := runtime.ResolveRunbookDetail(r.Context(), s.store, registry, scope, strings.TrimSpace(r.PathValue("id")))
	if err != nil {
		s.respondRunbookError(w, err)
		return
	}
	if value == nil {
		s.respondRunbookError(w, runtime.ErrRunbookActivationNotFound)
		return
	}
	s.respondJSON(w, http.StatusOK, value)
}

func (s *Server) handleUpdateRunbook(w http.ResponseWriter, r *http.Request) {
	scope, err := scopeFromQuery(r)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	var request runtime.UpdateRunbookActivationRequest
	if err = decodeStrictJSON(r, &request); err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	value, err := runtime.NewRunbookActivationService(s.store).Update(r.Context(), scope, strings.TrimSpace(r.PathValue("id")), request)
	if err != nil {
		s.respondRunbookError(w, err)
		return
	}
	s.respondJSON(w, http.StatusOK, value)
}

func (s *Server) handleStartRunbook(w http.ResponseWriter, r *http.Request) {
	scope, err := scopeFromQuery(r)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	var request runtime.StartRunbookActivationRequest
	if err = decodeStrictJSON(r, &request); err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	value, err := runtime.StartRunbookActivation(r.Context(), s.store, scope, strings.TrimSpace(r.PathValue("id")), request)
	if err != nil {
		s.respondRunbookError(w, err)
		return
	}
	s.respondJSON(w, http.StatusCreated, value)
}

func runbookFilterFromQuery(r *http.Request) (runtime.RunbookActivationFilter, error) {
	scope, err := scopeFromQuery(r)
	if err != nil {
		return runtime.RunbookActivationFilter{}, err
	}
	limit, err := boundedIntQuery(r, "limit", 100, 1, 500)
	if err != nil {
		return runtime.RunbookActivationFilter{}, err
	}
	offset, err := boundedIntQuery(r, "offset", 0, 0, 1000000)
	if err != nil {
		return runtime.RunbookActivationFilter{}, err
	}
	filter := runtime.RunbookActivationFilter{
		Scope: scope, ObjectiveID: strings.TrimSpace(r.URL.Query().Get("objectiveId")), Limit: limit, Offset: offset,
	}
	ownerType, ownerID := strings.TrimSpace(r.URL.Query().Get("ownerType")), strings.TrimSpace(r.URL.Query().Get("ownerId"))
	if ownerType != "" || ownerID != "" {
		owner := runtime.ObjectiveOwner{Type: runtime.OwnerType(ownerType), ID: ownerID}
		if err := owner.Validate(); err != nil {
			return runtime.RunbookActivationFilter{}, err
		}
		filter.Owner = &owner
	}
	for _, value := range queryValues(r, "status") {
		status := runtime.RunbookActivationStatus(value)
		switch status {
		case runtime.RunbookActivationActive, runtime.RunbookActivationPaused, runtime.RunbookActivationRetired:
			filter.Statuses = append(filter.Statuses, status)
		default:
			return runtime.RunbookActivationFilter{}, errors.New("invalid Runbook status")
		}
	}
	for _, value := range queryValues(r, "triggerKind") {
		kind := runbook.TriggerKind(value)
		switch kind {
		case runbook.TriggerSchedule, runbook.TriggerEvent:
			filter.TriggerKinds = append(filter.TriggerKinds, kind)
		default:
			return runtime.RunbookActivationFilter{}, errors.New("invalid Runbook trigger kind")
		}
	}
	return filter, nil
}

func (s *Server) respondRunbookError(w http.ResponseWriter, err error) {
	if errors.Is(err, runtime.ErrRunbookActivationNotFound) {
		s.respondError(w, http.StatusNotFound, err.Error())
		return
	}
	if errors.Is(err, runtime.ErrRunbookActivationRevision) {
		s.respondError(w, http.StatusConflict, err.Error())
		return
	}
	if errors.Is(err, runtime.ErrRunbookActivationInactive) {
		s.respondError(w, http.StatusConflict, err.Error())
		return
	}
	if errors.Is(err, runtime.ErrRunbookDefinitionNotFound) || errors.Is(err, kernelagent.ErrDeploymentNotFound) || errors.Is(err, kernelagent.ErrDefinitionNotFound) {
		s.respondError(w, http.StatusConflict, err.Error())
		return
	}
	s.respondError(w, http.StatusBadRequest, err.Error())
}
