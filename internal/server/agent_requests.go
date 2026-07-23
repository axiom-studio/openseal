package server

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/axiom-studio/openseal/pkg/kernelapi"
	"github.com/axiom-studio/openseal/pkg/runtime"
)

func (s *Server) collaborationService(w http.ResponseWriter) (*runtime.CollaborationService, bool) {
	store, ok := s.store.(runtime.CollaborationKernelStore)
	if !ok {
		s.respondError(w, http.StatusNotImplemented, "agent requests are not configured")
		return nil, false
	}
	return runtime.NewCollaborationService(store), true
}

func (s *Server) handleCreateAgentRequest(w http.ResponseWriter, r *http.Request) {
	service, ok := s.collaborationService(w)
	if !ok {
		return
	}
	var payload kernelapi.CreateAgentRequestRequest
	if err := decodeStrictJSON(r, &payload); err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if idempotencyKey == "" {
		idempotencyKey = strings.TrimSpace(payload.IdempotencyKey)
	}
	result, err := service.CreateAgentRequest(r.Context(), runtime.CreateAgentRequestRequest{
		ID: strings.TrimSpace(payload.ID), Scope: payload.Scope, Kind: payload.Kind,
		Requester: payload.Requester, Recipient: payload.Recipient, SourceRunID: strings.TrimSpace(payload.SourceRunID),
		Goal: strings.TrimSpace(payload.Goal), Instructions: strings.TrimSpace(payload.Instructions), SemanticRole: strings.TrimSpace(payload.SemanticRole),
		AcceptanceCriteria: payload.AcceptanceCriteria, ArtifactRequirements: payload.ArtifactRequirements,
		SharedContext: payload.SharedContext, ChildCheckpoint: payload.ChildCheckpoint, ConversationRefs: payload.ConversationRefs,
		BudgetAllocation: payload.BudgetAllocation, IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		s.respondAgentRequestError(w, err)
		return
	}
	status := http.StatusCreated
	if len(result.Events) == 0 {
		status = http.StatusOK
	}
	s.respondJSON(w, status, result)
}

func (s *Server) handleGetAgentRequest(w http.ResponseWriter, r *http.Request) {
	service, ok := s.collaborationService(w)
	if !ok {
		return
	}
	scope, err := scopeFromQuery(r)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	request, err := service.GetAgentRequest(r.Context(), scope, strings.TrimSpace(r.PathValue("id")))
	if err != nil {
		s.respondAgentRequestError(w, err)
		return
	}
	s.respondJSON(w, http.StatusOK, request)
}

func (s *Server) handleListAgentRequests(w http.ResponseWriter, r *http.Request) {
	service, ok := s.collaborationService(w)
	if !ok {
		return
	}
	filter, err := agentRequestFilterFromQuery(r)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	requests, err := service.ListAgentRequests(r.Context(), filter)
	if err != nil {
		s.respondAgentRequestError(w, err)
		return
	}
	s.respondJSON(w, http.StatusOK, requests)
}

func (s *Server) handleRespondAgentRequest(w http.ResponseWriter, r *http.Request) {
	service, ok := s.collaborationService(w)
	if !ok {
		return
	}
	scope, err := scopeFromQuery(r)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	var payload kernelapi.RespondAgentRequestRequest
	if err := decodeStrictJSON(r, &payload); err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	result, err := service.RespondAgentRequest(r.Context(), runtime.RespondAgentRequestRequest{
		Scope: scope, RequestID: strings.TrimSpace(r.PathValue("id")), ExpectedRevision: payload.ExpectedRevision,
		Decision: payload.Decision, Principal: payload.Principal, AssignedAgentID: strings.TrimSpace(payload.AssignedAgentID), Message: strings.TrimSpace(payload.Message),
	})
	if err != nil {
		s.respondAgentRequestError(w, err)
		return
	}
	s.respondJSON(w, http.StatusOK, result)
}

func (s *Server) handleCompleteAgentRequest(w http.ResponseWriter, r *http.Request) {
	service, ok := s.collaborationService(w)
	if !ok {
		return
	}
	scope, err := scopeFromQuery(r)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	var payload kernelapi.CompleteAgentRequestRequest
	if err := decodeStrictJSON(r, &payload); err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	actor := payload.Actor
	if actor.Type == "" && strings.TrimSpace(actor.ID) == "" {
		actor = payload.Principal
	}
	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if idempotencyKey == "" {
		idempotencyKey = strings.TrimSpace(payload.IdempotencyKey)
	}
	result, err := service.CompleteAgentRequest(r.Context(), runtime.CompleteAgentRequestRequest{
		Scope: scope, RequestID: strings.TrimSpace(r.PathValue("id")), ExpectedRevision: payload.ExpectedRevision,
		ExpectedChildRevision: payload.ExpectedChildRevision, Principal: payload.Principal, Actor: actor,
		Summary: strings.TrimSpace(payload.Summary), AcceptanceEvidence: payload.AcceptanceEvidence,
		Artifacts: payload.Artifacts, CompletionKey: idempotencyKey,
	})
	if err != nil {
		s.respondAgentRequestError(w, err)
		return
	}
	s.respondJSON(w, http.StatusOK, result)
}

func agentRequestFilterFromQuery(r *http.Request) (runtime.AgentRequestFilter, error) {
	scope, err := scopeFromQuery(r)
	if err != nil {
		return runtime.AgentRequestFilter{}, err
	}
	limit, err := boundedIntQuery(r, "limit", 50, 1, 100)
	if err != nil {
		return runtime.AgentRequestFilter{}, err
	}
	offset, err := boundedIntQuery(r, "offset", 0, 0, 1_000_000)
	if err != nil {
		return runtime.AgentRequestFilter{}, err
	}
	filter := runtime.AgentRequestFilter{Scope: scope, SourceRunID: strings.TrimSpace(r.URL.Query().Get("sourceRunId")), Limit: limit, Offset: offset}
	if filter.Requester, err = collaborationPartyFromQuery(r, "requester"); err != nil {
		return runtime.AgentRequestFilter{}, err
	}
	if filter.Recipient, err = collaborationPartyFromQuery(r, "recipient"); err != nil {
		return runtime.AgentRequestFilter{}, err
	}
	for _, value := range queryValues(r, "kind") {
		kind := runtime.AgentRequestKind(value)
		if kind != runtime.AgentRequestKindRequest && kind != runtime.AgentRequestKindHandoff {
			return runtime.AgentRequestFilter{}, fmt.Errorf("invalid agent request kind %q", value)
		}
		filter.Kinds = append(filter.Kinds, kind)
	}
	for _, value := range queryValues(r, "status") {
		status := runtime.AgentRequestStatus(value)
		if !validAgentRequestStatus(status) {
			return runtime.AgentRequestFilter{}, fmt.Errorf("invalid agent request status %q", value)
		}
		filter.Statuses = append(filter.Statuses, status)
	}
	return filter, nil
}

func collaborationPartyFromQuery(r *http.Request, prefix string) (*runtime.CollaborationParty, error) {
	typeValue := strings.TrimSpace(r.URL.Query().Get(prefix + "Type"))
	id := strings.TrimSpace(r.URL.Query().Get(prefix + "Id"))
	if typeValue == "" && id == "" {
		return nil, nil
	}
	party := runtime.CollaborationParty{Type: runtime.OwnerType(typeValue), ID: id}
	if typeValue == "" || id == "" {
		return nil, fmt.Errorf("%sType and %sId must be provided together", prefix, prefix)
	}
	if err := party.Validate(); err != nil {
		return nil, err
	}
	return &party, nil
}

func validAgentRequestStatus(status runtime.AgentRequestStatus) bool {
	switch status {
	case runtime.AgentRequestStatusPending, runtime.AgentRequestStatusClarificationRequested, runtime.AgentRequestStatusAccepted,
		runtime.AgentRequestStatusCompleted, runtime.AgentRequestStatusFailed, runtime.AgentRequestStatusRejected, runtime.AgentRequestStatusCanceled:
		return true
	default:
		return false
	}
}

func (s *Server) respondAgentRequestError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, runtime.ErrRunNotFound), errors.Is(err, runtime.ErrAgentRequestNotFound), errors.Is(err, runtime.ErrArtifactNotFound):
		s.respondError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, runtime.ErrAgentRequestUnauthorized):
		s.respondError(w, http.StatusForbidden, err.Error())
	case errors.Is(err, runtime.ErrRevisionConflict), errors.Is(err, runtime.ErrInvalidAgentRequestState),
		errors.Is(err, runtime.ErrAgentRequestIdempotency), errors.Is(err, runtime.ErrBudgetExhausted):
		s.respondError(w, http.StatusConflict, err.Error())
	case errors.Is(err, runtime.ErrAgentRequestAssignment), errors.Is(err, runtime.ErrUnsafeSharedContext), errors.Is(err, runtime.ErrInvalidArtifact),
		errors.Is(err, runtime.ErrInvalidScope), errors.Is(err, runtime.ErrInvalidOwner):
		s.respondError(w, http.StatusBadRequest, err.Error())
	case strings.Contains(err.Error(), "required") || strings.Contains(err.Error(), "must be") || strings.Contains(err.Error(), "invalid"):
		s.respondError(w, http.StatusBadRequest, err.Error())
	default:
		s.logger.Errorw("agent request API failed", "error", err)
		s.respondError(w, http.StatusInternalServerError, "agent request operation failed")
	}
}
