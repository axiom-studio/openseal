package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"

	"github.com/axiom-studio/openseal/pkg/validation"
)

const (
	agentBuilderConversationKeyPrefix = "agent-builder:"
	agentBuilderPromptKeyPrefix       = "agent-builder-prompt:"
	defaultAgentBuilderDraftLimit     = 50
	maximumAgentBuilderDraftLimit     = 100
	agentBuilderDraftRunScan          = 20
	agentBuilderDraftApprovalScan     = 100
	agentBuilderDraftMessageScan      = 50
)

type StartAgentBuilderResult struct {
	Conversation *Conversation
	Message      *ChannelMessage
	Replayed     bool
}

// AgentBuilderConversationWriter creates and posts to conversations. The
// Engine implements it and schedules the builder's Run on post.
type AgentBuilderConversationWriter interface {
	CreateConversation(context.Context, runtimeconversation.CreateConversationRequest) (*Conversation, bool, error)
	PostChannelMessage(context.Context, runtimeconversation.PostChannelMessageRequest) (*ChannelMessageCommitResult, error)
}

// StartAgentBuilderConversation creates (or replays) the builder conversation
// keyed by req.IdempotencyKey and posts the request as its first message.
func StartAgentBuilderConversation(ctx context.Context, writer AgentBuilderConversationWriter, req StartAgentBuilderRequest) (*StartAgentBuilderResult, error) {
	if writer == nil {
		return nil, errors.New("conversation store is not configured")
	}
	if err := req.Scope.Validate(); err != nil {
		return nil, err
	}
	if req.Requester.Type != ConversationParticipantUser || strings.TrimSpace(req.Requester.ID) == "" {
		return nil, fmt.Errorf("%w: a builder conversation is started by a person", ErrInvalidConversation)
	}
	key := strings.TrimSpace(req.IdempotencyKey)
	if key == "" {
		return nil, fmt.Errorf("%w: idempotency key is required", ErrInvalidConversation)
	}
	prompt := strings.TrimSpace(req.Prompt)
	if err := validation.ValidateNoSensitiveInput(prompt); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidConversation, err)
	}
	parameters := req.Parameters
	parameters.Kind = strings.TrimSpace(parameters.Kind)
	if parameters.Kind == "" {
		parameters.Kind = AgentBuilderKindAgent
	}
	parameters.Name = strings.TrimSpace(parameters.Name)
	parameters.Model = strings.TrimSpace(parameters.Model)
	if err := parameters.Validate(); err != nil {
		return nil, err
	}
	conversation, conversationReplayed, err := writer.CreateConversation(ctx, runtimeconversation.CreateConversationRequest{
		Scope:          req.Scope,
		Owner:          ObjectiveOwner{Type: OwnerTypeAgent, ID: AgentBuilderDeploymentID},
		Title:          agentBuilderConversationTitle(parameters),
		Origin:         &ConversationReference{Kind: ConversationReferenceAgentBuilder, ID: key},
		SpaceID:        strings.TrimSpace(req.SpaceID),
		CreatedBy:      strings.TrimSpace(req.Requester.ID),
		IdempotencyKey: agentBuilderConversationKeyPrefix + key,
	})
	if err != nil {
		return nil, err
	}
	posted, err := writer.PostChannelMessage(ctx, runtimeconversation.PostChannelMessageRequest{
		Scope: req.Scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
		Sender: req.Requester, Intent: MessageIntentUpdate, Content: prompt,
		Audience:          ConversationAudience{Kind: ConversationAudienceChannel},
		BuilderParameters: &parameters,
		IdempotencyKey:    agentBuilderPromptKeyPrefix + key,
	})
	if err != nil {
		return nil, err
	}
	if posted.Conversation != nil {
		conversation = posted.Conversation
	}
	return &StartAgentBuilderResult{Conversation: conversation, Message: posted.Message, Replayed: conversationReplayed && posted.Replayed}, nil
}

func agentBuilderConversationTitle(parameters AgentBuilderParameters) string {
	if parameters.Name != "" {
		return parameters.Name
	}
	if parameters.Kind == AgentBuilderKindTeam {
		return "New team"
	}
	return "New agent"
}

// AgentBuilderDraftFilter selects the viewer's builder conversations.
type AgentBuilderDraftFilter struct {
	Scope Scope
	// Requester is the viewer; only conversations whose first message they
	// sent are listed.
	Requester ConversationParticipant
	// Principal is the viewer as approver; it decides ready vs needs_approval.
	Principal ApprovalPrincipal
	// SpaceID lists only that space's drafts; "" lists every space.
	SpaceID string
	Limit   int // at most 100; 0 means 50
	Offset  int
}

