package runtime

import (
	"context"
	"encoding/json"
)

// ConversationCredentialRequest is a credential request as placed in one
// conversation view. ThreadRootMessageID is a projection, never authority.
type ConversationCredentialRequest struct {
	CredentialRequest
	ThreadRootMessageID string `json:"threadRootMessageId,omitempty"`
}

func (s *ConversationChangeService) listConversationCredentialRequests(ctx context.Context, conversation *Conversation, runs []*AgentRun, viewer *ConversationViewer) ([]*ConversationCredentialRequest, error) {
	if s.credentials == nil || conversation == nil || conversation.Owner.Type != OwnerTypeAgent {
		return []*ConversationCredentialRequest{}, nil
	}
	stored, err := s.credentials.ListCredentialRequests(ctx, conversation.Scope, conversation.Owner.ID, conversation.ID)
	if err != nil {
		return nil, err
	}
	candidates := make([]*CredentialRequest, 0, len(stored))
	for _, request := range stored {
		if request != nil && request.Scope == conversation.Scope && request.DeploymentID == conversation.Owner.ID && request.ConversationID == conversation.ID {
			candidates = append(candidates, cloneCredentialRequest(request))
		}
	}
	candidates = boundConversationInteractions(candidates, func(r *CredentialRequest) bool { return r.Status == CredentialRequestStatusPending },
		func(r *CredentialRequest) (string, int64, int64) {
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
	result := make([]*ConversationCredentialRequest, 0, len(candidates))
	for _, request := range candidates {
		trigger := messages[request.TriggerMessageID]
		if viewer != nil && trigger == nil {
			continue // A request inherits its originating message's visibility.
		}
		thread := conversationRunLineageValue(byRun[request.RunID], byRun, "threadRootMessageId")
		if thread == "" {
			thread = externalConversationThreadRoot(conversation, trigger)
		}
		result = append(result, &ConversationCredentialRequest{CredentialRequest: *request, ThreadRootMessageID: thread})
	}
	return result, nil
}

func conversationCredentialRequestProjectionDigest(requests []*ConversationCredentialRequest) (string, error) {
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
