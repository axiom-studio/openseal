package models

import (
	"context"
	"fmt"

	"github.com/axiom-studio/openseal/pkg/runbook"
)

type RunProgressAcknowledgementStore interface {
	ExternalConversationStore
	PortfolioStore
	ListActivity(context.Context, ActivityFilter) ([]*ActivityEvent, error)
}

const RunReportingMilestonesContextKey = "reportingMilestones"

const RunReportingRootRunIDContextKey = "reportingRootRunId"

func RunReportsMilestone(run *AgentRun, milestone runbook.ReportingMilestone) bool {
	if run == nil {
		return false
	}
	switch values := run.Context[RunReportingMilestonesContextKey].(type) {
	case []interface{}:
		for _, value := range values {
			if fmt.Sprint(value) == string(milestone) {
				return true
			}
		}
	case []string:
		for _, value := range values {
			if value == string(milestone) {
				return true
			}
		}
	}
	return false
}

// FailedConversationReportCandidate is a failed chat Run with an Agent to
// speak for it: an Agent's own Run or a Team member's. A Team's routing Run
// has no speaker, so it reports nothing.
func FailedConversationReportCandidate(run *AgentRun) bool {
	if run == nil || run.Status != AgentRunStatusFailed || run.Kind != RunKindConversation || run.ParentRunID != "" {
		return false
	}
	if run.Owner.Type != OwnerTypeAgent && !IsTeamMemberRun(run) {
		return false
	}
	conversationID, _ := run.Context[ConversationRunConversationIDContextKey].(string)
	triggerID, _ := run.Context[ConversationRunTriggerIDContextKey].(string)
	return IsValidOpaqueIdentifier(conversationID, 128) && IsValidOpaqueIdentifier(triggerID, 128)
}
