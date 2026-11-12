// Temporary compatibility for the pkg/runtime split. New code imports
// pkg/runtime/conversationrun. Remove once in-flight branches have landed.
//
// Exported declarations of pkg/runtime/conversationrun, kept so that
// importers of pkg/runtime continue to compile.

package runtime

import (
	"context"

	"github.com/axiom-studio/openseal/pkg/models"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/conversationrun"
	"github.com/axiom-studio/openseal/pkg/runtime/workqueue"
	"go.uber.org/zap"
)

type AgentTurnCatalog = conversationrun.AgentTurnCatalog

// PollVoterKey forwards to conversationrun.PollVoterKey.
func PollVoterKey(participant models.ConversationParticipant) string {
	return conversationrun.PollVoterKey(participant)
}

// PollVotes forwards to conversationrun.PollVotes.
func PollVotes(componentID string, latest []*models.SurfaceInteraction) map[string][]string {
	return conversationrun.PollVotes(componentID, latest)
}

const FinalModeSpoken = conversationrun.FinalModeSpoken

type ConversationLoopAuthority = conversationrun.ConversationLoopAuthority

type ConversationLoopStores = conversationrun.ConversationLoopStores

type ConversationRunSchedulerConfig = conversationrun.ConversationRunSchedulerConfig

type ConversationRunReconcileResult = conversationrun.ConversationRunReconcileResult

type ConversationRunScheduler = conversationrun.ConversationRunScheduler

type ConversationSessionContextResolver = conversationrun.ConversationSessionContextResolver

// NewConversationRunScheduler forwards to conversationrun.NewConversationRunScheduler.
func NewConversationRunScheduler(conversationStore models.ConversationStore, runStore models.RunCommandStore, config conversationrun.ConversationRunSchedulerConfig, opts ...models.ServiceOption) (*conversationrun.ConversationRunScheduler, error) {
	return conversationrun.NewConversationRunScheduler(conversationStore, runStore, config, opts...)
}

type ConversationRunTurnRunnerConfig = conversationrun.ConversationRunTurnRunnerConfig

type ConversationRunTurnRunner = conversationrun.ConversationRunTurnRunner

// NewConversationRunTurnRunner forwards to conversationrun.NewConversationRunTurnRunner.
func NewConversationRunTurnRunner(conversationStore models.ConversationStore, coordinator *runtimeconversation.ConversationCoordinator, config conversationrun.ConversationRunTurnRunnerConfig, opts ...models.ServiceOption) (*conversationrun.ConversationRunTurnRunner, error) {
	return conversationrun.NewConversationRunTurnRunner(conversationStore, coordinator, config, opts...)
}

const AgentBuilderDeploymentID = conversationrun.AgentBuilderDeploymentID

const ChannelReviewParticipantID = conversationrun.ChannelReviewParticipantID

type ConversationRunReconcilerConfig = conversationrun.ConversationRunReconcilerConfig

type ConversationRunReconciler = conversationrun.ConversationRunReconciler

// NewConversationRunReconciler forwards to conversationrun.NewConversationRunReconciler.
func NewConversationRunReconciler(scheduler *conversationrun.ConversationRunScheduler, scopes workqueue.WorkerScopeSource, logger *zap.SugaredLogger, config conversationrun.ConversationRunReconcilerConfig) (*conversationrun.ConversationRunReconciler, error) {
	return conversationrun.NewConversationRunReconciler(scheduler, scopes, logger, config)
}

// ConversationTaskGoalSummary forwards to conversationrun.ConversationTaskGoalSummary.
func ConversationTaskGoalSummary(goal string) string {
	return conversationrun.ConversationTaskGoalSummary(goal)
}

type StartConversationTaskRequest = conversationrun.StartConversationTaskRequest

type GetConversationTaskRequest = conversationrun.GetConversationTaskRequest

type CancelConversationTaskRequest = conversationrun.CancelConversationTaskRequest

type ConversationTaskService = conversationrun.ConversationTaskService

// NewConversationTaskService forwards to conversationrun.NewConversationTaskService.
func NewConversationTaskService(store models.ConversationTaskKernelStore, opts ...models.ServiceOption) *conversationrun.ConversationTaskService {
	return conversationrun.NewConversationTaskService(store, opts...)
}

type ConversationTaskContinuationReconciler = conversationrun.ConversationTaskContinuationReconciler

// NewConversationTaskContinuationReconciler forwards to conversationrun.NewConversationTaskContinuationReconciler.
func NewConversationTaskContinuationReconciler(store models.ConversationTaskContinuationStore) (*conversationrun.ConversationTaskContinuationReconciler, error) {
	return conversationrun.NewConversationTaskContinuationReconciler(store)
}

// FindConversationTaskResultMessage forwards to conversationrun.FindConversationTaskResultMessage.
func FindConversationTaskResultMessage(ctx context.Context, store models.ConversationStore, task *models.ConversationTask, run *models.AgentRun) (*models.ChannelMessage, error) {
	return conversationrun.FindConversationTaskResultMessage(ctx, store, task, run)
}

const ConversationTaskSkillSetupWakeType = conversationrun.ConversationTaskSkillSetupWakeType

var ErrRunTerminalReportUndeliverable = conversationrun.ErrRunTerminalReportUndeliverable
