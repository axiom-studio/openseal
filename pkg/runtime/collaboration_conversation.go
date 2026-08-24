package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
)

const agentRequestProjectionCacheLimit = 10000

// AgentRequestConversationProjector reconciles the authoritative
// AgentRequest lifecycle into linked channels. Messages are idempotent
// projections: conversations never own or advance collaboration state.
type AgentRequestConversationProjector struct {
	collaboration *CollaborationService
	conversations *ConversationService
	mu            sync.Mutex
	completed     map[string]struct{}
	completedFIFO []string
}

func NewAgentRequestConversationProjector(store interface {
	CollaborationKernelStore
	ConversationStore
}) (*AgentRequestConversationProjector, error) {
	if store == nil {
		return nil, errors.New("collaboration conversation store is required")
	}
	return &AgentRequestConversationProjector{
		collaboration: NewCollaborationService(store),
		conversations: NewConversationService(store),
		completed:     make(map[string]struct{}),
	}, nil
}

func (p *AgentRequestConversationProjector) Reconcile(ctx context.Context, scope Scope) (int, error) {
	if p == nil || p.collaboration == nil || p.conversations == nil {
		return 0, errors.New("AgentRequest conversation projector is not configured")
	}
	if err := scope.Validate(); err != nil {
		return 0, err
	}
	const pageSize = 100
	projected := 0
	var failures []error
	for offset := 0; ; offset += pageSize {
		requests, err := p.collaboration.ListAgentRequests(ctx, AgentRequestFilter{Scope: scope, Limit: pageSize, Offset: offset})
		if err != nil {
			return projected, errors.Join(append(failures, err)...)
		}
		for _, request := range requests {
			if request == nil || len(request.ConversationRefs) == 0 {
				continue
			}
			for _, conversationID := range request.ConversationRefs {
				conversationID = strings.TrimSpace(conversationID)
				if conversationID == "" {
					continue
				}
				projectionKey := agentRequestConversationProjectionKey(request, conversationID)
				if p.projectionCompleted(projectionKey) {
					continue
				}
				if err := p.projectRequest(ctx, request, conversationID); err != nil {
					// Conversations are independently owned channel projections. Once a
					// channel is deleted, its historical AgentRequest reference can never
					// be projected again and must not poison the durable inbox worker.
					if errors.Is(err, ErrConversationNotFound) {
						p.rememberProjection(projectionKey)
						continue
					}
					failures = append(failures, fmt.Errorf("request %s conversation %s: %w", request.ID, conversationID, err))
					continue
				}
				p.rememberProjection(projectionKey)
				projected++
			}
		}
		if len(requests) < pageSize {
			return projected, errors.Join(failures...)
		}
	}
}

func agentRequestConversationProjectionKey(request *AgentRequest, conversationID string) string {
	if request == nil {
		return ""
	}
	return fmt.Sprintf("%s\x00%s\x00%s\x00%d\x00%s", request.Scope.Kind, request.Scope.ID, request.ID, request.Revision, conversationID)
}