type AgentBuilderDraftStatus string

const (
	AgentBuilderDraftDrafting      AgentBuilderDraftStatus = "drafting"
	AgentBuilderDraftNeedsInput    AgentBuilderDraftStatus = "needs_input"
	AgentBuilderDraftReady         AgentBuilderDraftStatus = "ready"
	AgentBuilderDraftNeedsApproval AgentBuilderDraftStatus = "needs_approval"
	AgentBuilderDraftStopped       AgentBuilderDraftStatus = "stopped"
	AgentBuilderDraftFailed        AgentBuilderDraftStatus = "failed"
	AgentBuilderDraftIdle          AgentBuilderDraftStatus = "draft"
)

// AgentBuilderDraft is one builder conversation that has not produced its
// Agent or Team yet (D5).
type AgentBuilderDraft struct {
	ConversationID string `json:"conversationId"`
	// SpaceID is the space the builder conversation lives in, so a drafts
	// rail can show only the current space's drafts.
	SpaceID string `json:"spaceId"`
	// Title is the newest card's displayName, else the builder parameters'
	// name, else "".
	Title  string                  `json:"title"`
	Status AgentBuilderDraftStatus `json:"status"`
	// RunID is the newest top-level builder Run of the conversation.
	RunID string `json:"runId,omitempty"`
	// Approval is the pending agent card, if any.
	Approval          *ApprovalCheckpoint `json:"approval,omitempty"`
	QuestionMessageID string              `json:"questionMessageId,omitempty"`
	UpdatedAt         time.Time           `json:"updatedAt"`
}

// ListAgentBuilderDrafts lists the requester's active builder conversations,
// newest first, that have not produced their Agent or Team, with the status
// the drafts rail shows. The store selects the page; only its conversations
// are read for status.
func ListAgentBuilderDrafts(ctx context.Context, reader AgentBuilderDraftReader, filter AgentBuilderDraftFilter) ([]AgentBuilderDraft, error) {
	if reader == nil {
		return nil, errors.New("conversation store is not configured")
	}
	if err := filter.Scope.Validate(); err != nil {
		return nil, err
	}
	if filter.Requester.Type != ConversationParticipantUser || strings.TrimSpace(filter.Requester.ID) == "" {
		return nil, fmt.Errorf("%w: drafts are listed for a person", ErrInvalidConversation)
	}
	limit := filter.Limit
	if limit == 0 {
		limit = defaultAgentBuilderDraftLimit
	}
	if limit < 0 || limit > maximumAgentBuilderDraftLimit || filter.Offset < 0 {
		return nil, fmt.Errorf("%w: limit must be 1 to %d and offset cannot be negative", ErrInvalidConversation, maximumAgentBuilderDraftLimit)
	}
	owner := ObjectiveOwner{Type: OwnerTypeAgent, ID: AgentBuilderDeploymentID}
	conversations, err := reader.ListConversations(ctx, ConversationFilter{
		Scope: filter.Scope, Owner: &owner, OriginKind: ConversationReferenceAgentBuilder, AgentBuilderDraftOf: filter.Requester.ID,
		SpaceID: strings.TrimSpace(filter.SpaceID), Statuses: []ConversationStatus{ConversationStatusActive}, Limit: limit, Offset: filter.Offset,
	})
	if err != nil {
		return nil, err
	}
	drafts := make([]AgentBuilderDraft, 0, len(conversations))
	for _, conversation := range conversations {
		draft, err := agentBuilderDraft(ctx, reader, filter, conversation)
		if err != nil {
			return nil, err
		}
		drafts = append(drafts, draft)
	}
	return drafts, nil
}

