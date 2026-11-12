package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"

	"github.com/axiom-studio/openseal/pkg/models"
)

// conversationInteractionHistoryLimit bounds the decided approvals and the
// resolved or dismissed Skill setup requests a change set carries. Pending
// interactions are never truncated: the user must be able to act on them.
const conversationInteractionHistoryLimit = 50

type conversationApprovalStore interface {
	ListApprovals(context.Context, models.ApprovalFilter) ([]*models.ApprovalCheckpoint, error)
}

// SkillSetupRequestReader lists one agent conversation's Skill setup
// requests saved to deploymentID, or to any deployment when it is empty.
// Hosts may supply a reader that first reconciles pending requests against
// saved bindings, so the projection never shows completed setup.
type SkillSetupRequestReader func(ctx context.Context, scope models.Scope, deploymentID, conversationID string) ([]*models.SkillSetupRequest, error)

// SetSkillSetupRequestReader replaces the store read used for the
// conversation's Skill setup request projection.
func (s *ChangeService) SetSkillSetupRequestReader(reader SkillSetupRequestReader) {
	if s != nil && reader != nil {
		s.skillSetups = reader
	}
}

// SkillSetupRequest is a setup request as placed in one
// conversation view. ThreadRootMessageID is a projection, never authority.
type SkillSetupRequest struct {
	models.SkillSetupRequest
	ThreadRootMessageID string `json:"threadRootMessageId,omitempty"`
}

// listConversationApprovals projects approvals for the conversation's own
// Runs (including connector-thread, delegated and task Runs) with their
// conversation placement resolved here, so clients never read Run lineage.
func (s *ChangeService) listConversationApprovals(ctx context.Context, conversation *models.Conversation, runs []*models.AgentRun) ([]*models.ApprovalCheckpoint, error) {
	if s.approvals == nil || conversation == nil {
		return []*models.ApprovalCheckpoint{}, nil
	}
	byRun := make(map[string]*models.AgentRun, len(runs))
	ids := make([]string, 0, len(runs))
	for _, run := range runs {
		if run != nil && byRun[run.ID] == nil {
			byRun[run.ID] = run
			ids = append(ids, run.ID)
		}
	}
	byID := make(map[string]*models.ApprovalCheckpoint)
	for start := 0; start < len(ids); start += conversationActivityRunBatchSize {
		page, err := s.approvals.ListApprovals(ctx, models.ApprovalFilter{
			Scope: conversation.Scope, RunIDs: ids[start:min(start+conversationActivityRunBatchSize, len(ids))],
		})
		if err != nil {
			return nil, err
		}
		for _, approval := range page {
			if approval == nil || approval.Scope != conversation.Scope || byRun[approval.RunID] == nil {
				continue
			}
			run := byRun[approval.RunID]
			approval.ConversationContext = &models.ApprovalConversationContext{
				ConversationID:      conversation.ID,
				TriggerMessageID:    conversationRunLineageValue(run, byRun, models.ConversationRunTriggerIDContextKey),
				ThreadRootMessageID: conversationRunLineageValue(run, byRun, "threadRootMessageId"),
			}
			byID[approval.ID] = approval
		}
	}
	// The agent's own control conversation also shows work that started
	// outside any conversation (for example a scheduled Run).
	if conversation.Origin != nil && conversation.Origin.Kind == models.ConversationReferenceAgentControl {
		owner := conversation.Owner
		for _, filter := range []models.ApprovalFilter{
			{Status: []models.ApprovalStatus{models.ApprovalStatusPending}},
			{NewestFirst: true, Limit: conversationInteractionHistoryLimit},
		} {
			filter.Scope, filter.Owner, filter.ConversationID, filter.IncludeUnscoped = conversation.Scope, &owner, conversation.ID, true
			page, err := s.approvals.ListApprovals(ctx, filter)
			if err != nil {
				return nil, err
			}
			for _, approval := range page {
				if approval == nil || approval.Scope != conversation.Scope || byID[approval.ID] != nil ||
					approval.ConversationContext == nil || approval.ConversationContext.ConversationID != "" {
					continue
				}
				approval.ConversationContext = &models.ApprovalConversationContext{ConversationID: conversation.ID}
				byID[approval.ID] = approval
			}
		}
	}
	all := make([]*models.ApprovalCheckpoint, 0, len(byID))
	for _, approval := range byID {
		all = append(all, approval)
	}
	approvals := boundConversationInteractions(all, func(a *models.ApprovalCheckpoint) bool { return a.Status == models.ApprovalStatusPending },
		func(a *models.ApprovalCheckpoint) (string, int64, int64) {
			return a.ID, a.CreatedAt.UnixNano(), a.UpdatedAt.UnixNano()
		})
	// Place a card in its thread from the trigger message when the Run did
	// not record the thread itself.
	triggers := make([]string, 0, len(approvals))
	for _, approval := range approvals {
		if context := approval.ConversationContext; context.ThreadRootMessageID == "" && context.TriggerMessageID != "" {
			triggers = append(triggers, context.TriggerMessageID)
		}
	}
	messages, err := s.loadConversationMessages(ctx, conversation, triggers)
	if err != nil {
		return nil, err
	}
	for _, approval := range approvals {
		if context := approval.ConversationContext; context.ThreadRootMessageID == "" {
			context.ThreadRootMessageID = models.ExternalConversationThreadRoot(conversation, messages[context.TriggerMessageID])
		}
	}
	return approvals, nil
}

