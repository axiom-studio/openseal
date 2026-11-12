package client

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/axiom-studio/openseal/pkg/kernelapi"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
)

type ConversationClient interface {
	CreateConversation(context.Context, kernelapi.CreateConversationRequest, string) (*models.Conversation, error)
	ListConversations(context.Context, models.ConversationFilter) ([]*models.Conversation, error)
	GetConversation(context.Context, models.Scope, string) (*models.Conversation, error)
	UpdateConversation(context.Context, string, kernelapi.UpdateConversationRequest) (*models.Conversation, error)
	PostChannelMessage(context.Context, string, kernelapi.PostChannelMessageRequest, string) (*models.ChannelMessageCommitResult, error)
	ListChannelMessages(context.Context, models.ChannelMessageFilter) ([]*models.ChannelMessage, error)
	GetChannelMessage(context.Context, models.Scope, string, string) (*models.ChannelMessage, error)
	GetParticipationRound(context.Context, models.Scope, string, string) (*models.ParticipationRoundResult, error)
	ListParticipationRounds(context.Context, models.ParticipationRoundFilter) ([]*models.ParticipationRoundResult, error)
	AdvanceConversationCursor(context.Context, string, kernelapi.AdvanceConversationCursorRequest) (*models.ConversationCursor, bool, error)
	GetConversationCursor(context.Context, models.Scope, string, models.ConversationParticipant) (*models.ConversationCursor, error)
	SetConversationPresence(context.Context, string, kernelapi.SetConversationPresenceRequest) (*models.ConversationPresence, error)
	ReleaseConversationPresence(context.Context, string, kernelapi.ReleaseConversationPresenceRequest) error
	ListConversationPresence(context.Context, models.Scope, string) ([]*models.ConversationPresence, error)
}

