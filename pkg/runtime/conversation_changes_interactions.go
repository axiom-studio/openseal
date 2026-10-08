package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
)

// conversationInteractionHistoryLimit bounds the decided approvals and the
// resolved or dismissed Skill setup requests a change set carries. Pending
// interactions are never truncated: the user must be able to act on them.
const conversationInteractionHistoryLimit = 50

type conversationApprovalStore interface {
	ListApprovals(context.Context, ApprovalFilter) ([]*ApprovalCheckpoint, error)
}

// SkillSetupRequestReader lists one agent conversation's Skill setup
// requests. Hosts may supply a reader that first reconciles pending requests
// against saved bindings, so the projection never shows completed setup.
type SkillSetupRequestReader func(ctx context.Context, scope Scope, deploymentID, conversationID string) ([]*SkillSetupRequest, error)

// SetSkillSetupRequestReader replaces the store read used for the
// conversation's Skill setup request projection.
func (s *ConversationChangeService) SetSkillSetupRequestReader(reader SkillSetupRequestReader) {
	if s != nil && reader != nil {
		s.skillSetups = reader
	}
}

// ConversationSkillSetupRequest is a setup request as placed in one
// conversation view. ThreadRootMessageID is a projection, never authority.
type ConversationSkillSetupRequest struct {
	SkillSetupRequest
	ThreadRootMessageID string `json:"threadRootMessageId,omitempty"`
}

// listConversationApprovals projects approvals for the conversation's own
// Runs (including connector-thread, delegated and task Runs) with their
// conversation placement resolved here, so clients never read Run lineage.
func (s *ConversationChangeService) listConversationApprovals(ctx context.Context, conversation *Conversation, runs []*AgentRun) ([]*ApprovalCheckpoint, error) {
	if s.approvals == nil || conversation == nil {
		return []*ApprovalCheckpoint{}, nil
	}
	byRun := make(map[string]*AgentRun, len(runs))
	ids := make([]string, 0, len(runs))
	for _, run := range runs {
		if run != nil && byRun[run.ID] == nil {
			byRun[run.ID] = run
			ids = append(ids, run.ID)
		}
	}
	byID := make(map[string]*ApprovalCheckpoint)
	for start := 0; start < len(ids); start += conversationActivityRunBatchSize {
		page, err := s.approvals.ListApprovals(ctx, ApprovalFilter{
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
			approval.ConversationContext = &ApprovalConversationContext{
				ConversationID:      conversation.ID,
				TriggerMessageID:    conversationRunLineageValue(run, byRun, conversationRunContextTriggerID),
				ThreadRootMessageID: conversationRunLineageValue(run, byRun, "threadRootMessageId"),
			}
			byID[approval.ID] = approval
		}
	}
	// The agent's own control conversation also shows work that started
	// outside any conversation (for example a scheduled Run).
	if conversation.Origin != nil && conversation.Origin.Kind == ConversationReferenceAgentControl {
		owner := conversation.Owner
		for _, filter := range []ApprovalFilter{
			{Status: []ApprovalStatus{ApprovalStatusPending}},
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
				approval.ConversationContext = &ApprovalConversationContext{ConversationID: conversation.ID}
				byID[approval.ID] = approval
			}
		}
	}
	all := make([]*ApprovalCheckpoint, 0, len(byID))
	for _, approval := range byID {
		all = append(all, approval)
	}
	approvals := boundConversationInteractions(all, func(a *ApprovalCheckpoint) bool { return a.Status == ApprovalStatusPending },
		func(a *ApprovalCheckpoint) (string, int64, int64) {
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
			context.ThreadRootMessageID = externalConversationThreadRoot(conversation, messages[context.TriggerMessageID])
		}
	}
	return approvals, nil
}

