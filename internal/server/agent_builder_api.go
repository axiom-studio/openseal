package server

import (
	"context"
	"errors"
	"net/http"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/kernelapi"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentbuilder"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/conversationrun"
)

// localBuilderRequester is the authenticated local operator: the requester of
// builder conversations started through the daemon and the viewer of drafts.
var localBuilderRequester = models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "local-operator"}

// agentBuilderWriter creates through the conversation service and posts
// through the host dispatcher when one schedules Runs.
type agentBuilderWriter struct {
	service *conversation.ConversationService
	post    func(context.Context, conversation.PostChannelMessageRequest) (*models.ChannelMessageCommitResult, error)
}

func (w agentBuilderWriter) CreateConversation(ctx context.Context, request conversation.CreateConversationRequest) (*models.Conversation, bool, error) {
	return w.service.CreateConversation(ctx, request)
}

func (w agentBuilderWriter) PostChannelMessage(ctx context.Context, request conversation.PostChannelMessageRequest) (*models.ChannelMessageCommitResult, error) {
	return w.post(ctx, request)
}

type agentBuilderReader struct {
	service *conversation.ConversationService
	store   models.KernelStore
}

func (r agentBuilderReader) ListConversations(ctx context.Context, filter models.ConversationFilter) ([]*models.Conversation, error) {
	return r.service.ListConversations(ctx, filter)
}

func (r agentBuilderReader) ListChannelMessages(ctx context.Context, filter models.ChannelMessageFilter) ([]*models.ChannelMessage, error) {
	return r.service.ListChannelMessages(ctx, filter)
}

func (r agentBuilderReader) ListAgentRuns(ctx context.Context, filter models.AgentRunFilter) ([]*models.AgentRun, error) {
	return r.store.ListAgentRuns(ctx, filter)
}

func (r agentBuilderReader) ListApprovals(ctx context.Context, filter models.ApprovalFilter) ([]*models.ApprovalCheckpoint, error) {
	return r.store.ListApprovals(ctx, filter)
}

func (s *Server) handleStartAgentBuilderConversation(w http.ResponseWriter, r *http.Request) {
	service, ok := s.conversationService()
	if !ok {
		s.respondError(w, http.StatusNotImplemented, "builder conversations are unavailable")
		return
	}
	var payload kernelapi.StartAgentBuilderConversationRequest
	if err := decodeStrictJSON(r, &payload); err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !s.authorizeDesktopChannelWrite(w, payload.Scope, nil) {
		return
	}
	if payload.Parameters.Draft != nil {
		s.respondError(w, http.StatusBadRequest, "builder parameters cannot carry a draft")
		return
	}
	if !s.agentBuilderAvailable(r.Context(), payload.Scope) {
		s.respondError(w, http.StatusNotFound, "the agent builder is not set up in this workspace")
		return
	}
	post := service.PostChannelMessage
	if s.channelMessageDispatcher != nil {
		post = s.channelMessageDispatcher
	}
	result, err := agentbuilder.StartAgentBuilderConversation(r.Context(), agentBuilderWriter{service: service, post: post}, models.StartAgentBuilderRequest{
		Scope: payload.Scope, Requester: localBuilderRequester, Prompt: payload.Prompt, Parameters: payload.Parameters,
		SpaceID: payload.SpaceID, IdempotencyKey: requestIdempotencyKey(r, payload.IdempotencyKey),
	})
	if err != nil {
		s.respondConversationError(w, err)
		return
	}
	status := http.StatusCreated
	if result.Replayed {
		status = http.StatusOK
	}
	s.respondJSON(w, status, kernelapi.StartAgentBuilderConversationResponse{
		Conversation: result.Conversation, Message: result.Message, Replayed: result.Replayed,
	})
}

func (s *Server) handleListAgentBuilderDrafts(w http.ResponseWriter, r *http.Request) {
	service, ok := s.conversationService()
	if !ok {
		s.respondError(w, http.StatusNotImplemented, "builder conversations are unavailable")
		return
	}
	scope, err := scopeFromQuery(r)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !s.authorizeDesktopChannelWrite(w, scope, nil) {
		return
	}
	limit, err := boundedIntQuery(r, "limit", 50, 1, 100)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	offset, err := boundedIntQuery(r, "offset", 0, 0, 1_000_000)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	drafts, err := agentbuilder.ListAgentBuilderDrafts(r.Context(), agentBuilderReader{service: service, store: s.store}, agentbuilder.AgentBuilderDraftFilter{
		Scope: scope, Requester: localBuilderRequester,
		Principal: models.ApprovalPrincipal{Type: string(localBuilderRequester.Type), ID: localBuilderRequester.ID},
		Limit:     limit, Offset: offset,
	})
	if err != nil {
		s.respondConversationError(w, err)
		return
	}
	s.respondJSON(w, http.StatusOK, kernelapi.ListAgentBuilderDraftsResponse{Drafts: drafts})
}

// agentBuilderCapabilityScope is the scope whose builder the capability
// document describes: the desktop workspace, else the query's scope.
func (s *Server) agentBuilderCapabilityScope(r *http.Request) (models.Scope, bool) {
	if s.desktopConversationScope != nil {
		return *s.desktopConversationScope, true
	}
	scope, err := scopeFromQuery(r)
	return scope, err == nil
}

// agentBuilderAvailable reports whether the builder Agent is deployed in scope
// and the store keeps conversations.
func (s *Server) agentBuilderAvailable(ctx context.Context, scope models.Scope) bool {
	if _, ok := s.store.(models.ConversationStore); !ok {
		return false
	}
	agents, ok := s.store.(kernelagent.Store)
	if !ok {
		return false
	}
	deployment, err := agents.GetDeployment(ctx, capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}, conversationrun.AgentBuilderDeploymentID)
	if err != nil {
		if !errors.Is(err, kernelagent.ErrDeploymentNotFound) {
			s.logger.Warnw("look up the agent builder", "error", err)
		}
		return false
	}
	return deployment != nil
}
