package client

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/axiom-studio/openseal/pkg/kernelapi"
	"github.com/axiom-studio/openseal/pkg/runtime"
)

type ConversationClient interface {
	CreateConversation(context.Context, kernelapi.CreateConversationRequest, string) (*runtime.Conversation, error)
	ListConversations(context.Context, runtime.ConversationFilter) ([]*runtime.Conversation, error)
	GetConversation(context.Context, runtime.Scope, string) (*runtime.Conversation, error)
	UpdateConversation(context.Context, string, kernelapi.UpdateConversationRequest) (*runtime.Conversation, error)
	PostChannelMessage(context.Context, string, kernelapi.PostChannelMessageRequest, string) (*runtime.ChannelMessageCommitResult, error)
	ListChannelMessages(context.Context, runtime.ChannelMessageFilter) ([]*runtime.ChannelMessage, error)
	GetChannelMessage(context.Context, runtime.Scope, string, string) (*runtime.ChannelMessage, error)
	CoordinateParticipation(context.Context, string, kernelapi.CoordinateParticipationRequest, string) (*runtime.ParticipationRoundResult, error)
	GetParticipationRound(context.Context, runtime.Scope, string, string) (*runtime.ParticipationRoundResult, error)
	ListParticipationRounds(context.Context, runtime.ParticipationRoundFilter) ([]*runtime.ParticipationRoundResult, error)
	AdvanceConversationCursor(context.Context, string, kernelapi.AdvanceConversationCursorRequest) (*runtime.ConversationCursor, bool, error)
	GetConversationCursor(context.Context, runtime.Scope, string, runtime.ConversationParticipant) (*runtime.ConversationCursor, error)
	SetConversationPresence(context.Context, string, kernelapi.SetConversationPresenceRequest) (*runtime.ConversationPresence, error)
	ReleaseConversationPresence(context.Context, string, kernelapi.ReleaseConversationPresenceRequest) error
	ListConversationPresence(context.Context, runtime.Scope, string) ([]*runtime.ConversationPresence, error)
}

