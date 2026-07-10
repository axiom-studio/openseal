package server

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/kernelapi"
	"github.com/axiom-studio/openseal/pkg/runtime"
)

func (s *Server) conversationService() (*runtime.ConversationService, bool) {
	store, ok := s.store.(runtime.ConversationStore)
	if !ok {
		return nil, false
	}
	return runtime.NewConversationService(store), true
}

func (s *Server) handleCreateConversation(w http.ResponseWriter, r *http.Request) {
	service, ok := s.conversationService()
	if !ok {
		s.respondError(w, http.StatusNotImplemented, "team channels are unavailable")
		return
	}
	var payload kernelapi.CreateConversationRequest
	if err := decodeStrictJSON(r, &payload); err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	key := requestIdempotencyKey(r, payload.IdempotencyKey)
	conversation, replayed, err := service.CreateConversation(r.Context(), runtime.CreateConversationRequest{
		ID: payload.ID, Scope: payload.Scope, Owner: payload.Owner, Title: payload.Title, IdempotencyKey: key,
	})
	if err != nil {
		s.respondConversationError(w, err)
		return
	}
	status := http.StatusCreated
	if replayed {
		status = http.StatusOK
	}
	s.respondJSON(w, status, conversation)
}

func (s *Server) handleListConversations(w http.ResponseWriter, r *http.Request) {
	service, ok := s.conversationService()
	if !ok {
		s.respondError(w, http.StatusNotImplemented, "team channels are unavailable")
		return
	}
	filter, err := conversationFilterFromQuery(r)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	values, err := service.ListConversations(r.Context(), filter)
	if err != nil {
		s.respondConversationError(w, err)
		return
	}
	s.respondJSON(w, http.StatusOK, values)
}

func (s *Server) handleGetConversation(w http.ResponseWriter, r *http.Request) {
	service, scope, conversationID, ok := s.conversationRequestContext(w, r)
	if !ok {
		return
	}
	value, err := service.GetConversation(r.Context(), scope, conversationID)
	if err != nil {
		s.respondConversationError(w, err)
		return
	}
	s.respondJSON(w, http.StatusOK, value)
}

func (s *Server) handlePostChannelMessage(w http.ResponseWriter, r *http.Request) {
	service, _, conversationID, ok := s.conversationRequestContextFromBodyStore(w, r)
	if !ok {
		return
	}
	var payload kernelapi.PostChannelMessageRequest
	if err := decodeStrictJSON(r, &payload); err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	key := requestIdempotencyKey(r, payload.IdempotencyKey)
	result, err := service.PostChannelMessage(r.Context(), runtime.PostChannelMessageRequest{
		ID: payload.ID, Scope: payload.Scope, ConversationID: conversationID, ExpectedRevision: payload.ExpectedRevision,
		Sender: payload.Sender, Intent: payload.Intent, Content: payload.Content, Audience: payload.Audience,
		ReplyToMessageID: payload.ReplyToMessageID, Mentions: payload.Mentions, References: payload.References,
		RequiresResponse: payload.RequiresResponse, ResolvesMessageID: payload.ResolvesMessageID,
		SupersedesMessageID: payload.SupersedesMessageID, IdempotencyKey: key,
	})
	if err != nil {
		s.respondConversationError(w, err)
		return
	}
	status := http.StatusCreated
	if result.Replayed {
		status = http.StatusOK
	}
	s.respondJSON(w, status, result)
}

func (s *Server) handleListChannelMessages(w http.ResponseWriter, r *http.Request) {
	service, scope, conversationID, ok := s.conversationRequestContext(w, r)
	if !ok {
		return
	}
	limit, err := boundedIntQuery(r, "limit", 100, 1, 500)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	after, err := boundedIntQuery(r, "afterSequence", 0, 0, 1_000_000_000)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	filter := runtime.ChannelMessageFilter{
		Scope: scope, ConversationID: conversationID, ThreadRootID: strings.TrimSpace(r.URL.Query().Get("threadRootId")),
		AfterSequence: int64(after), Limit: limit, Descending: strings.EqualFold(r.URL.Query().Get("order"), "desc"),
	}
	for _, raw := range queryValues(r, "intent") {
		intent := runtime.ConversationMessageIntent(raw)
		if !validChannelIntent(intent) {
			s.respondError(w, http.StatusBadRequest, fmt.Sprintf("invalid message intent %q", raw))
			return
		}
		filter.Intents = append(filter.Intents, intent)
	}
	values, err := service.ListChannelMessages(r.Context(), filter)
	if err != nil {
		s.respondConversationError(w, err)
		return
	}
	s.respondJSON(w, http.StatusOK, values)
}

func (s *Server) handleGetChannelMessage(w http.ResponseWriter, r *http.Request) {
	service, scope, conversationID, ok := s.conversationRequestContext(w, r)
	if !ok {
		return
	}
	value, err := service.GetChannelMessage(r.Context(), scope, conversationID, strings.TrimSpace(r.PathValue("messageId")))
	if err != nil {
		s.respondConversationError(w, err)
		return
	}
	s.respondJSON(w, http.StatusOK, value)
}

