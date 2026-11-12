package server

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/axiom-studio/openseal/pkg/kernelapi"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/artifactcatalog"
	"github.com/axiom-studio/openseal/pkg/runtime/collaboration"
)

func (s *Server) collaborationService(w http.ResponseWriter) (*collaboration.Service, bool) {
	store, ok := s.store.(models.CollaborationKernelStore)
	if !ok {
		s.respondError(w, http.StatusNotImplemented, "agent requests are not configured")
		return nil, false
	}
	return collaboration.NewService(store), true
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
	result, err := service.CreateAgentRequest(r.Context(), collaboration.CreateAgentRequestRequest{
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
	result, err := service.RespondAgentRequest(r.Context(), collaboration.RespondAgentRequestRequest{
		Scope: scope, RequestID: strings.TrimSpace(r.PathValue("id")), ExpectedRevision: payload.ExpectedRevision,
		Decision: payload.Decision, Principal: payload.Principal, Actor: payload.Actor,
		AssignedAgentID: strings.TrimSpace(payload.AssignedAgentID), Message: strings.TrimSpace(payload.Message),
		DecisionRunID: strings.TrimSpace(payload.DecisionRunID),
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
	result, err := service.CompleteAgentRequest(r.Context(), collaboration.CompleteAgentRequestRequest{
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

func (s *Server) handleReviewAgentRequestCompletion(w http.ResponseWriter, r *http.Request) {
	service, ok := s.collaborationService(w)
	if !ok {
		return
	}
	scope, err := scopeFromQuery(r)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	var payload kernelapi.ReviewAgentRequestCompletionRequest
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
	result, err := service.ReviewAgentRequestCompletion(r.Context(), collaboration.ReviewAgentRequestCompletionRequest{
		Scope: scope, RequestID: strings.TrimSpace(r.PathValue("id")), ExpectedRevision: payload.ExpectedRevision,
		ExpectedChildRevision: payload.ExpectedChildRevision, Principal: payload.Principal, Actor: actor,
		Approve: payload.Approve, Summary: strings.TrimSpace(payload.Summary), IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		s.respondAgentRequestError(w, err)
		return
	}
	s.respondJSON(w, http.StatusOK, result)
}

func agentRequestFilterFromQuery(r *http.Request) (models.AgentRequestFilter, error) {
	scope, err := scopeFromQuery(r)
	if err != nil {
		return models.AgentRequestFilter{}, err
	}
	limit, err := boundedIntQuery(r, "limit", 50, 1, 100)
	if err != nil {
		return models.AgentRequestFilter{}, err
	}
	offset, err := boundedIntQuery(r, "offset", 0, 0, 1_000_000)
	if err != nil {
		return models.AgentRequestFilter{}, err
	}
	filter := models.AgentRequestFilter{Scope: scope, SourceRunID: strings.TrimSpace(r.URL.Query().Get("sourceRunId")), Limit: limit, Offset: offset}
	if filter.Requester, err = collaborationPartyFromQuery(r, "requester"); err != nil {
		return models.AgentRequestFilter{}, err
	}
	if filter.Recipient, err = collaborationPartyFromQuery(r, "recipient"); err != nil {
		return models.AgentRequestFilter{}, err
	}
	for _, value := range queryValues(r, "kind") {
		kind := models.AgentRequestKind(value)
		if kind != models.AgentRequestKindRequest && kind != models.AgentRequestKindHandoff && kind != models.AgentRequestKindEscalation {
			return models.AgentRequestFilter{}, fmt.Errorf("invalid agent request kind %q", value)
		}
		filter.Kinds = append(filter.Kinds, kind)
	}
	for _, value := range queryValues(r, "status") {
		status := models.AgentRequestStatus(value)
		if !validAgentRequestStatus(status) {
			return models.AgentRequestFilter{}, fmt.Errorf("invalid agent request status %q", value)
		}
		filter.Statuses = append(filter.Statuses, status)
	}
	return filter, nil
}

func collaborationPartyFromQuery(r *http.Request, prefix string) (*models.CollaborationParty, error) {
	typeValue := strings.TrimSpace(r.URL.Query().Get(prefix + "Type"))
	id := strings.TrimSpace(r.URL.Query().Get(prefix + "Id"))
	if typeValue == "" && id == "" {
		return nil, nil
	}
	party := models.CollaborationParty{Type: models.OwnerType(typeValue), ID: id}
	if typeValue == "" || id == "" {
		return nil, fmt.Errorf("%sType and %sId must be provided together", prefix, prefix)
	}
	if err := party.Validate(); err != nil {
		return nil, err
	}
	return &party, nil
}

func validAgentRequestStatus(status models.AgentRequestStatus) bool {
	switch status {
	case models.AgentRequestStatusPending, models.AgentRequestStatusClarificationRequested, models.AgentRequestStatusAccepted,
		models.AgentRequestStatusCompletionReview, models.AgentRequestStatusCompleted, models.AgentRequestStatusFailed,
		models.AgentRequestStatusRejected, models.AgentRequestStatusCanceled:
		return true
	default:
		return false
	}
}

func (s *Server) respondAgentRequestError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, models.ErrRunNotFound), errors.Is(err, models.ErrAgentRequestNotFound), errors.Is(err, artifactcatalog.ErrArtifactNotFound):
		s.respondError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, collaboration.ErrAgentRequestUnauthorized):
		s.respondError(w, http.StatusForbidden, err.Error())
	case errors.Is(err, models.ErrRevisionConflict), errors.Is(err, models.ErrInvalidAgentRequestState),
		errors.Is(err, models.ErrAgentRequestIdempotency), errors.Is(err, models.ErrBudgetExhausted):
		s.respondError(w, http.StatusConflict, err.Error())
	case errors.Is(err, models.ErrAgentRequestAssignment), errors.Is(err, models.ErrUnsafeSharedContext), errors.Is(err, models.ErrInvalidArtifact),
		errors.Is(err, models.ErrInvalidScope), errors.Is(err, models.ErrInvalidOwner):
		s.respondError(w, http.StatusBadRequest, err.Error())
	case strings.Contains(err.Error(), "required") || strings.Contains(err.Error(), "must be") || strings.Contains(err.Error(), "invalid"):
		s.respondError(w, http.StatusBadRequest, err.Error())
	default:
		s.logger.Errorw("agent request API failed", "error", err)
		s.respondError(w, http.StatusInternalServerError, "agent request operation failed")
	}
}