func (s *ConversationChangeService) listConversationSkillSetupRequests(ctx context.Context, conversation *Conversation, runs []*AgentRun, viewer *ConversationViewer) ([]*ConversationSkillSetupRequest, error) {
	if s.skillSetups == nil || conversation == nil || conversation.Owner.Type != OwnerTypeAgent {
		return []*ConversationSkillSetupRequest{}, nil
	}
	stored, err := s.skillSetups(ctx, conversation.Scope, conversation.Owner.ID, conversation.ID)
	if err != nil {
		return nil, err
	}
	candidates := make([]*SkillSetupRequest, 0, len(stored))
	for _, request := range stored {
		if request != nil && request.Scope == conversation.Scope && request.DeploymentID == conversation.Owner.ID && request.ConversationID == conversation.ID {
			candidates = append(candidates, cloneSkillSetupRequest(request))
		}
	}
	candidates = boundConversationInteractions(candidates, func(r *SkillSetupRequest) bool { return r.Status == "pending" },
		func(r *SkillSetupRequest) (string, int64, int64) {
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
		selected := make([]*ChannelMessage, 0, len(messages))
		for _, message := range messages {
			selected = append(selected, message)
		}
		visible, err := s.conversations.filterVisibleChannelMessages(ctx, conversation.Scope, conversation.ID, selected, *viewer)
		if err != nil {
			return nil, err
		}
		messages = make(map[string]*ChannelMessage, len(visible))
		for _, message := range visible {
			messages[message.ID] = message
		}
	}
	byRun := make(map[string]*AgentRun, len(runs))
	for _, run := range runs {
		if run != nil {
			byRun[run.ID] = run
		}
	}
	result := make([]*ConversationSkillSetupRequest, 0, len(candidates))
	for _, request := range candidates {
		trigger := messages[request.TriggerMessageID]
		if viewer != nil && trigger == nil {
			continue // A request inherits its originating message's visibility.
		}
		thread := conversationRunLineageValue(byRun[request.RunID], byRun, "threadRootMessageId")
		if thread == "" {
			thread = externalConversationThreadRoot(conversation, trigger)
		}
		result = append(result, &ConversationSkillSetupRequest{SkillSetupRequest: *request, ThreadRootMessageID: thread})
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
func conversationRunLineageValue(run *AgentRun, runs map[string]*AgentRun, key string) string {
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

func (s *ConversationChangeService) loadConversationMessages(ctx context.Context, conversation *Conversation, ids []string) (map[string]*ChannelMessage, error) {
	unique := make([]string, 0, len(ids))
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if strings.TrimSpace(id) != "" && !seen[id] {
			seen[id] = true
			unique = append(unique, id)
		}
	}
	if len(unique) == 0 || s.conversations == nil {
		return map[string]*ChannelMessage{}, nil
	}
	if batch, ok := s.conversations.store.(ChannelMessageBatchStore); ok {
		return loadChannelMessageBatches(ctx, batch, conversation.Scope, conversation.ID, unique)
	}
	result := make(map[string]*ChannelMessage, len(unique))
	for _, id := range unique {
		message, err := s.conversations.GetChannelMessage(ctx, conversation.Scope, conversation.ID, id)
		if errors.Is(err, ErrChannelMessageNotFound) {
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

func conversationApprovalProjectionDigest(approvals []*ApprovalCheckpoint) (string, error) {
	type entry struct {
		ID       string                       `json:"id"`
		Revision int64                        `json:"revision"`
		Status   ApprovalStatus               `json:"status"`
		Context  *ApprovalConversationContext `json:"context,omitempty"`
	}
	values := make([]entry, 0, len(approvals))
	for _, approval := range approvals {
		values = append(values, entry{ID: approval.ID, Revision: approval.Revision, Status: approval.Status, Context: approval.ConversationContext})
	}
	encoded, err := json.Marshal(values)
	if err != nil {
		return "", err
	}
	return hashBytes(encoded), nil
}

func conversationSkillSetupProjectionDigest(requests []*ConversationSkillSetupRequest) (string, error) {
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
	return hashBytes(encoded), nil
}