func (s *Server) handleCoordinateParticipation(w http.ResponseWriter, r *http.Request) {
	service, _, conversationID, ok := s.conversationRequestContextFromBodyStore(w, r)
	if !ok {
		return
	}
	var payload kernelapi.CoordinateParticipationRequest
	if err := decodeStrictJSON(r, &payload); err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	key := requestIdempotencyKey(r, payload.IdempotencyKey)
	result, err := service.CoordinateParticipation(r.Context(), runtime.CoordinateParticipationRequest{
		ID: payload.ID, Scope: payload.Scope, ConversationID: conversationID, ExpectedRevision: payload.ExpectedRevision,
		TriggerMessageID: payload.TriggerMessageID, Policy: payload.Policy, Proposals: payload.Proposals, IdempotencyKey: key,
	})
	if err != nil {
		s.respondConversationError(w, err)
		return
	}
	status := http.StatusCreated
	if result.Replayed {
		status = http.StatusOK
	}
	s.respondJSON(w, status, result)
}

func (s *Server) handleGetParticipationRound(w http.ResponseWriter, r *http.Request) {
	service, scope, conversationID, ok := s.conversationRequestContext(w, r)
	if !ok {
		return
	}
	result, err := service.GetParticipationRound(r.Context(), scope, conversationID, strings.TrimSpace(r.PathValue("roundId")))
	if err != nil {
		s.respondConversationError(w, err)
		return
	}
	s.respondJSON(w, http.StatusOK, result)
}

func (s *Server) handleListParticipationRounds(w http.ResponseWriter, r *http.Request) {
	service, scope, conversationID, ok := s.conversationRequestContext(w, r)
	if !ok {
		return
	}
	limit, err := boundedIntQuery(r, "limit", 50, 1, 500)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	offset, err := boundedIntQuery(r, "offset", 0, 0, 1_000_000)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	values, err := service.ListParticipationRounds(r.Context(), runtime.ParticipationRoundFilter{
		Scope: scope, ConversationID: conversationID, Limit: limit, Offset: offset,
	})
	if err != nil {
		s.respondConversationError(w, err)
		return
	}
	s.respondJSON(w, http.StatusOK, values)
}

func (s *Server) handleAdvanceConversationCursor(w http.ResponseWriter, r *http.Request) {
	service, _, conversationID, ok := s.conversationRequestContextFromBodyStore(w, r)
	if !ok {
		return
	}
	var payload kernelapi.AdvanceConversationCursorRequest
	if err := decodeStrictJSON(r, &payload); err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	cursor, replayed, err := service.AdvanceCursor(r.Context(), runtime.AdvanceConversationCursorRequest{
		Scope: payload.Scope, ConversationID: conversationID, Participant: payload.Participant,
		ExpectedRevision: payload.ExpectedRevision, DeliveredSequence: payload.DeliveredSequence, ReadSequence: payload.ReadSequence,
	})
	if err != nil {
		s.respondConversationError(w, err)
		return
	}
	s.respondJSON(w, http.StatusOK, struct {
		Cursor   *runtime.ConversationCursor `json:"cursor"`
		Replayed bool                        `json:"replayed"`
	}{Cursor: cursor, Replayed: replayed})
}

func (s *Server) handleGetConversationCursor(w http.ResponseWriter, r *http.Request) {
	service, scope, conversationID, ok := s.conversationRequestContext(w, r)
	if !ok {
		return
	}
	participant, err := participantFromQuery(r)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	cursor, err := service.GetCursor(r.Context(), scope, conversationID, participant)
	if err != nil {
		s.respondConversationError(w, err)
		return
	}
	if cursor == nil {
		s.respondError(w, http.StatusNotFound, "conversation cursor not found")
		return
	}
	s.respondJSON(w, http.StatusOK, cursor)
}

func (s *Server) handleSetConversationPresence(w http.ResponseWriter, r *http.Request) {
	service, _, conversationID, ok := s.conversationRequestContextFromBodyStore(w, r)
	if !ok {
		return
	}
	var payload kernelapi.SetConversationPresenceRequest
	if err := decodeStrictJSON(r, &payload); err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	presence, err := service.SetPresence(r.Context(), runtime.SetConversationPresenceRequest{
		Scope: payload.Scope, ConversationID: conversationID, Participant: payload.Participant, State: payload.State,
		Summary: payload.Summary, RunID: payload.RunID, LeaseID: payload.LeaseID, ExpectedRevision: payload.ExpectedRevision,
		TTL: time.Duration(payload.TTLSeconds) * time.Second,
	})
	if err != nil {
		s.respondConversationError(w, err)
		return
	}
	s.respondJSON(w, http.StatusOK, presence)
}