func (s *ChangeService) listConversationSkillSetupRequests(ctx context.Context, conversation *models.Conversation, runs []*models.AgentRun, viewer *models.ConversationViewer) ([]*SkillSetupRequest, error) {
	if s.skillSetups == nil || conversation == nil {
		return []*SkillSetupRequest{}, nil
	}
	// A request in a conversation is saved to its umbrella, or to the Agent
	// the conversation's builder acted for; both cards are in this chat.
	stored, err := s.skillSetups(ctx, conversation.Scope, "", conversation.ID)
	if err != nil {
		return nil, err
	}
	candidates := make([]*models.SkillSetupRequest, 0, len(stored))
	for _, request := range stored {
		if request != nil && request.Scope == conversation.Scope && request.ChatOwner() == conversation.Owner && request.ConversationID == conversation.ID {
			candidates = append(candidates, models.CloneSkillSetupRequest(request))
		}
	}
	candidates = boundConversationInteractions(candidates, func(r *models.SkillSetupRequest) bool { return r.Status == "pending" },
		func(r *models.SkillSetupRequest) (string, int64, int64) {
			return r.ID, r.CreatedAt.UnixNano(), r.UpdatedAt.UnixNano()
		})
	triggers := make([]string, 0, len(candidates))
	for _, request := range candidates {
		triggers = append(triggers, request.TriggerMessageID)
	}
	messages, err := s.loadConversationMessages(ctx, conversation, triggers)
	if err != nil {
		return nil, err
	}
	if viewer != nil {
		selected := make([]*models.ChannelMessage, 0, len(messages))
		for _, message := range messages {
			selected = append(selected, message)
		}
		visible, err := s.conversations.filterVisibleChannelMessages(ctx, conversation.Scope, conversation.ID, selected, *viewer)
		if err != nil {
			return nil, err
		}
		messages = make(map[string]*models.ChannelMessage, len(visible))
		for _, message := range visible {
			messages[message.ID] = message
		}
	}
	byRun := make(map[string]*models.AgentRun, len(runs))
	for _, run := range runs {
		if run != nil {
			byRun[run.ID] = run
		}
	}
	result := make([]*SkillSetupRequest, 0, len(candidates))
	for _, request := range candidates {
		trigger := messages[request.TriggerMessageID]
		if viewer != nil && trigger == nil {
			continue // A request inherits its originating message's visibility.
		}
		thread := conversationRunLineageValue(byRun[request.RunID], byRun, "threadRootMessageId")
		if thread == "" {
			thread = models.ExternalConversationThreadRoot(conversation, trigger)
		}
		result = append(result, &SkillSetupRequest{SkillSetupRequest: *request, ThreadRootMessageID: thread})
	}
	return result, nil
}

