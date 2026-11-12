package action

import (
	"context"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/workqueue"
	"github.com/axiom-studio/openseal/pkg/skill"
	"go.uber.org/zap"
)

// Test-only access to action internals for the external action tests,
// which use fixtures shared with tests of other packages and so cannot be
// in-package tests.

const (
	MaximumProposalRejectionMessageBytes          = maximumProposalRejectionMessageBytes
	ProposalRejectionReviewSummaryRequiredMessage = proposalRejectionReviewSummaryRequiredMessage
	LoopActionMaintenanceRetry                    = loopActionMaintenanceRetry
	ApprovalResolveAttempts                       = approvalResolveAttempts
)

// ErrLoopActionAbandoned is the error an abandoned loop call records.
var ErrLoopActionAbandoned = errLoopActionAbandoned

func CanonicalActionCredentialLeasePayload(lease models.ActionCredentialLease) ([]byte, error) {
	return canonicalActionCredentialLeasePayload(lease)
}

func CreatedAgentBuilderConversation(ctx context.Context, store models.KernelStore, call *models.ActionCall, run *models.AgentRun) (string, error) {
	return createdAgentBuilderConversation(ctx, store, call, run)
}

func HostActionBinding(bound *skill.BoundAction) *skill.Binding { return hostActionBinding(bound) }

func IsTargetedSkillBindingApproval(approval *models.ApprovalCheckpoint) bool {
	return isTargetedSkillBindingApproval(approval)
}

func ApprovedOnlyByPerson(approval *models.ApprovalCheckpoint) bool {
	return approvedOnlyByPerson(approval)
}

// NewActionWorkerSupervisorRetiringAfter builds a supervisor whose idle
// scope pools retire after retire instead of workqueue.PoolRetireAfter.
func NewActionWorkerSupervisorRetiringAfter(
	store models.KernelStore,
	catalog agentrun.ActionExecutionCatalog,
	credentials CredentialResolver,
	issuer CredentialLeaseIssuer,
	dispatcher agentrun.ActionDispatcher,
	source workqueue.WorkerScopeSource,
	logger *zap.SugaredLogger,
	config DynamicActionWorkerConfig,
	retire time.Duration,
) (*WorkerSupervisor, error) {
	return newActionWorkerSupervisor(store, catalog, credentials, issuer, dispatcher, source, logger, config, retire)
}

func (c *Coordinator) SetNewID(newID func() string) { c.newID = newID }

func (c *Coordinator) SetNestedLimits(calls, concurrent int) {
	c.nestedMaxCalls, c.nestedMaxConcurrent = calls, concurrent
}

func (c *ApprovalCoordinator) SetNewID(newID func() string) { c.newID = newID }

func (w *Worker) SetNewID(newID func() string) { w.newID = newID }

func (w *Worker) SetLoopLease(lease time.Duration) { w.loopLease = lease }

func (w *Worker) SetOnClaim(onClaim func()) { w.onClaim = onClaim }

func (w *Worker) SetCredentials(credentials CredentialResolver) { w.credentials = credentials }