func (s *Server) handleReleaseConversationPresence(w http.ResponseWriter, r *http.Request) {
	service, _, conversationID, ok := s.conversationRequestContextFromBodyStore(w, r)
	if !ok {
		return
	}
	var payload kernelapi.ReleaseConversationPresenceRequest
	if err := decodeStrictJSON(r, &payload); err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	err := service.ReleasePresence(r.Context(), runtime.ReleaseConversationPresenceRequest{
		Scope: payload.Scope, ConversationID: conversationID, Participant: payload.Participant, LeaseID: payload.LeaseID,
	})
	if err != nil {
		s.respondConversationError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListConversationPresence(w http.ResponseWriter, r *http.Request) {
	service, scope, conversationID, ok := s.conversationRequestContext(w, r)
	if !ok {
		return
	}
	values, err := service.ListPresence(r.Context(), scope, conversationID)
	if err != nil {
		s.respondConversationError(w, err)
		return
	}
	s.respondJSON(w, http.StatusOK, values)
}

func (s *Server) conversationRequestContext(w http.ResponseWriter, r *http.Request) (*runtime.ConversationService, runtime.Scope, string, bool) {
	service, ok := s.conversationService()
	if !ok {
		s.respondError(w, http.StatusNotImplemented, "team channels are unavailable")
		return nil, runtime.Scope{}, "", false
	}
	scope, err := scopeFromQuery(r)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return nil, runtime.Scope{}, "", false
	}
	return service, scope, strings.TrimSpace(r.PathValue("id")), true
}

func (s *Server) conversationRequestContextFromBodyStore(w http.ResponseWriter, r *http.Request) (*runtime.ConversationService, runtime.Scope, string, bool) {
	service, ok := s.conversationService()
	if !ok {
		s.respondError(w, http.StatusNotImplemented, "team channels are unavailable")
		return nil, runtime.Scope{}, "", false
	}
	return service, runtime.Scope{}, strings.TrimSpace(r.PathValue("id")), true
}

func conversationFilterFromQuery(r *http.Request) (runtime.ConversationFilter, error) {
	scope, err := scopeFromQuery(r)
	if err != nil {
		return runtime.ConversationFilter{}, err
	}
	limit, err := boundedIntQuery(r, "limit", 50, 1, 500)
	if err != nil {
		return runtime.ConversationFilter{}, err
	}
	offset, err := boundedIntQuery(r, "offset", 0, 0, 1_000_000)
	if err != nil {
		return runtime.ConversationFilter{}, err
	}
	filter := runtime.ConversationFilter{Scope: scope, Limit: limit, Offset: offset}
	ownerType, ownerID := strings.TrimSpace(r.URL.Query().Get("ownerType")), strings.TrimSpace(r.URL.Query().Get("ownerId"))
	if ownerType != "" || ownerID != "" {
		owner := runtime.ObjectiveOwner{Type: runtime.OwnerType(ownerType), ID: ownerID}
		if err := owner.Validate(); err != nil {
			return runtime.ConversationFilter{}, err
		}
		filter.Owner = &owner
	}
	for _, raw := range queryValues(r, "status") {
		status := runtime.ConversationStatus(raw)
		if status != runtime.ConversationStatusActive && status != runtime.ConversationStatusArchived {
			return runtime.ConversationFilter{}, fmt.Errorf("invalid conversation status %q", raw)
		}
		filter.Statuses = append(filter.Statuses, status)
	}
	return filter, nil
}

func participantFromQuery(r *http.Request) (runtime.ConversationParticipant, error) {
	participant := runtime.ConversationParticipant{
		Type: runtime.ConversationParticipantType(strings.TrimSpace(r.URL.Query().Get("participantType"))),
		ID:   strings.TrimSpace(r.URL.Query().Get("participantId")),
	}
	return participant, participant.Validate()
}

func requestIdempotencyKey(r *http.Request, fallback string) string {
	if key := strings.TrimSpace(r.Header.Get("Idempotency-Key")); key != "" {
		return key
	}
	return strings.TrimSpace(fallback)
}

func validChannelIntent(intent runtime.ConversationMessageIntent) bool {
	switch intent {
	case runtime.MessageIntentQuestion, runtime.MessageIntentAnswer, runtime.MessageIntentUpdate, runtime.MessageIntentProposal,
		runtime.MessageIntentDecision, runtime.MessageIntentObjection, runtime.MessageIntentHandoff,
		runtime.MessageIntentApprovalRequest, runtime.MessageIntentAcknowledgment, runtime.MessageIntentSystem:
		return true
	default:
		return false
	}
}

func (s *Server) respondConversationError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, runtime.ErrConversationNotFound), errors.Is(err, runtime.ErrChannelMessageNotFound),
		errors.Is(err, runtime.ErrParticipationRoundNotFound):
		s.respondError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, runtime.ErrMessageConflict), errors.Is(err, runtime.ErrRevisionConflict),
		errors.Is(err, runtime.ErrConversationCursorConflict), errors.Is(err, runtime.ErrConversationPresenceConflict):
		s.respondError(w, http.StatusConflict, err.Error())
	case errors.Is(err, runtime.ErrInvalidConversation), errors.Is(err, runtime.ErrInvalidScope), errors.Is(err, runtime.ErrInvalidOwner):
		s.respondError(w, http.StatusBadRequest, err.Error())
	default:
		s.logger.Errorw("team channel API failed", "error", err)
		s.respondError(w, http.StatusInternalServerError, "team channel operation failed")
	}
}
