// Temporary compatibility for the pkg/runtime split. New code imports
// pkg/runtime/activity. Remove once in-flight branches have landed.
//
// Exported declarations of pkg/runtime/activity, kept so that importers of
// pkg/runtime continue to compile.

package runtime

import (
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/activity"
)

type RunTransitionRequest = activity.RunTransitionRequest

type RunActivityService = activity.RunActivityService

// NewRunActivityService forwards to activity.NewRunActivityService.
func NewRunActivityService(portfolio models.PortfolioStore, activityStore models.RunActivityStore, opts ...models.ServiceOption) *RunActivityService {
	return activity.NewRunActivityService(portfolio, activityStore, opts...)
}

type ActivityFeedRequest = activity.ActivityFeedRequest

type ActivityFeedPage = activity.ActivityFeedPage

type ApprovalReviewContext = activity.ApprovalReviewContext

type ApprovalReviewFact = activity.ApprovalReviewFact

type ExternalOperationIdentity = activity.ExternalOperationIdentity

type TurnAction = activity.TurnAction

const ActivityEventTurnNarration = activity.ActivityEventTurnNarration

type BeginAgentTurnRequest = activity.BeginAgentTurnRequest

type FinishAgentTurnRequest = activity.FinishAgentTurnRequest

type StepModelRecord = activity.StepModelRecord

type AgentTurnService = activity.AgentTurnService

// NewAgentTurnService forwards to activity.NewAgentTurnService.
func NewAgentTurnService(portfolio models.PortfolioStore, turns models.AgentTurnStore, opts ...models.ServiceOption) *AgentTurnService {
	return activity.NewAgentTurnService(portfolio, turns, opts...)
}