// boundConversationInteractions keeps every pending item and the most recently
// updated settled ones, ordered oldest first.
func boundConversationInteractions[T any](items []T, pending func(T) bool, key func(T) (string, int64, int64)) []T {
	open := make([]T, 0, len(items))
	settled := make([]T, 0, len(items))
	for _, item := range items {
		if pending(item) {
			open = append(open, item)
		} else {
			settled = append(settled, item)
		}
	}
	sort.Slice(settled, func(i, j int) bool {
		leftID, _, leftUpdated := key(settled[i])
		rightID, _, rightUpdated := key(settled[j])
		if leftUpdated != rightUpdated {
			return leftUpdated > rightUpdated
		}
		return leftID > rightID
	})
	if len(settled) > conversationInteractionHistoryLimit {
		settled = settled[:conversationInteractionHistoryLimit]
	}
	result := append(open, settled...)
	sort.Slice(result, func(i, j int) bool {
		leftID, leftCreated, _ := key(result[i])
		rightID, rightCreated, _ := key(result[j])
		if leftCreated != rightCreated {
			return leftCreated < rightCreated
		}
		return leftID < rightID
	})
	return result
}

// conversationRunLineageValue reads a string context value from a Run, or
// the nearest projected ancestor that recorded it.
func conversationRunLineageValue(run *models.AgentRun, runs map[string]*models.AgentRun, key string) string {
	for depth := 0; run != nil && depth < 16; depth++ {
		if value, _ := run.Context[key].(string); strings.TrimSpace(value) != "" {
			return value
		}
		next := run.ParentRunID
		if next == "" || next == run.ID {
			next = run.RootRunID
		}
		if next == "" || next == run.ID {
			return ""
		}
		run = runs[next]
	}
	return ""
}

func (s *ChangeService) loadConversationMessages(ctx context.Context, conversation *models.Conversation, ids []string) (map[string]*models.ChannelMessage, error) {
	unique := make([]string, 0, len(ids))
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if strings.TrimSpace(id) != "" && !seen[id] {
			seen[id] = true
			unique = append(unique, id)
		}
	}
	if len(unique) == 0 || s.conversations == nil {
		return map[string]*models.ChannelMessage{}, nil
	}
	if batch, ok := s.conversations.store.(models.ChannelMessageBatchStore); ok {
		return loadChannelMessageBatches(ctx, batch, conversation.Scope, conversation.ID, unique)
	}
	result := make(map[string]*models.ChannelMessage, len(unique))
	for _, id := range unique {
		message, err := s.conversations.GetChannelMessage(ctx, conversation.Scope, conversation.ID, id)
		if errors.Is(err, models.ErrChannelMessageNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if message != nil {
			result[id] = message
		}
	}
	return result, nil
}

func conversationApprovalProjectionDigest(approvals []*models.ApprovalCheckpoint) (string, error) {
	type entry struct {
		ID       string                              `json:"id"`
		Revision int64                               `json:"revision"`
		Status   models.ApprovalStatus               `json:"status"`
		Context  *models.ApprovalConversationContext `json:"context,omitempty"`
	}
	values := make([]entry, 0, len(approvals))
	for _, approval := range approvals {
		values = append(values, entry{ID: approval.ID, Revision: approval.Revision, Status: approval.Status, Context: approval.ConversationContext})
	}
	encoded, err := json.Marshal(values)
	if err != nil {
		return "", err
	}
	return models.SHA256Digest(encoded), nil
}

func conversationSkillSetupProjectionDigest(requests []*SkillSetupRequest) (string, error) {
	type entry struct {
		ID       string `json:"id"`
		Revision int64  `json:"revision"`
		Status   string `json:"status"`
		Thread   string `json:"thread,omitempty"`
	}
	values := make([]entry, 0, len(requests))
	for _, request := range requests {
		values = append(values, entry{ID: request.ID, Revision: request.Revision, Status: request.Status, Thread: request.ThreadRootMessageID})
	}
	encoded, err := json.Marshal(values)
	if err != nil {
		return "", err
	}
	return models.SHA256Digest(encoded), nil
}