func (c *KernelHTTPClient) CreateConversation(ctx context.Context, request kernelapi.CreateConversationRequest, idempotencyKey string) (*models.Conversation, error) {
	var result models.Conversation
	if err := c.do(ctx, http.MethodPost, "/api/v1/conversations", request, idempotencyKey, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *KernelHTTPClient) ListConversations(ctx context.Context, filter models.ConversationFilter) ([]*models.Conversation, error) {
	query := scopeQuery(filter.Scope)
	if filter.Owner != nil {
		query.Set("ownerType", string(filter.Owner.Type))
		query.Set("ownerId", filter.Owner.ID)
	}
	for _, status := range filter.Statuses {
		query.Add("status", string(status))
	}
	setPage(query, filter.Limit, filter.Offset)
	var result []*models.Conversation
	if err := c.do(ctx, http.MethodGet, "/api/v1/conversations?"+query.Encode(), nil, "", &result); err != nil {
		return nil, err
	}
	return result, nil
}

func (c *KernelHTTPClient) GetConversation(ctx context.Context, scope models.Scope, conversationID string) (*models.Conversation, error) {
	var result models.Conversation
	path := conversationPath(conversationID) + "?" + scopeQuery(scope).Encode()
	if err := c.do(ctx, http.MethodGet, path, nil, "", &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *KernelHTTPClient) UpdateConversation(ctx context.Context, conversationID string, request kernelapi.UpdateConversationRequest) (*models.Conversation, error) {
	var result models.Conversation
	if err := c.do(ctx, http.MethodPatch, conversationPath(conversationID), request, "", &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *KernelHTTPClient) SetConversationApprovalMode(ctx context.Context, conversationID string, request kernelapi.SetConversationApprovalModeRequest) (*conversation.SetConversationApprovalModeResult, error) {
	var result conversation.SetConversationApprovalModeResult
	if err := c.do(ctx, http.MethodPatch, conversationPath(conversationID)+"/approval-mode", request, "", &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *KernelHTTPClient) PostChannelMessage(ctx context.Context, conversationID string, request kernelapi.PostChannelMessageRequest, idempotencyKey string) (*models.ChannelMessageCommitResult, error) {
	var result models.ChannelMessageCommitResult
	if err := c.do(ctx, http.MethodPost, conversationPath(conversationID)+"/messages", request, idempotencyKey, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *KernelHTTPClient) ListChannelMessages(ctx context.Context, filter models.ChannelMessageFilter) ([]*models.ChannelMessage, error) {
	query := scopeQuery(filter.Scope)
	setIfPresent(query, "threadRootId", filter.ThreadRootID)
	if filter.ChannelTimeline {
		query.Set("channelTimeline", "true")
	}
	if filter.AfterSequence > 0 {
		query.Set("afterSequence", strconv.FormatInt(filter.AfterSequence, 10))
	}
	if filter.BeforeSequence > 0 {
		query.Set("beforeSequence", strconv.FormatInt(filter.BeforeSequence, 10))
	}
	for _, intent := range filter.Intents {
		query.Add("intent", string(intent))
	}
	if filter.Limit > 0 {
		query.Set("limit", strconv.Itoa(filter.Limit))
	}
	if filter.Descending {
		query.Set("order", "desc")
	}
	var result []*models.ChannelMessage
	path := conversationPath(filter.ConversationID) + "/messages?" + query.Encode()
	if err := c.do(ctx, http.MethodGet, path, nil, "", &result); err != nil {
		return nil, err
	}
	return result, nil
}

func (c *KernelHTTPClient) GetChannelMessage(ctx context.Context, scope models.Scope, conversationID, messageID string) (*models.ChannelMessage, error) {
	var result models.ChannelMessage
	path := conversationPath(conversationID) + "/messages/" + url.PathEscape(strings.TrimSpace(messageID)) + "?" + scopeQuery(scope).Encode()
	if err := c.do(ctx, http.MethodGet, path, nil, "", &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *KernelHTTPClient) GetParticipationRound(ctx context.Context, scope models.Scope, conversationID, roundID string) (*models.ParticipationRoundResult, error) {
	var result models.ParticipationRoundResult
	path := conversationPath(conversationID) + "/participation-rounds/" + url.PathEscape(strings.TrimSpace(roundID)) + "?" + scopeQuery(scope).Encode()
	if err := c.do(ctx, http.MethodGet, path, nil, "", &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *KernelHTTPClient) ListParticipationRounds(ctx context.Context, filter models.ParticipationRoundFilter) ([]*models.ParticipationRoundResult, error) {
	query := scopeQuery(filter.Scope)
	setPage(query, filter.Limit, filter.Offset)
	var result []*models.ParticipationRoundResult
	path := conversationPath(filter.ConversationID) + "/participation-rounds?" + query.Encode()
	if err := c.do(ctx, http.MethodGet, path, nil, "", &result); err != nil {
		return nil, err
	}
	return result, nil
}

func (c *KernelHTTPClient) AdvanceConversationCursor(ctx context.Context, conversationID string, request kernelapi.AdvanceConversationCursorRequest) (*models.ConversationCursor, bool, error) {
	var result struct {
		Cursor   *models.ConversationCursor `json:"cursor"`
		Replayed bool                       `json:"replayed"`
	}
	if err := c.do(ctx, http.MethodPut, conversationPath(conversationID)+"/cursor", request, "", &result); err != nil {
		return nil, false, err
	}
	return result.Cursor, result.Replayed, nil
}

func (c *KernelHTTPClient) GetConversationCursor(ctx context.Context, scope models.Scope, conversationID string, participant models.ConversationParticipant) (*models.ConversationCursor, error) {
	query := scopeQuery(scope)
	query.Set("participantType", string(participant.Type))
	query.Set("participantId", participant.ID)
	var result models.ConversationCursor
	if err := c.do(ctx, http.MethodGet, conversationPath(conversationID)+"/cursor?"+query.Encode(), nil, "", &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *KernelHTTPClient) SetConversationPresence(ctx context.Context, conversationID string, request kernelapi.SetConversationPresenceRequest) (*models.ConversationPresence, error) {
	var result models.ConversationPresence
	if err := c.do(ctx, http.MethodPut, conversationPath(conversationID)+"/presence", request, "", &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *KernelHTTPClient) ReleaseConversationPresence(ctx context.Context, conversationID string, request kernelapi.ReleaseConversationPresenceRequest) error {
	return c.do(ctx, http.MethodDelete, conversationPath(conversationID)+"/presence", request, "", nil)
}

func (c *KernelHTTPClient) ListConversationPresence(ctx context.Context, scope models.Scope, conversationID string) ([]*models.ConversationPresence, error) {
	var result []*models.ConversationPresence
	if err := c.do(ctx, http.MethodGet, conversationPath(conversationID)+"/presence?"+scopeQuery(scope).Encode(), nil, "", &result); err != nil {
		return nil, err
	}
	return result, nil
}

// AddMessageReaction adds the actor's emoji to a channel message.
func (c *KernelHTTPClient) AddMessageReaction(ctx context.Context, conversationID string, request kernelapi.MessageReactionRequest) (*conversation.MessageReactionResult, error) {
	var result conversation.MessageReactionResult
	if err := c.do(ctx, http.MethodPut, conversationPath(conversationID)+"/reactions", request, "", &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// RemoveMessageReaction removes the actor's emoji from a channel message.
func (c *KernelHTTPClient) RemoveMessageReaction(ctx context.Context, conversationID string, request kernelapi.MessageReactionRequest) (*conversation.MessageReactionResult, error) {
	var result conversation.MessageReactionResult
	if err := c.do(ctx, http.MethodDelete, conversationPath(conversationID)+"/reactions", request, "", &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func conversationPath(conversationID string) string {
	return "/api/v1/conversations/" + url.PathEscape(strings.TrimSpace(conversationID))
}

func setPage(query url.Values, limit, offset int) {
	if limit > 0 {
		query.Set("limit", strconv.Itoa(limit))
	}
	if offset > 0 {
		query.Set("offset", strconv.Itoa(offset))
	}
}

var _ ConversationClient = (*KernelHTTPClient)(nil)