func (p *AgentRequestConversationProjector) projectionCompleted(key string) bool {
	if p == nil || key == "" {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.completed[key]
	return ok
}

func (p *AgentRequestConversationProjector) rememberProjection(key string) {
	if p == nil || key == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.completed[key]; ok {
		return
	}
	if len(p.completedFIFO) >= agentRequestProjectionCacheLimit {
		delete(p.completed, p.completedFIFO[0])
		p.completedFIFO = p.completedFIFO[1:]
	}
	p.completed[key] = struct{}{}
	p.completedFIFO = append(p.completedFIFO, key)
}

func (p *AgentRequestConversationProjector) projectRequest(ctx context.Context, request *AgentRequest, conversationID string) error {
	conversation, err := p.conversations.GetConversation(ctx, request.Scope, conversationID)
	if err != nil {
		return err
	}
	rootKey := "agent-request:" + request.ID + ":requested"
	rootID := stableConversationID(request.Scope, rootKey, "message")
	intent := MessageIntentProposal
	content := "Requested: " + request.Goal
	if request.Kind == AgentRequestKindHandoff {
		intent = MessageIntentHandoff
		content = "Handed off: " + request.Goal
	} else if request.Kind == AgentRequestKindEscalation {
		intent = MessageIntentEscalation
		content = "Escalated: " + request.Goal
	}
	current, err := p.post(ctx, conversation, PostChannelMessageRequest{
		Sender: participantForCollaborationParty(request.Requester), SenderDisplayName: displayNameForCollaborationParty(request.Requester),
		Intent: intent, Content: content, Audience: ConversationAudience{Kind: ConversationAudienceChannel},
		Mentions: []ConversationParticipant{participantForCollaborationParty(request.Recipient)},
		References: []ConversationReference{
			{Kind: ConversationReferenceRequest, ID: request.ID},
			{Kind: ConversationReferenceRun, ID: request.SourceRunID},
		},
		RequiresResponse: true, IdempotencyKey: rootKey,
	})
	if err != nil {
		return err
	}

	if request.Clarification != "" {
		clarificationKey := "agent-request:" + request.ID + ":clarification"
		clarificationID := stableConversationID(request.Scope, clarificationKey, "message")
		current, err = p.post(ctx, current, PostChannelMessageRequest{
			Sender: participantForAssignedAgent(request), SenderDisplayName: displayNameForAssignedAgent(request),
			Intent: MessageIntentQuestion, Content: request.Clarification,
			Audience: ConversationAudience{Kind: ConversationAudienceChannel}, ReplyToMessageID: rootID,
			Mentions:         []ConversationParticipant{participantForCollaborationParty(request.Requester)},
			References:       []ConversationReference{{Kind: ConversationReferenceRequest, ID: request.ID}},
			RequiresResponse: true, ResolvesMessageID: rootID, IdempotencyKey: clarificationKey,
		})
		if err != nil {
			return err
		}
		if request.Response != "" {
			current, err = p.post(ctx, current, PostChannelMessageRequest{
				Sender: participantForCollaborationParty(request.Requester), SenderDisplayName: displayNameForCollaborationParty(request.Requester),
				Intent: MessageIntentAnswer, Content: request.Response,
				Audience: ConversationAudience{Kind: ConversationAudienceChannel}, ReplyToMessageID: clarificationID,
				References:        []ConversationReference{{Kind: ConversationReferenceRequest, ID: request.ID}},
				ResolvesMessageID: clarificationID, IdempotencyKey: "agent-request:" + request.ID + ":clarification-response",
			})
			if err != nil {
				return err
			}
		}
	}

	if request.AcceptedAt != nil {
		current, err = p.post(ctx, current, PostChannelMessageRequest{
			Sender: participantForAssignedAgent(request), SenderDisplayName: displayNameForAssignedAgent(request),
			Intent: MessageIntentAcknowledgment, Content: "Accepted: " + request.Goal,
			Audience: ConversationAudience{Kind: ConversationAudienceChannel}, ReplyToMessageID: rootID,
			References: requestConversationReferences(request, true), ResolvesMessageID: rootID,
			IdempotencyKey: "agent-request:" + request.ID + ":accepted",
		})
		if err != nil {
			return err
		}
	}

	completionKey := "agent-request:" + request.ID + ":completion-submitted"
	completionID := stableConversationID(request.Scope, completionKey, "message")
	if request.CompletionSubmittedAt != nil {
		requiresReview := request.DelegationPolicy != nil && request.DelegationPolicy.RequireCompletionReview
		completionIntent := MessageIntentUpdate
		if requiresReview {
			completionIntent = MessageIntentProposal
		}
		current, err = p.post(ctx, current, PostChannelMessageRequest{
			Sender: participantForAssignedAgent(request), SenderDisplayName: displayNameForAssignedAgent(request),
			Intent: completionIntent, Content: "Completed delegated work: " + request.CompletionSummary,
			Audience: ConversationAudience{Kind: ConversationAudienceChannel}, ReplyToMessageID: rootID,
			Mentions: completionReviewMentions(request, requiresReview), References: requestConversationReferences(request, true),
			RequiresResponse: requiresReview, IdempotencyKey: completionKey,
		})
		if err != nil {
			return err
		}
	}

	if request.ReviewedAt != nil && request.CompletionReviewer != nil {
		content := "Approved completed work: " + request.CompletionReviewSummary
		if request.Status == AgentRequestStatusFailed {
			content = "Rejected completed work: " + request.CompletionReviewSummary
		}
		current, err = p.post(ctx, current, PostChannelMessageRequest{
			Sender:            participantForCollaborationParty(*request.CompletionReviewer),
			SenderDisplayName: displayNameForCollaborationParty(*request.CompletionReviewer),
			Intent:            MessageIntentDecision, Content: content,
			Audience: ConversationAudience{Kind: ConversationAudienceChannel}, ReplyToMessageID: completionID,
			References: requestConversationReferences(request, true), ResolvesMessageID: completionID,
			IdempotencyKey: "agent-request:" + request.ID + ":completion-reviewed",
		})
		if err != nil {
			return err
		}
	}

	if request.AcceptedAt == nil && (request.Status == AgentRequestStatusRejected || request.Status == AgentRequestStatusFailed || request.Status == AgentRequestStatusCanceled) {
		content := collaborationTerminalContent(request)
		_, err = p.post(ctx, current, PostChannelMessageRequest{
			Sender: participantForAssignedAgent(request), SenderDisplayName: displayNameForAssignedAgent(request),
			Intent: MessageIntentDecision, Content: content,
			Audience: ConversationAudience{Kind: ConversationAudienceChannel}, ReplyToMessageID: rootID,
			References:        []ConversationReference{{Kind: ConversationReferenceRequest, ID: request.ID}},
			ResolvesMessageID: rootID, IdempotencyKey: "agent-request:" + request.ID + ":terminal",
		})
		return err
	}
	return nil
}

func (p *AgentRequestConversationProjector) post(
	ctx context.Context,
	conversation *Conversation,
	request PostChannelMessageRequest,
) (*Conversation, error) {
	request.Scope = conversation.Scope
	request.ConversationID = conversation.ID
	request.ExpectedRevision = conversation.Revision
	result, err := p.conversations.PostChannelMessage(ctx, request)
	if errors.Is(err, ErrRevisionConflict) {
		conversation, err = p.conversations.GetConversation(ctx, conversation.Scope, conversation.ID)
		if err != nil {
			return nil, err
		}
		request.ExpectedRevision = conversation.Revision
		result, err = p.conversations.PostChannelMessage(ctx, request)
	}
	if err != nil {
		return nil, err
	}
	return result.Conversation, nil
}

func participantForCollaborationParty(party CollaborationParty) ConversationParticipant {
	participantType := ConversationParticipantAgent
	if party.Type == OwnerTypeTeam {
		participantType = ConversationParticipantTeam
	}
	return ConversationParticipant{Type: participantType, ID: party.ID}
}

func participantForAssignedAgent(request *AgentRequest) ConversationParticipant {
	if request != nil && strings.TrimSpace(request.AssignedAgentID) != "" {
		return ConversationParticipant{Type: ConversationParticipantAgent, ID: request.AssignedAgentID}
	}
	if request != nil {
		return participantForCollaborationParty(request.Recipient)
	}
	return ConversationParticipant{}
}

func displayNameForCollaborationParty(party CollaborationParty) string {
	if party.Type == OwnerTypeTeam {
		return "Team"
	}
	return "Agent"
}

func displayNameForAssignedAgent(request *AgentRequest) string {
	if request != nil && strings.TrimSpace(request.AssignedAgentID) != "" {
		return "Agent"
	}
	if request != nil {
		return displayNameForCollaborationParty(request.Recipient)
	}
	return "Agent"
}

func requestConversationReferences(request *AgentRequest, includeChild bool) []ConversationReference {
	references := []ConversationReference{
		{Kind: ConversationReferenceRequest, ID: request.ID},
		{Kind: ConversationReferenceRun, ID: request.SourceRunID},
	}
	if includeChild && strings.TrimSpace(request.ChildRunID) != "" {
		references = append(references, ConversationReference{Kind: ConversationReferenceRun, ID: request.ChildRunID})
	}
	for _, artifact := range request.Artifacts {
		references = append(references, ConversationReference{Kind: ConversationReferenceArtifact, ID: artifact.ID, Version: int64(artifact.Version)})
	}
	return references
}

func completionReviewMentions(request *AgentRequest, required bool) []ConversationParticipant {
	if !required || request == nil {
		return nil
	}
	return []ConversationParticipant{participantForCollaborationParty(request.Requester)}
}

func collaborationTerminalContent(request *AgentRequest) string {
	reason := strings.TrimSpace(request.ResolutionReason)
	if reason == "" {
		reason = strings.TrimSpace(request.Clarification)
	}
	if reason == "" {
		reason = "No additional reason was recorded."
	}
	return fmt.Sprintf("%s: %s", strings.ReplaceAll(string(request.Status), "_", " "), reason)
}