func (c *KernelHTTPClient) CreateConversation(ctx context.Context, request kernelapi.CreateConversationRequest, idempotencyKey string) (*runtime.Conversation, error) {
	var result runtime.Conversation
	if err := c.do(ctx, http.MethodPost, "/api/v1/conversations", request, idempotencyKey, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *KernelHTTPClient) ListConversations(ctx context.Context, filter runtime.ConversationFilter) ([]*runtime.Conversation, error) {
	query := scopeQuery(filter.Scope)
	if filter.Owner != nil {
		query.Set("ownerType", string(filter.Owner.Type))
		query.Set("ownerId", filter.Owner.ID)
	}
	for _, status := range filter.Statuses {
		query.Add("status", string(status))
	}
	setPage(query, filter.Limit, filter.Offset)
	var result []*runtime.Conversation
	if err := c.do(ctx, http.MethodGet, "/api/v1/conversations?"+query.Encode(), nil, "", &result); err != nil {
		return nil, err
	}
	return result, nil
}

func (c *KernelHTTPClient) GetConversation(ctx context.Context, scope runtime.Scope, conversationID string) (*runtime.Conversation, error) {
	var result runtime.Conversation
	path := conversationPath(conversationID) + "?" + scopeQuery(scope).Encode()
	if err := c.do(ctx, http.MethodGet, path, nil, "", &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *KernelHTTPClient) UpdateConversation(ctx context.Context, conversationID string, request kernelapi.UpdateConversationRequest) (*runtime.Conversation, error) {
	var result runtime.Conversation
	if err := c.do(ctx, http.MethodPatch, conversationPath(conversationID), request, "", &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *KernelHTTPClient) PostChannelMessage(ctx context.Context, conversationID string, request kernelapi.PostChannelMessageRequest, idempotencyKey string) (*runtime.ChannelMessageCommitResult, error) {
	var result runtime.ChannelMessageCommitResult
	if err := c.do(ctx, http.MethodPost, conversationPath(conversationID)+"/messages", request, idempotencyKey, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *KernelHTTPClient) ListChannelMessages(ctx context.Context, filter runtime.ChannelMessageFilter) ([]*runtime.ChannelMessage, error) {
	query := scopeQuery(filter.Scope)
	setIfPresent(query, "threadRootId", filter.ThreadRootID)
	if filter.AfterSequence > 0 {
		query.Set("afterSequence", strconv.FormatInt(filter.AfterSequence, 10))
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
	var result []*runtime.ChannelMessage
	path := conversationPath(filter.ConversationID) + "/messages?" + query.Encode()
	if err := c.do(ctx, http.MethodGet, path, nil, "", &result); err != nil {
		return nil, err
	}
	return result, nil
}

func (c *KernelHTTPClient) GetChannelMessage(ctx context.Context, scope runtime.Scope, conversationID, messageID string) (*runtime.ChannelMessage, error) {
	var result runtime.ChannelMessage
	path := conversationPath(conversationID) + "/messages/" + url.PathEscape(strings.TrimSpace(messageID)) + "?" + scopeQuery(scope).Encode()
	if err := c.do(ctx, http.MethodGet, path, nil, "", &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *KernelHTTPClient) CoordinateParticipation(ctx context.Context, conversationID string, request kernelapi.CoordinateParticipationRequest, idempotencyKey string) (*runtime.ParticipationRoundResult, error) {
	var result runtime.ParticipationRoundResult
	if err := c.do(ctx, http.MethodPost, conversationPath(conversationID)+"/participation-rounds", request, idempotencyKey, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *KernelHTTPClient) GetParticipationRound(ctx context.Context, scope runtime.Scope, conversationID, roundID string) (*runtime.ParticipationRoundResult, error) {
	var result runtime.ParticipationRoundResult
	path := conversationPath(conversationID) + "/participation-rounds/" + url.PathEscape(strings.TrimSpace(roundID)) + "?" + scopeQuery(scope).Encode()
	if err := c.do(ctx, http.MethodGet, path, nil, "", &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *KernelHTTPClient) ListParticipationRounds(ctx context.Context, filter runtime.ParticipationRoundFilter) ([]*runtime.ParticipationRoundResult, error) {
	query := scopeQuery(filter.Scope)
	setPage(query, filter.Limit, filter.Offset)
	var result []*runtime.ParticipationRoundResult
	path := conversationPath(filter.ConversationID) + "/participation-rounds?" + query.Encode()
	if err := c.do(ctx, http.MethodGet, path, nil, "", &result); err != nil {
		return nil, err
	}
	return result, nil
}

func (c *KernelHTTPClient) AdvanceConversationCursor(ctx context.Context, conversationID string, request kernelapi.AdvanceConversationCursorRequest) (*runtime.ConversationCursor, bool, error) {
	var result struct {
		Cursor   *runtime.ConversationCursor `json:"cursor"`
		Replayed bool                        `json:"replayed"`
	}
	if err := c.do(ctx, http.MethodPut, conversationPath(conversationID)+"/cursor", request, "", &result); err != nil {
		return nil, false, err
	}
	return result.Cursor, result.Replayed, nil
}

func (c *KernelHTTPClient) GetConversationCursor(ctx context.Context, scope runtime.Scope, conversationID string, participant runtime.ConversationParticipant) (*runtime.ConversationCursor, error) {
	query := scopeQuery(scope)
	query.Set("participantType", string(participant.Type))
	query.Set("participantId", participant.ID)
	var result runtime.ConversationCursor
	if err := c.do(ctx, http.MethodGet, conversationPath(conversationID)+"/cursor?"+query.Encode(), nil, "", &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *KernelHTTPClient) SetConversationPresence(ctx context.Context, conversationID string, request kernelapi.SetConversationPresenceRequest) (*runtime.ConversationPresence, error) {
	var result runtime.ConversationPresence
	if err := c.do(ctx, http.MethodPut, conversationPath(conversationID)+"/presence", request, "", &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *KernelHTTPClient) ReleaseConversationPresence(ctx context.Context, conversationID string, request kernelapi.ReleaseConversationPresenceRequest) error {
	return c.do(ctx, http.MethodDelete, conversationPath(conversationID)+"/presence", request, "", nil)
}

func (c *KernelHTTPClient) ListConversationPresence(ctx context.Context, scope runtime.Scope, conversationID string) ([]*runtime.ConversationPresence, error) {
	var result []*runtime.ConversationPresence
	if err := c.do(ctx, http.MethodGet, conversationPath(conversationID)+"/presence?"+scopeQuery(scope).Encode(), nil, "", &result); err != nil {
		return nil, err
	}
	return result, nil
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
