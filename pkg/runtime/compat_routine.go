// Temporary compatibility for the pkg/runtime split. New code imports
// pkg/runtime/routine. Remove once in-flight branches have landed.
//
// Exported declarations of pkg/runtime/routine, kept so that importers of
// pkg/runtime continue to compile.

package runtime

import (
	"context"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/routine"
	"github.com/axiom-studio/openseal/pkg/skill"
)

const DeclinedRoutineBriefReply = routine.DeclinedRoutineBriefReply

const RoutineTimezoneInstruction = routine.RoutineTimezoneInstruction

const RoutineDeliveryInstruction = routine.RoutineDeliveryInstruction

const RoutineEventTenantRunCap = routine.RoutineEventTenantRunCap

const RoutineEventDispatchInterval = routine.RoutineEventDispatchInterval

const RoutineEventHeaderMaxBytes = routine.RoutineEventHeaderMaxBytes

type RoutineEventSourceChecker = routine.RoutineEventSourceChecker

type RoutineEventDispatchResult = routine.RoutineEventDispatchResult

type RoutineEventDispatcher = routine.RoutineEventDispatcher

// NewRoutineEventDispatcher forwards to routine.NewRoutineEventDispatcher.
func NewRoutineEventDispatcher(store models.KernelStore, matcher *routine.RoutineEventMatcher, opts ...models.ServiceOption) *routine.RoutineEventDispatcher {
	return routine.NewRoutineEventDispatcher(store, matcher, opts...)
}

type RoutineEventSourceCatalog = routine.RoutineEventSourceCatalog

const RoutineEventSourceNeedsSignIn = routine.RoutineEventSourceNeedsSignIn

const RoutineEventSourceNotConfigured = routine.RoutineEventSourceNotConfigured

const RoutineEventSourceSharedSignInNotAllowed = routine.RoutineEventSourceSharedSignInNotAllowed

const RoutineEventSourceNew = routine.RoutineEventSourceNew

type RoutineEventSourceOption = routine.RoutineEventSourceOption

type RoutineIndexPublisher = routine.RoutineIndexPublisher

type RoutineEventStats = routine.RoutineEventStats

const RoutineEventIndexRebuildInterval = routine.RoutineEventIndexRebuildInterval

const RoutineEventMaxPendingPerRoutine = routine.RoutineEventMaxPendingPerRoutine

const RoutineEventMaxPendingPerTenant = routine.RoutineEventMaxPendingPerTenant

const RoutineTestEventsPerHour = routine.RoutineTestEventsPerHour

var ErrRoutineNotEventRoutine = routine.ErrRoutineNotEventRoutine

var ErrRoutineTestEventLimit = routine.ErrRoutineTestEventLimit

var ErrRoutineEventsWaitingIsFull = routine.ErrRoutineEventsWaitingIsFull

type RoutineEventMatcher = routine.RoutineEventMatcher

// NewRoutineEventMatcher forwards to routine.NewRoutineEventMatcher.
func NewRoutineEventMatcher(store routine.RoutineEventMatcherStore, opts ...models.ServiceOption) *routine.RoutineEventMatcher {
	return routine.NewRoutineEventMatcher(store, opts...)
}

type RoutineEventFileKeeper = routine.RoutineEventFileKeeper

const RunbookActionEventSources = routine.RunbookActionEventSources

const RunbookActionSendTestEvent = routine.RunbookActionSendTestEvent

var ErrRoutineEventsUnavailable = routine.ErrRoutineEventsUnavailable

var ErrHooksUnavailable = routine.ErrHooksUnavailable

var ErrRoutineSourceUnavailable = routine.ErrRoutineSourceUnavailable

type RoutineSourceUnavailableError = routine.RoutineSourceUnavailableError

type RoutineEventSourceChannel = routine.RoutineEventSourceChannel

type RoutineEventSourceView = routine.RoutineEventSourceView

// NewRoutineEventSourceChecker forwards to routine.NewRoutineEventSourceChecker.
func NewRoutineEventSourceChecker(store interface{}) routine.RoutineEventSourceChecker {
	return routine.NewRoutineEventSourceChecker(store)
}

type RoutineEventBatchView = routine.RoutineEventBatchView

type RoutineEventView = routine.RoutineEventView

