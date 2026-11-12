package agentrun

import (
	"context"
	"fmt"

	"github.com/axiom-studio/openseal/pkg/models"
)

// acceptedHandoffTransfer recognizes the durable ownership transfer committed
// atomically by RespondAgentRequest. Run output identifies the exact receipt;
// neither that output nor child context is sufficient to authorize a transfer.
func (s *RunCommandService) acceptedHandoffTransfer(ctx context.Context, source *models.AgentRun) (*models.AgentRequest, error) {
	if source.Status != models.AgentRunStatusCompleted {
		return nil, nil
	}
	requestID, _ := source.Output["handoffRequestId"].(string)
	childID, _ := source.Output["childRunId"].(string)
	if requestID == "" || childID == "" {
		return nil, nil
	}
	requests, ok := s.store.(interface {
		GetAgentRequest(context.Context, models.Scope, string) (*models.AgentRequest, error)
	})
	if !ok {
		return nil, fmt.Errorf("%w: handoff cancellation ownership requires a scoped agent request reader", models.ErrInvalidRunTransition)
	}
	request, err := requests.GetAgentRequest(ctx, source.Scope, requestID)
	if err != nil {
		return nil, err
	}
	// A grouped request can leave these output references while its source
	// remains owned by the join. Later source completion is not a transfer.
	if request == nil || request.ID != requestID || request.Scope != source.Scope ||
		request.Kind != models.AgentRequestKindHandoff || request.SourceRunID != source.ID || request.ChildRunID != childID ||
		request.DependencyGroupID != "" || request.DependencyID != "" ||
		request.AcceptedAt == nil || request.AcceptedAt.IsZero() || !models.RequesterControlsRun(request.Requester, source) ||
		(request.Status != models.AgentRequestStatusAccepted && request.Status != models.AgentRequestStatusCompleted) ||
		(request.DelegationPolicy != nil && request.DelegationPolicy.RequireCompletionReview) {
		return nil, nil
	}
	return request, nil
}

func handoffOwnsChild(transfer *models.AgentRequest, source, child *models.AgentRun) bool {
	return transfer != nil && child.ID == transfer.ChildRunID && child.Scope == source.Scope &&
		child.ParentRunID == source.ID && child.RootRunID == source.RootRunID && child.Source == models.RunSourceHandoff &&
		child.Owner == models.CollaborationChildOwner(source, transfer) &&
		transfer.AssignedAgentID != "" && child.AssignedAgentID == transfer.AssignedAgentID
}
