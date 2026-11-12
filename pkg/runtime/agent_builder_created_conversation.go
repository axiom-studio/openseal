package runtime

import (
	"context"
	"errors"

	"github.com/axiom-studio/openseal/pkg/models"
)

// agentBuilderCreatesResource reports a card whose approval and success
// produce the conversation's Agent or Team.
func agentBuilderCreatesResource(approval *models.ApprovalCheckpoint) bool {
	resourceType, _ := approval.ProposedAction["resourceType"].(string)
	operation, _ := approval.ProposedAction["operation"].(string)
	return resourceType == models.AgentProfileResourceType && operation == models.AgentActionCreateAgent ||
		resourceType == "team" && operation == models.AgentBuilderTeamOperation
}

// createdAgentBuilderConversation is the conversation whose approved card
// call, now succeeded, creates an Agent or Team; "" for any other call.
func createdAgentBuilderConversation(ctx context.Context, store models.KernelStore, call *models.ActionCall, run *models.AgentRun) (string, error) {
	if call.ApprovalID == "" {
		return "", nil
	}
	approval, err := store.GetApproval(ctx, call.Scope, call.ApprovalID)
	if err != nil && !errors.Is(err, models.ErrApprovalNotFound) {
		return "", err
	}
	if approval == nil || approval.Status != models.ApprovalStatusApproved || !agentBuilderCreatesResource(approval) {
		return "", nil
	}
	// The same conversation ListApprovals reports for the card: the Run's
	// own, else its root Run's.
	if id, _ := run.Context["conversationId"].(string); id != "" || run.RootRunID == run.ID {
		return id, nil
	}
	root, err := store.GetAgentRun(ctx, run.Scope, run.RootRunID)
	if err != nil || root == nil {
		return "", err
	}
	id, _ := root.Context["conversationId"].(string)
	return id, nil
}