// NewRoutineEventBatchView forwards to routine.NewRoutineEventBatchView.
func NewRoutineEventBatchView(batch *models.RoutineEventBatch, records []*models.RoutineEventRecord) *routine.RoutineEventBatchView {
	return routine.NewRoutineEventBatchView(batch, records)
}

type RoutineBrief = routine.RoutineBrief

type RoutineGrantCatalog = routine.RoutineGrantCatalog

var ErrRoutineEventSendGrantNotPinned = routine.ErrRoutineEventSendGrantNotPinned

type RevokeRoutineGrantsRequest = routine.RevokeRoutineGrantsRequest

var ErrRoutineGrantsNotOwner = routine.ErrRoutineGrantsNotOwner

const DefaultRoutineScriptSeconds = routine.DefaultRoutineScriptSeconds

const RoutineScriptContextKey = routine.RoutineScriptContextKey

type ChatFile = routine.ChatFile

var ErrChatFileNotFound = routine.ErrChatFileNotFound

type ChatFileResolver = routine.ChatFileResolver

type RoutineScriptStatus = routine.RoutineScriptStatus

var ErrRoutineHasNoScript = routine.ErrRoutineHasNoScript

// ResolveRoutineScriptStatus forwards to routine.ResolveRoutineScriptStatus.
func ResolveRoutineScriptStatus(ctx context.Context, files routine.ChatFileResolver, activation *models.RunbookActivation) (*routine.RoutineScriptStatus, error) {
	return routine.ResolveRoutineScriptStatus(ctx, files, activation)
}

const RunbookManagementSkillVersion = routine.RunbookManagementSkillVersion

const RunbookActionStart = routine.RunbookActionStart

const RunbookActionReplaceSchedule = routine.RunbookActionReplaceSchedule

const RunbookManagementEndpoint = routine.RunbookManagementEndpoint

// RunbookManagementSkill forwards to routine.RunbookManagementSkill.
func RunbookManagementSkill() *skill.Definition {
	return routine.RunbookManagementSkill()
}

type RunbookActionValidator = routine.RunbookActionValidator

// NewRunbookActionValidator forwards to routine.NewRunbookActionValidator.
func NewRunbookActionValidator(store routine.RunbookActionStore) (*routine.RunbookActionValidator, error) {
	return routine.NewRunbookActionValidator(store)
}

type RunbookActionDispatcher = routine.RunbookActionDispatcher

// NewRunbookActionDispatcher forwards to routine.NewRunbookActionDispatcher.
func NewRunbookActionDispatcher(store routine.RunbookActionStore, fallback agentrun.ActionDispatcher) (*routine.RunbookActionDispatcher, error) {
	return routine.NewRunbookActionDispatcher(store, fallback)
}

var ErrRunbookDefinitionNotFound = routine.ErrRunbookDefinitionNotFound

var ErrRunbookActivationInactive = routine.ErrRunbookActivationInactive

var ErrRunbookActivationUnverified = routine.ErrRunbookActivationUnverified

type RunbookDetail = routine.RunbookDetail

// ResolveRunbookDetail forwards to routine.ResolveRunbookDetail.
func ResolveRunbookDetail(ctx context.Context, store models.RunbookActivationReader, catalog models.RunbookDefinitionCatalog, scope models.Scope, activationID string) (*routine.RunbookDetail, error) {
	return routine.ResolveRunbookDetail(ctx, store, catalog, scope, activationID)
}

type CreateRunbookActivationRequest = routine.CreateRunbookActivationRequest

type UpdateRunbookActivationRequest = routine.UpdateRunbookActivationRequest

type StartRunbookActivationRequest = routine.StartRunbookActivationRequest

// StartRunbookActivation forwards to routine.StartRunbookActivation.
func StartRunbookActivation(ctx context.Context, store models.KernelStore, scope models.Scope, activationID string, request routine.StartRunbookActivationRequest) (*agentrun.AgentRunCommandResult, error) {
	return routine.StartRunbookActivation(ctx, store, scope, activationID, request)
}

// StartRunbookActivationWithExecutionPreparer forwards to routine.StartRunbookActivationWithExecutionPreparer.
func StartRunbookActivationWithExecutionPreparer(ctx context.Context, store models.KernelStore, scope models.Scope, activationID string, request routine.StartRunbookActivationRequest, preparer models.AcceptedRunExecutionPreparer) (*agentrun.AgentRunCommandResult, error) {
	return routine.StartRunbookActivationWithExecutionPreparer(ctx, store, scope, activationID, request, preparer)
}

