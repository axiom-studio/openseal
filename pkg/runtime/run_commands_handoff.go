package runtime

import (
	"context"
	"fmt"
)

// acceptedHandoffTransfer recognizes the durable ownership transfer committed
// atomically by RespondAgentRequest. Run output identifies the exact receipt;
// neither that output nor child context is sufficient to authorize a transfer.
func (s *RunCommandService) acceptedHandoffTransfer(ctx context.Context, source *AgentRun) (*AgentRequest, error) {
	if source.Status != AgentRunStatusCompleted {
		return nil, nil
	}
	requestID, _ := source.Output["handoffRequestId"].(string)
	childID, _ := source.Output["childRunId"].(string)
	if requestID == "" || childID == "" {
		return nil, nil
	}
	requests, ok := s.store.(interface {
		GetAgentRequest(context.Context, Scope, string) (*AgentRequest, error)
	})
	if !ok {
		return nil, fmt.Errorf("%w: handoff cancellation ownership requires a scoped agent request reader", ErrInvalidRunTransition)
	}
	request, err := requests.GetAgentRequest(ctx, source.Scope, requestID)
	if err != nil {
		return nil, err
	}
	// A grouped request can leave these output references while its source
	// remains owned by the join. Later source completion is not a transfer.
	if request == nil || request.ID != requestID || request.Scope != source.Scope ||
		request.Kind != AgentRequestKindHandoff || request.SourceRunID != source.ID || request.ChildRunID != childID ||
		request.DependencyGroupID != "" || request.DependencyID != "" ||
		request.AcceptedAt == nil || request.AcceptedAt.IsZero() || !requesterControlsRun(request.Requester, source) ||
		(request.Status != AgentRequestStatusAccepted && request.Status != AgentRequestStatusCompleted) ||
		(request.DelegationPolicy != nil && request.DelegationPolicy.RequireCompletionReview) {
		return nil, nil
	}
	return request, nil
}

func handoffOwnsChild(transfer *AgentRequest, source, child *AgentRun) bool {
	return transfer != nil && child.ID == transfer.ChildRunID && child.Scope == source.Scope &&
		child.ParentRunID == source.ID && child.RootRunID == source.RootRunID && child.Source == RunSourceHandoff &&
		child.Owner == (ObjectiveOwner{Type: transfer.Recipient.Type, ID: transfer.Recipient.ID}) &&
		transfer.AssignedAgentID != "" && child.AssignedAgentID == transfer.AssignedAgentID
}
