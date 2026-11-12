package server

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/kernelapi"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
)

func (s *Server) conversationService() (*conversation.ConversationService, bool) {
	store, ok := s.store.(models.ConversationStore)
	if !ok {
		return nil, false
	}
	return conversation.NewConversationService(store), true
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
	if !s.authorizeDesktopChannelWrite(w, payload.Scope, nil) {
		return
	}
	key := requestIdempotencyKey(r, payload.IdempotencyKey)
	conversation, replayed, err := service.CreateConversation(r.Context(), conversation.CreateConversationRequest{
		ID: payload.ID, Scope: payload.Scope, Owner: payload.Owner, Title: payload.Title, Origin: payload.Origin,
		SpaceID: payload.SpaceID, CreatedBy: localBuilderRequester.ID, IdempotencyKey: key,
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
	if r.URL.Query().Has("participantType") || r.URL.Query().Has("participantId") {
		participant, err := participantFromQuery(r)
		if err != nil {
			s.respondError(w, http.StatusBadRequest, err.Error())
			return
		}
		type readPosition struct {
			Participant  models.ConversationParticipant `json:"participant"`
			ReadSequence int64                          `json:"readSequence"`
			Revision     int64                          `json:"revision"`
		}
		type conversationWithReadPosition struct {
			*models.Conversation
			ReadPosition readPosition `json:"readPosition"`
		}
		result := make([]conversationWithReadPosition, 0, len(values))
		for _, value := range values {
			cursor, err := service.GetCursor(r.Context(), filter.Scope, value.ID, participant)
			if err != nil {
				s.respondConversationError(w, err)
				return
			}
			position := readPosition{Participant: participant}
			if cursor != nil {
				position.ReadSequence = cursor.ReadSequence
				position.Revision = cursor.Revision
			}
			result = append(result, conversationWithReadPosition{Conversation: value, ReadPosition: position})
		}
		s.respondJSON(w, http.StatusOK, result)
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

func (s *Server) handleUpdateConversation(w http.ResponseWriter, r *http.Request) {
	service, _, conversationID, ok := s.conversationRequestContextFromBodyStore(w, r)
	if !ok {
		return
	}
	var payload kernelapi.UpdateConversationRequest
	if err := decodeStrictJSON(r, &payload); err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !s.authorizeDesktopChannelWrite(w, payload.Scope, nil) {
		return
	}
	if payload.Title == nil && payload.Status == nil && payload.ParticipationEnabled == nil {
		s.respondError(w, http.StatusBadRequest, "a channel title, status, or participation change is required")
		return
	}
	if payload.ParticipationEnabled != nil {
		if s.channelParticipationAuthorizer == nil {
			s.respondError(w, http.StatusForbidden, "channel participation settings are not available on this host")
			return
		}
		current, err := service.GetConversation(r.Context(), payload.Scope, conversationID)
		if err != nil {
			s.respondConversationError(w, err)
			return
		}
		if current.Revision != payload.ExpectedRevision {
			s.respondConversationError(w, models.ErrRevisionConflict)
			return
		}
		if *payload.ParticipationEnabled {
			if err := s.channelParticipationAuthorizer(r.Context(), current); err != nil {
				s.respondError(w, http.StatusUnprocessableEntity, err.Error())
				return
			}
		}
	}
	result, err := service.UpdateConversation(r.Context(), conversation.UpdateConversationRequest{
		Scope: payload.Scope, ConversationID: conversationID, ExpectedRevision: payload.ExpectedRevision, Title: payload.Title, Status: payload.Status, ParticipationEnabled: payload.ParticipationEnabled,
	})
	if err != nil {
		s.respondConversationError(w, err)
		return
	}
	s.respondJSON(w, http.StatusOK, result)
}

func (s *Server) conversationApprovalModeSupported() bool {
	_, ok := s.store.(models.ConversationApprovalModeKernelStore)
	return ok
}

// handleSetConversationApprovalMode is the governed approval mode update. The
// embedding host authenticates the caller and authorizes it to execute the
// conversation's Agent before forwarding the request with that actor.
func (s *Server) handleSetConversationApprovalMode(w http.ResponseWriter, r *http.Request) {
	store, ok := s.store.(models.ConversationApprovalModeKernelStore)
	if !ok {
		s.respondError(w, http.StatusNotImplemented, "conversation approval modes are unavailable")
		return
	}
	var payload kernelapi.SetConversationApprovalModeRequest
	if err := decodeStrictJSON(r, &payload); err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	actor := models.ConversationParticipant{Type: models.ConversationParticipantType(strings.TrimSpace(payload.Actor.Type)), ID: strings.TrimSpace(payload.Actor.ID)}
	if !s.authorizeDesktopChannelWrite(w, payload.Scope, &actor) {
		return
	}
	result, err := conversation.NewConversationApprovalModeService(store).SetApprovalMode(r.Context(), conversation.SetConversationApprovalModeRequest{
		Scope: payload.Scope, ConversationID: strings.TrimSpace(r.PathValue("id")), ExpectedRevision: payload.ExpectedRevision,
		Mode: payload.Mode, Actor: payload.Actor, CorrelationID: strings.TrimSpace(r.Header.Get("X-Correlation-ID")),
	})
	if err != nil {
		s.respondConversationError(w, err)
		return
	}
	s.respondJSON(w, http.StatusOK, result)
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
	if !s.authorizeDesktopChannelWrite(w, payload.Scope, &payload.Sender) {
		return
	}
	key := requestIdempotencyKey(r, payload.IdempotencyKey)
	post := service.PostChannelMessage
	if s.channelMessageDispatcher != nil {
		post = s.channelMessageDispatcher
	}
	result, err := post(r.Context(), conversation.PostChannelMessageRequest{
		ID: payload.ID, Scope: payload.Scope, ConversationID: conversationID, ExpectedRevision: payload.ExpectedRevision,
		Sender: payload.Sender, Intent: payload.Intent, Content: payload.Content, Audience: payload.Audience,
		ReplyToMessageID: payload.ReplyToMessageID, QuotedMessageID: payload.QuotedMessageID, BroadcastToChannel: payload.BroadcastToChannel,
		Mentions: payload.Mentions, References: payload.References,
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
	before, err := boundedIntQuery(r, "beforeSequence", 0, 0, 1_000_000_000)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	filter := models.ChannelMessageFilter{
		Scope: scope, ConversationID: conversationID, ThreadRootID: strings.TrimSpace(r.URL.Query().Get("threadRootId")),
		AfterSequence: int64(after), BeforeSequence: int64(before), Limit: limit, Descending: strings.EqualFold(r.URL.Query().Get("order"), "desc"),
	}
	if values, present := r.URL.Query()["channelTimeline"]; present {
		if len(values) != 1 || (values[0] != "true" && values[0] != "false") {
			s.respondError(w, http.StatusBadRequest, "channelTimeline must be true or false")
			return
		}
		filter.ChannelTimeline = values[0] == "true"
	}
	for _, raw := range queryValues(r, "intent") {
		intent := models.ConversationMessageIntent(raw)
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

func (s *Server) handleListConversationChanges(w http.ResponseWriter, r *http.Request) {
	_, scope, conversationID, ok := s.conversationRequestContext(w, r)
	if !ok {
		return
	}
	conversationStore, ok := s.store.(models.ConversationStore)
	if !ok {
		s.respondError(w, http.StatusNotImplemented, "team channel changes are unavailable")
		return
	}
	limit, err := boundedIntQuery(r, "limit", 100, 1, 500)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	service, err := conversation.NewConversationChangeService(conversationStore, s.store)
	if err != nil {
		s.respondConversationError(w, err)
		return
	}
	changes, err := service.ListChanges(r.Context(), conversation.ConversationChangeRequest{
		Scope: scope, ConversationID: conversationID, Cursor: strings.TrimSpace(r.URL.Query().Get("cursor")), Limit: limit,
	})
	if err != nil {
		s.respondConversationError(w, err)
		return
	}
	s.respondJSON(w, http.StatusOK, changes)
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
	values, err := service.ListParticipationRounds(r.Context(), models.ParticipationRoundFilter{
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
	if !s.authorizeDesktopChannelWrite(w, payload.Scope, &payload.Participant) {
		return
	}
	cursor, replayed, err := service.AdvanceCursor(r.Context(), conversation.AdvanceConversationCursorRequest{
		Scope: payload.Scope, ConversationID: conversationID, Participant: payload.Participant,
		ExpectedRevision: payload.ExpectedRevision, DeliveredSequence: payload.DeliveredSequence, ReadSequence: payload.ReadSequence,
	})
	if err != nil {
		s.respondConversationError(w, err)
		return
	}
	s.respondJSON(w, http.StatusOK, struct {
		Cursor   *models.ConversationCursor `json:"cursor"`
		Replayed bool                       `json:"replayed"`
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
	presence, err := service.SetPresence(r.Context(), conversation.SetConversationPresenceRequest{
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
	err := service.ReleasePresence(r.Context(), models.ReleaseConversationPresenceRequest{
		Scope: payload.Scope, ConversationID: conversationID, Participant: payload.Participant, LeaseID: payload.LeaseID,
	})
	if err != nil {
		s.respondConversationError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAddMessageReaction(w http.ResponseWriter, r *http.Request) {
	s.handleMessageReaction(w, r, true)
}

func (s *Server) handleRemoveMessageReaction(w http.ResponseWriter, r *http.Request) {
	s.handleMessageReaction(w, r, false)
}

// handleMessageReaction applies reaction.add or reaction.remove for the
// authenticated actor. Both are idempotent; the result says whether the
// reaction changed and carries the message's current reactions.
func (s *Server) handleMessageReaction(w http.ResponseWriter, r *http.Request, add bool) {
	service, _, conversationID, ok := s.conversationRequestContextFromBodyStore(w, r)
	if !ok {
		return
	}
	var payload kernelapi.MessageReactionRequest
	if err := decodeStrictJSON(r, &payload); err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !s.authorizeDesktopChannelWrite(w, payload.Scope, &payload.Actor) {
		return
	}
	service.SetReactionProjector(s.reactionProjector)
	change := service.RemoveMessageReaction
	if add {
		change = service.AddMessageReaction
	}
	result, err := change(r.Context(), conversation.MessageReactionRequest{
		Scope: payload.Scope, ConversationID: conversationID, MessageID: payload.MessageID, Emoji: payload.Emoji,
		Actor: payload.Actor, ActorRoles: payload.Roles,
	})
	if err != nil {
		s.respondConversationError(w, err)
		return
	}
	s.respondJSON(w, http.StatusOK, result)
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

func (s *Server) conversationRequestContext(w http.ResponseWriter, r *http.Request) (*conversation.ConversationService, models.Scope, string, bool) {
	service, ok := s.conversationService()
	if !ok {
		s.respondError(w, http.StatusNotImplemented, "team channels are unavailable")
		return nil, models.Scope{}, "", false
	}
	scope, err := scopeFromQuery(r)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return nil, models.Scope{}, "", false
	}
	return service, scope, strings.TrimSpace(r.PathValue("id")), true
}

func (s *Server) conversationRequestContextFromBodyStore(w http.ResponseWriter, r *http.Request) (*conversation.ConversationService, models.Scope, string, bool) {
	service, ok := s.conversationService()
	if !ok {
		s.respondError(w, http.StatusNotImplemented, "team channels are unavailable")
		return nil, models.Scope{}, "", false
	}
	return service, models.Scope{}, strings.TrimSpace(r.PathValue("id")), true
}

func conversationFilterFromQuery(r *http.Request) (models.ConversationFilter, error) {
	scope, err := scopeFromQuery(r)
	if err != nil {
		return models.ConversationFilter{}, err
	}
	limit, err := boundedIntQuery(r, "limit", 50, 1, 500)
	if err != nil {
		return models.ConversationFilter{}, err
	}
	offset, err := boundedIntQuery(r, "offset", 0, 0, 1_000_000)
	if err != nil {
		return models.ConversationFilter{}, err
	}
	filter := models.ConversationFilter{Scope: scope, Limit: limit, Offset: offset}
	ownerType, ownerID := strings.TrimSpace(r.URL.Query().Get("ownerType")), strings.TrimSpace(r.URL.Query().Get("ownerId"))
	if ownerType != "" || ownerID != "" {
		owner := models.ObjectiveOwner{Type: models.OwnerType(ownerType), ID: ownerID}
		if err := owner.Validate(); err != nil {
			return models.ConversationFilter{}, err
		}
		filter.Owner = &owner
	}
	filter.SpaceID = strings.TrimSpace(r.URL.Query().Get("spaceId"))
	for _, raw := range queryValues(r, "status") {
		status := models.ConversationStatus(raw)
		if status != models.ConversationStatusActive && status != models.ConversationStatusArchived {
			return models.ConversationFilter{}, fmt.Errorf("invalid conversation status %q", raw)
		}
		filter.Statuses = append(filter.Statuses, status)
	}
	return filter, nil
}

func participantFromQuery(r *http.Request) (models.ConversationParticipant, error) {
	participant := models.ConversationParticipant{
		Type: models.ConversationParticipantType(strings.TrimSpace(r.URL.Query().Get("participantType"))),
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

func validChannelIntent(intent models.ConversationMessageIntent) bool {
	switch intent {
	case models.MessageIntentQuestion, models.MessageIntentAnswer, models.MessageIntentUpdate, models.MessageIntentProposal,
		models.MessageIntentDecision, models.MessageIntentObjection, models.MessageIntentHandoff,
		models.MessageIntentApprovalRequest, models.MessageIntentAcknowledgment, models.MessageIntentSystem:
		return true
	default:
		return false
	}
}

func (s *Server) respondConversationError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, models.ErrConversationNotFound), errors.Is(err, models.ErrChannelMessageNotFound),
		errors.Is(err, models.ErrParticipationRoundNotFound), errors.Is(err, models.ErrSpaceNotFound):
		s.respondError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, models.ErrMessageConflict), errors.Is(err, models.ErrRevisionConflict),
		errors.Is(err, models.ErrConversationCursorConflict), errors.Is(err, models.ErrConversationPresenceConflict),
		errors.Is(err, models.ErrNoDefaultSpace):
		s.respondError(w, http.StatusConflict, err.Error())
	case errors.Is(err, models.ErrInvalidConversation), errors.Is(err, models.ErrInvalidScope), errors.Is(err, models.ErrInvalidOwner):
		s.respondError(w, http.StatusBadRequest, err.Error())
	default:
		s.logger.Errorw("team channel API failed", "error", err)
		s.respondError(w, http.StatusInternalServerError, "team channel operation failed")
	}
}

func (s *Server) authorizeDesktopChannelWrite(w http.ResponseWriter, scope models.Scope, sender *models.ConversationParticipant) bool {
	if s.desktopConversationScope == nil {
		return true
	}
	if s.desktopConversationScope.Kind != "local" || scope != *s.desktopConversationScope ||
		(sender != nil && *sender != (models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "local-operator"})) {
		s.respondError(w, http.StatusForbidden, "desktop channel changes require the configured local workspace operator")
		return false
	}
	return true
}