type RunbookActivationService = routine.RunbookActivationService

// NewRunbookActivationService forwards to routine.NewRunbookActivationService.
func NewRunbookActivationService(store interface {
	models.RunbookActivationStore
	models.PortfolioStore
}, opts ...models.ServiceOption) *routine.RunbookActivationService {
	return routine.NewRunbookActivationService(store, opts...)
}

// RunbookActivationIDForKey forwards to routine.RunbookActivationIDForKey.
func RunbookActivationIDForKey(scope models.Scope, idempotencyKey string) string {
	return routine.RunbookActivationIDForKey(scope, idempotencyKey)
}

type RunbookExecutionAuditRun = routine.RunbookExecutionAuditRun

type RunbookTurnAudit = routine.RunbookTurnAudit

type RunbookActionAudit = routine.RunbookActionAudit

type RunbookApprovalAudit = routine.RunbookApprovalAudit

type RunbookDeliveryAudit = routine.RunbookDeliveryAudit

type RunbookArtifactAudit = routine.RunbookArtifactAudit

type RunbookChildRunAudit = routine.RunbookChildRunAudit

type RunbookNodeExecutionAudit = routine.RunbookNodeExecutionAudit

type RunbookVisitExecutionAudit = routine.RunbookVisitExecutionAudit

type RunbookDataLineage = routine.RunbookDataLineage

type RunbookExecutionAudit = routine.RunbookExecutionAudit

type RunExecutionAudit = routine.RunExecutionAudit

type RunbookExecutionAuditService = routine.RunbookExecutionAuditService

// NewRunbookExecutionAuditService forwards to routine.NewRunbookExecutionAuditService.
func NewRunbookExecutionAuditService(store models.RunbookExecutionAuditStore, catalog models.RunbookDefinitionCatalog) *routine.RunbookExecutionAuditService {
	return routine.NewRunbookExecutionAuditService(store, catalog)
}

type RunbookScheduleResult = routine.RunbookScheduleResult

type RunbookScheduler = routine.RunbookScheduler

// NewRunbookScheduler forwards to routine.NewRunbookScheduler.
func NewRunbookScheduler(store interface {
	models.RunCommandStore
	models.PortfolioStore
	models.RunbookActivationStore
}, opts ...models.ServiceOption) *routine.RunbookScheduler {
	return routine.NewRunbookScheduler(store, opts...)
}

type RunbookStepTraceStatus = routine.RunbookStepTraceStatus

type RunbookDataAvailability = routine.RunbookDataAvailability

const RunbookStepTraceRunning = routine.RunbookStepTraceRunning

const RunbookStepTraceWaiting = routine.RunbookStepTraceWaiting

const RunbookStepTraceSucceeded = routine.RunbookStepTraceSucceeded

const RunbookStepTraceFailed = routine.RunbookStepTraceFailed

const RunbookDataAvailable = routine.RunbookDataAvailable

const RunbookDataMissing = routine.RunbookDataMissing

type RunbookDataObservation = routine.RunbookDataObservation

type RunbookStepTrace = routine.RunbookStepTrace

type RunbookExecutionTrace = routine.RunbookExecutionTrace

// RunbookTraceFromCheckpoint forwards to routine.RunbookTraceFromCheckpoint.
func RunbookTraceFromCheckpoint(checkpoint map[string]interface{}) (*routine.RunbookExecutionTrace, error) {
	return routine.RunbookTraceFromCheckpoint(checkpoint)
}

type RunbookStepProducer = routine.RunbookStepProducer

// NewRunbookStepProducer forwards to routine.NewRunbookStepProducer.
func NewRunbookStepProducer(definition *runbook.Definition, entrypoint string, calls models.ActionCallReader, opts ...models.ServiceOption) (*routine.RunbookStepProducer, error) {
	return routine.NewRunbookStepProducer(definition, entrypoint, calls, opts...)
}

var ErrRoutineReportingUnavailable = routine.ErrRoutineReportingUnavailable

var ErrRoutineNotCreator = routine.ErrRoutineNotCreator

// RoutineEventKinds and RoutineEventLatencyBuckets share their backing
// arrays with routine's lists: reading or writing an element is the same
// as through routine, but reassigning the runtime variable does not change
// routine's.
var (
	RoutineEventKinds          = routine.RoutineEventKinds
	RoutineEventLatencyBuckets = routine.RoutineEventLatencyBuckets
)