func agentBuilderDraft(ctx context.Context, reader AgentBuilderDraftReader, filter AgentBuilderDraftFilter, conversation *Conversation) (AgentBuilderDraft, error) {
	approvals, err := reader.ListApprovals(ctx, ApprovalFilter{
		Scope: filter.Scope, ConversationID: conversation.ID, NewestFirst: true, Limit: agentBuilderDraftApprovalScan,
	})
	if err != nil {
		return AgentBuilderDraft{}, err
	}
	var newestCard, pendingCard *ApprovalCheckpoint
	for _, approval := range approvals {
		if !models.IsAgentBuilderApproval(approval) {
			continue
		}
		if newestCard == nil {
			newestCard = approval
		}
		if pendingCard == nil && approval.Status == ApprovalStatusPending {
			pendingCard = approval
		}
	}
	runs, err := reader.ListAgentRuns(ctx, AgentRunFilter{
		Scope: filter.Scope, ConversationID: conversation.ID, AssignedAgentID: AgentBuilderDeploymentID,
		Order: AgentRunOrderCreatedDesc, Limit: agentBuilderDraftRunScan,
	})
	if err != nil {
		return AgentBuilderDraft{}, err
	}
	var run *AgentRun
	for _, candidate := range runs {
		if candidate != nil && candidate.ParentRunID == "" {
			run = candidate
			break
		}
	}
	recent, err := reader.ListChannelMessages(ctx, ChannelMessageFilter{
		Scope: filter.Scope, ConversationID: conversation.ID, Descending: true, Limit: agentBuilderDraftMessageScan,
	})
	if err != nil {
		return AgentBuilderDraft{}, err
	}
	question := pendingAgentBuilderQuestion(recent)

	draft := AgentBuilderDraft{ConversationID: conversation.ID, SpaceID: conversation.SpaceID, UpdatedAt: conversation.UpdatedAt}
	draft.Title = agentBuilderCardDisplayName(newestCard)
	if draft.Title == "" {
		if draft.Title, err = agentBuilderParameterName(ctx, reader, filter.Scope, conversation.ID, recent); err != nil {
			return AgentBuilderDraft{}, err
		}
	}
	if run != nil {
		draft.RunID = run.ID
	}
	if pendingCard != nil {
		draft.Approval = cloneApprovalCheckpoint(pendingCard)
	}
	switch {
	case run != nil && agentBuilderRunDrafting(run.Status):
		draft.Status = AgentBuilderDraftDrafting
	case pendingCard != nil:
		draft.Status = AgentBuilderDraftNeedsApproval
		for _, eligible := range pendingCard.EligibleApprovers {
			if eligible == filter.Principal {
				draft.Status = AgentBuilderDraftReady
				break
			}
		}
	case question != nil:
		draft.Status = AgentBuilderDraftNeedsInput
		draft.QuestionMessageID = question.ID
	case run != nil && run.Status == AgentRunStatusCanceled:
		draft.Status = AgentBuilderDraftStopped
	case run != nil && run.Status == AgentRunStatusFailed:
		draft.Status = AgentBuilderDraftFailed
	default:
		draft.Status = AgentBuilderDraftIdle
	}
	return draft, nil
}

func agentBuilderRunDrafting(status AgentRunStatus) bool {
	switch status {
	case AgentRunStatusQueued, AgentRunStatusPlanning, AgentRunStatusRunning, AgentRunStatusWaitingForDependency, AgentRunStatusWaitingForAgent:
		return true
	}
	return false
}

func agentBuilderCardDisplayName(approval *ApprovalCheckpoint) string {
	if approval == nil {
		return ""
	}
	for _, key := range []string{"agent", "team"} {
		if profile, ok := approval.ProposedAction[key].(map[string]interface{}); ok {
			if name, _ := profile["displayName"].(string); strings.TrimSpace(name) != "" {
				return strings.TrimSpace(name)
			}
		}
	}
	return ""
}

// agentBuilderParameterName is the newest name the person set in the form:
// from the newest page of messages, else the first message, which is read
// only when that page does not reach it.
func agentBuilderParameterName(ctx context.Context, reader AgentBuilderDraftReader, scope Scope, conversationID string, recent []*ChannelMessage) (string, error) {
	for _, message := range recent {
		if message != nil && message.Sender.Type == ConversationParticipantUser && message.BuilderParameters != nil && message.BuilderParameters.Name != "" {
			return message.BuilderParameters.Name, nil
		}
	}
	if len(recent) < agentBuilderDraftMessageScan {
		return "", nil
	}
	first, err := reader.ListChannelMessages(ctx, ChannelMessageFilter{Scope: scope, ConversationID: conversationID, Limit: 1})
	if err != nil || len(first) == 0 || first[0].BuilderParameters == nil {
		return "", err
	}
	return first[0].BuilderParameters.Name, nil
}

// pendingAgentBuilderQuestion returns the newest question of the newest
// messages (descending) that no later message from a person answers.
func pendingAgentBuilderQuestion(recent []*ChannelMessage) *ChannelMessage {
	for _, message := range recent {
		if message == nil {
			continue
		}
		if message.Sender.Type == ConversationParticipantUser {
			return nil
		}
		if message.Intent == MessageIntentQuestion && message.Sender.Type == ConversationParticipantAgent {
			return message
		}
	}
	return nil
}
