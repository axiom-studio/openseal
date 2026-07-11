// Package openseal provides the stable public API for embedding OpenSeal
// as a library. Downstream consumers should import this package
// rather than reaching into individual pkg/ subpackages.
package openseal

import (
	"context"
	"fmt"
	"strings"
	"time"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/executor"
	"github.com/axiom-studio/openseal/pkg/runtime"
	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/axiom-studio/openseal/pkg/skill/clawhub"
	skillopenclaw "github.com/axiom-studio/openseal/pkg/skill/openclaw"
	skillsource "github.com/axiom-studio/openseal/pkg/skill/source"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
	"github.com/axiom-studio/openseal/pkg/types"
	"github.com/axiom-studio/openseal/pkg/workforce"
	"go.uber.org/zap"
)

// Re-export key types so consumers only import this package.
type (
	NodeDefinition       = executor.NodeDefinition
	ConnectionDefinition = executor.ConnectionDefinition
	ExecutionResult      = executor.ExecutionResult
	NodeResult           = executor.NodeResult
	Registry             = executor.Registry
	StepExecutor         = executor.StepExecutor
	ExecutionGraph       = executor.ExecutionGraph

	AgentNodeDefinition                   = types.AgentNodeDefinition
	AgentConnection                       = types.AgentConnection
	AgentLibraryBean                      = types.AgentLibraryBean
	AgentInstanceBean                     = types.AgentInstanceBean
	AgentWorkflow                         = types.AgentWorkflow
	AgentWorkflowBean                     = types.AgentWorkflowBean
	AgentDefinition                       = kernelagent.AgentDefinition
	AgentSkillRequirement                 = kernelagent.SkillRequirement
	AgentAuthorityPolicy                  = kernelagent.AuthorityPolicy
	AgentMemoryPolicy                     = kernelagent.MemoryPolicy
	AgentEscalationPolicy                 = kernelagent.EscalationPolicy
	AgentObjectiveTemplate                = kernelagent.ObjectiveTemplate
	AgentEvaluationCriterion              = kernelagent.EvaluationCriterion
	AgentAmendmentPolicy                  = kernelagent.AmendmentPolicy
	AgentDefinitionProvenance             = kernelagent.DefinitionProvenance
	AgentDeployment                       = kernelagent.AgentDeployment
	AgentDeploymentRestrictions           = kernelagent.DeploymentRestrictions
	AgentDeploymentCapacity               = kernelagent.DeploymentCapacity
	AgentDeploymentHealth                 = kernelagent.DeploymentHealth
	AgentDefinitionActivation             = kernelagent.DefinitionActivation
	AgentRolloutStatus                    = kernelagent.RolloutStatus
	AgentDefinitionAmendment              = kernelagent.DefinitionAmendment
	AgentAmendmentStatus                  = kernelagent.AmendmentStatus
	AgentDefinitionFieldChange            = kernelagent.DefinitionFieldChange
	AgentAmendmentEvaluation              = kernelagent.AmendmentEvaluation
	AgentAmendmentDecision                = kernelagent.AmendmentDecision
	ProposeAgentAmendmentRequest          = kernelagent.ProposeAmendmentRequest
	SubmitAgentAmendmentEvaluationRequest = kernelagent.SubmitAmendmentEvaluationRequest
	ResolveAgentAmendmentRequest          = kernelagent.ResolveAmendmentRequest
	AgentRegistryStore                    = kernelagent.Store
	TeamDefinition                        = kernelteam.Definition
	TeamRoleSlot                          = kernelteam.RoleSlot
	TeamRoleChannelParticipation          = kernelteam.RoleChannelParticipation
	TeamCoordinationMode                  = kernelteam.CoordinationMode
	TeamCoordinationPolicy                = kernelteam.CoordinationPolicy
	TeamDelegationPolicy                  = kernelteam.DelegationPolicy
	TeamApprovalPolicy                    = kernelteam.ApprovalPolicy
	TeamDeployment                        = kernelteam.Deployment
	TeamDeploymentStatus                  = kernelteam.DeploymentStatus
	TeamRosterAssignment                  = kernelteam.RosterAssignment
	TeamDeploymentRestrictions            = kernelteam.DeploymentRestrictions
	TeamDefinitionAmendment               = kernelteam.DefinitionAmendment
	TeamAmendmentStatus                   = kernelteam.AmendmentStatus
	TeamAmendmentEvaluation               = kernelteam.AmendmentEvaluation
	TeamAmendmentDecision                 = kernelteam.AmendmentDecision
	ProposeTeamAmendmentRequest           = kernelteam.ProposeAmendmentRequest
	SubmitTeamAmendmentEvaluationRequest  = kernelteam.SubmitAmendmentEvaluationRequest
	ResolveTeamAmendmentRequest           = kernelteam.ResolveAmendmentRequest
	TeamRegistryStore                     = kernelteam.Store
	WorkforceSharedContextPolicy          = workforce.SharedContextPolicy
	WorkforceObjectiveTemplate            = workforce.ObjectiveTemplate
	WorkforceEvaluationCriterion          = workforce.EvaluationCriterion
	WorkforceAmendmentPolicy              = workforce.AmendmentPolicy
	WorkforceDefinitionProvenance         = workforce.DefinitionProvenance
	WorkforceDefinitionActivation         = workforce.DefinitionActivation

	RunRecord                          = runtime.RunRecord
	RetryPolicy                        = runtime.RetryPolicy
	ExecutionStore                     = runtime.ExecutionStore
	PortfolioStore                     = runtime.PortfolioStore
	KernelStore                        = runtime.KernelStore
	PostgresStore                      = runtime.PostgresStore
	PostgresStoreOption                = runtime.PostgresStoreOption
	Scope                              = runtime.Scope
	ObjectiveOwner                     = runtime.ObjectiveOwner
	Objective                          = runtime.Objective
	ObjectiveStatus                    = runtime.ObjectiveStatus
	ObjectiveCadence                   = runtime.ObjectiveCadence
	ObjectiveCadenceType               = runtime.ObjectiveCadenceType
	ObjectiveScheduleResult            = runtime.ObjectiveScheduleResult
	ObjectiveFilter                    = runtime.ObjectiveFilter
	AgentRun                           = runtime.AgentRun
	AgentRunIntervention               = runtime.AgentRunIntervention
	BudgetPolicy                       = runtime.BudgetPolicy
	BudgetUsage                        = runtime.BudgetUsage
	BudgetReservation                  = runtime.BudgetReservation
	BudgetState                        = runtime.BudgetState
	AgentRunStatus                     = runtime.AgentRunStatus
	AgentRunFilter                     = runtime.AgentRunFilter
	RunSource                          = runtime.RunSource
	RunKind                            = runtime.RunKind
	WakeCondition                      = runtime.WakeCondition
	CreateObjectiveRequest             = runtime.CreateObjectiveRequest
	CreateObjectiveResult              = runtime.CreateObjectiveResult
	UpdateObjectiveRequest             = runtime.UpdateObjectiveRequest
	CreateAgentRunRequest              = runtime.CreateAgentRunRequest
	AgentRunCommandKind                = runtime.AgentRunCommandKind
	AgentRunCommandRequest             = runtime.AgentRunCommandRequest
	AgentRunCommandResult              = runtime.AgentRunCommandResult
	RunActivityStore                   = runtime.RunActivityStore
	ActivityEvent                      = runtime.ActivityEvent
	ActivityActor                      = runtime.ActivityActor
	ActivityFilter                     = runtime.ActivityFilter
	ActivityFeedRequest                = runtime.ActivityFeedRequest
	ActivityProjection                 = runtime.ActivityProjection
	ActivityFeedPage                   = runtime.ActivityFeedPage
	ActivitySeverity                   = runtime.ActivitySeverity
	ActivityVisibility                 = runtime.ActivityVisibility
	RunTransitionRequest               = runtime.RunTransitionRequest
	RunDependencyStore                 = runtime.RunDependencyStore
	DependencyKernelStore              = runtime.DependencyKernelStore
	RunDependencyKind                  = runtime.RunDependencyKind
	RunDependencyState                 = runtime.RunDependencyState
	FanInMode                          = runtime.FanInMode
	DependencyFailureMode              = runtime.DependencyFailureMode
	RunDependencyGroupStatus           = runtime.RunDependencyGroupStatus
	RunDependencyPolicy                = runtime.RunDependencyPolicy
	RunDependencyGroup                 = runtime.RunDependencyGroup
	RunDependency                      = runtime.RunDependency
	RunDependencySpec                  = runtime.RunDependencySpec
	RunDependencyEvaluation            = runtime.RunDependencyEvaluation
	CreateRunDependencyGroupRequest    = runtime.CreateRunDependencyGroupRequest
	ResolveRunDependencyRequest        = runtime.ResolveRunDependencyRequest
	RunDependencyResult                = runtime.RunDependencyResult
	ConversationStore                  = runtime.ConversationStore
	Conversation                       = runtime.Conversation
	ConversationStatus                 = runtime.ConversationStatus
	ConversationParticipantType        = runtime.ConversationParticipantType
	ConversationParticipant            = runtime.ConversationParticipant
	ConversationMessageIntent          = runtime.ConversationMessageIntent
	ConversationAudienceKind           = runtime.ConversationAudienceKind
	ConversationAudience               = runtime.ConversationAudience
	ConversationReferenceKind          = runtime.ConversationReferenceKind
	ConversationReference              = runtime.ConversationReference
	ConversationArchiveSource          = runtime.ConversationArchiveSource
	ConversationArchiveMessage         = runtime.ConversationArchiveMessage
	ImportConversationArchiveRequest   = runtime.ImportConversationArchiveRequest
	ConversationArchiveImportResult    = runtime.ConversationArchiveImportResult
	ChannelMessage                     = runtime.ChannelMessage
	ConversationCursor                 = runtime.ConversationCursor
	ConversationPresenceState          = runtime.ConversationPresenceState
	ConversationPresence               = runtime.ConversationPresence
	ParticipationSignals               = runtime.ParticipationSignals
	ParticipationProposal              = runtime.ParticipationProposal
	ParticipationDisposition           = runtime.ParticipationDisposition
	ParticipationReason                = runtime.ParticipationReason
	ParticipationDecision              = runtime.ParticipationDecision
	ConversationArbitrationPolicy      = runtime.ConversationArbitrationPolicy
	ConversationArbitration            = runtime.ConversationArbitration
	ParticipationRoundStatus           = runtime.ParticipationRoundStatus
	ParticipationRound                 = runtime.ParticipationRound
	ConversationParticipantBinding     = runtime.ConversationParticipantBinding
	ConversationParticipantQuery       = runtime.ConversationParticipantQuery
	ConversationParticipantSource      = runtime.ConversationParticipantSource
	ConversationParticipantSourceFunc  = runtime.ConversationParticipantSourceFunc
	ParticipationProposalContext       = runtime.ParticipationProposalContext
	ParticipationProposalProvider      = runtime.ParticipationProposalProvider
	ParticipationProposalProviderFunc  = runtime.ParticipationProposalProviderFunc
	ConversationCoordinationRequest    = runtime.ConversationCoordinationRequest
	ConversationCoordinatorConfig      = runtime.ConversationCoordinatorConfig
	ConversationRunSchedulerConfig     = runtime.ConversationRunSchedulerConfig
	ConversationRunTurnRunnerConfig    = runtime.ConversationRunTurnRunnerConfig
	ConversationRunReconcilerConfig    = runtime.ConversationRunReconcilerConfig
	ConversationRunReconcileResult     = runtime.ConversationRunReconcileResult
	ConversationChangeRequest          = runtime.ConversationChangeRequest
	ConversationChangeSet              = runtime.ConversationChangeSet
	CreateConversationRequest          = runtime.CreateConversationRequest
	ConversationFilter                 = runtime.ConversationFilter
	PostChannelMessageRequest          = runtime.PostChannelMessageRequest
	ChannelMessageFilter               = runtime.ChannelMessageFilter
	CoordinateParticipationRequest     = runtime.CoordinateParticipationRequest
	ParticipationRoundFilter           = runtime.ParticipationRoundFilter
	AdvanceConversationCursorRequest   = runtime.AdvanceConversationCursorRequest
	SetConversationPresenceRequest     = runtime.SetConversationPresenceRequest
	ReleaseConversationPresenceRequest = runtime.ReleaseConversationPresenceRequest
	ChannelMessageCommitResult         = runtime.ChannelMessageCommitResult
	ParticipationRoundResult           = runtime.ParticipationRoundResult
	CollaborationStore                 = runtime.CollaborationStore
	CollaborationKernelStore           = runtime.CollaborationKernelStore
	CollaborationParty                 = runtime.CollaborationParty
	ArtifactRequirement                = runtime.ArtifactRequirement
	ArtifactReference                  = runtime.ArtifactReference
	Artifact                           = runtime.Artifact
	ArtifactClassification             = runtime.ArtifactClassification
	ArtifactRetention                  = runtime.ArtifactRetention
	ArtifactProvenance                 = runtime.ArtifactProvenance
	ArtifactEvidenceLink               = runtime.EvidenceLink
	ArtifactEvidenceRelation           = runtime.EvidenceRelation
	ArtifactEvidenceTargetKind         = runtime.EvidenceTargetKind
	ArtifactFilter                     = runtime.ArtifactFilter
	ArtifactStore                      = runtime.ArtifactStore
	ArtifactCatalog                    = runtime.ArtifactCatalog
	RegisterArtifactRequest            = runtime.RegisterArtifactRequest
	ArtifactRegistrationResult         = runtime.ArtifactRegistrationResult
	ArtifactContentStore               = runtime.ArtifactContentStore
	ArtifactContentResolver            = runtime.ArtifactContentResolver
	ArtifactContentWrite               = runtime.ArtifactContentWrite
	ArtifactStoredContent              = runtime.ArtifactStoredContent
	ArtifactContentResolutionRequest   = runtime.ArtifactContentResolutionRequest
	ArtifactContentResolution          = runtime.ArtifactContentResolution
	AgentRequest                       = runtime.AgentRequest
	AgentRequestKind                   = runtime.AgentRequestKind
	AgentRequestStatus                 = runtime.AgentRequestStatus
	AgentRequestDecision               = runtime.AgentRequestDecision
	AgentRequestFilter                 = runtime.AgentRequestFilter
	CreateAgentRequestRequest          = runtime.CreateAgentRequestRequest
	AgentRequestGroupSpec              = runtime.AgentRequestGroupSpec
	CreateAgentRequestGroupRequest     = runtime.CreateAgentRequestGroupRequest
	AgentRequestGroupResult            = runtime.AgentRequestGroupResult
	RespondAgentRequestRequest         = runtime.RespondAgentRequestRequest
	CompleteAgentRequestRequest        = runtime.CompleteAgentRequestRequest
	AgentRequestResult                 = runtime.AgentRequestResult
	AgentTurnStore                     = runtime.AgentTurnStore
	AgentTurn                          = runtime.AgentTurn
	AgentTurnStatus                    = runtime.AgentTurnStatus
	AgentTurnFilter                    = runtime.AgentTurnFilter
	TurnDecision                       = runtime.TurnDecision
	TurnAction                         = runtime.TurnAction
	TurnUsage                          = runtime.TurnUsage
	BeginAgentTurnRequest              = runtime.BeginAgentTurnRequest
	FinishAgentTurnRequest             = runtime.FinishAgentTurnRequest
	TurnExecutionContext               = runtime.TurnExecutionContext
	TurnRunner                         = runtime.TurnRunner
	TurnRunnerFunc                     = runtime.TurnRunnerFunc
	TurnOutcome                        = runtime.TurnOutcome
	AdvanceAgentRunRequest             = runtime.AdvanceAgentRunRequest
	AdvanceAgentRunResult              = runtime.AdvanceAgentRunResult
	AgentRunScheduleStore              = runtime.AgentRunScheduleStore
	AgentRunClaimRequest               = runtime.AgentRunClaimRequest
	AgentRunWorkerConfig               = runtime.AgentRunWorkerConfig
	DynamicAgentRunWorkerConfig        = runtime.DynamicAgentRunWorkerConfig
	TurnRunnerBinding                  = runtime.TurnRunnerBinding
	TurnRunnerResolver                 = runtime.TurnRunnerResolver
	TurnRunnerResolverFunc             = runtime.TurnRunnerResolverFunc
	WorkerScopeSource                  = runtime.WorkerScopeSource
	WorkerScopeSourceFunc              = runtime.WorkerScopeSourceFunc
	WakeSignal                         = runtime.WakeSignal
	WokenRun                           = runtime.WokenRun
	WakeResult                         = runtime.WakeResult
	SkillCatalog                       = skill.Catalog
	SkillDefinition                    = skill.Definition
	SkillAction                        = skill.Action
	SkillBinding                       = skill.Binding
	SkillScope                         = skill.ScopeReference
	SkillRiskLevel                     = skill.RiskLevel
	SkillSideEffect                    = skill.SideEffect
	SkillIdempotencyMode               = skill.IdempotencyMode
	SkillCredentialRequirement         = skill.CredentialRequirement
	SkillCredentialReference           = skill.CredentialReference
	SkillTransportReference            = skill.TransportReference
	SkillTransportArgument             = skill.TransportArgument
	SkillArgumentRule                  = skill.ArgumentRule
	SkillActionRetryPolicy             = skill.ActionRetryPolicy
	ModelSkillAction                   = skill.ModelAction
	ModelSkillPrompt                   = skill.ModelPrompt
	BoundSkillAction                   = skill.BoundAction
	SkillPromptModule                  = skill.PromptModule
	SkillHostCapabilityState           = skill.HostCapabilityState
	SkillAdapterState                  = skill.AdapterState
	SkillAdapterCapability             = skill.AdapterCapability
	SkillResourceStageRequest          = skill.ResourceStageRequest
	SkillResourceStage                 = skill.ResourceStage
	SkillResourceStager                = skill.ResourceStager
	SkillResourceContentProvider       = skill.ResourceContentProvider
	SkillAvailabilityReason            = skill.AvailabilityReason
	ActivatedSkill                     = skill.ActivatedSkill
	UnavailableSkill                   = skill.UnavailableSkill
	SkillActivationSnapshot            = skill.ActivationSnapshot
	SkillCatalogStore                  = skill.CatalogStore
	ActionStore                        = runtime.ActionStore
	ActionCall                         = runtime.ActionCall
	ActionCallStatus                   = runtime.ActionCallStatus
	ActionFilter                       = runtime.ActionFilter
	ActionDisposition                  = runtime.ActionDisposition
	ActionPolicyInput                  = runtime.ActionPolicyInput
	ActionPolicyDecision               = runtime.ActionPolicyDecision
	ActionPolicyEvaluator              = runtime.ActionPolicyEvaluator
	ActionPolicyEvaluatorFunc          = runtime.ActionPolicyEvaluatorFunc
	ProposeActionRequest               = runtime.ProposeActionRequest
	ActionProposalResult               = runtime.ActionProposalResult
	ApprovalCheckpoint                 = runtime.ApprovalCheckpoint
	ApprovalStatus                     = runtime.ApprovalStatus
	ApprovalPrincipal                  = runtime.ApprovalPrincipal
	ApprovalFilter                     = runtime.ApprovalFilter
	ApprovalAuthorizer                 = runtime.ApprovalAuthorizer
	ApprovalAuthorizerFunc             = runtime.ApprovalAuthorizerFunc
	ResolveApprovalRequest             = runtime.ResolveApprovalRequest
	ApprovalResolutionResult           = runtime.ApprovalResolutionResult
	ActionWorkerConfig                 = runtime.ActionWorkerConfig
	DynamicActionWorkerConfig          = runtime.DynamicActionWorkerConfig
	CredentialResolver                 = runtime.CredentialResolver
	CredentialResolverFunc             = runtime.CredentialResolverFunc
	CredentialResolutionRequest        = runtime.CredentialResolutionRequest
	ActionDispatchInput                = runtime.ActionDispatchInput
	ActionDispatcher                   = runtime.ActionDispatcher
	ActionDispatcherFunc               = runtime.ActionDispatcherFunc
	ToolInvoker                        = runtime.ToolInvoker
	ToolInvokerFunc                    = runtime.ToolInvokerFunc
	ToolInvocation                     = runtime.ToolInvocation
	ToolActionDispatcher               = runtime.ToolActionDispatcher
	OpenClawSkillSource                = skillopenclaw.Source
	OpenClawSkillFile                  = skillopenclaw.File
	OpenClawSkillBundle                = skillopenclaw.Bundle
	OpenClawSkillDiagnostic            = skillopenclaw.Diagnostic
	OpenClawSkillCompilation           = skillopenclaw.Compilation
	SkillSourceRootKind                = skillsource.RootKind
	SkillSourceRoot                    = skillsource.Root
	SkillSourceCandidate               = skillsource.Candidate
	SkillSourceShadowed                = skillsource.ShadowedCandidate
	SkillSourceDiagnostic              = skillsource.Diagnostic
	SkillSourceSnapshot                = skillsource.Snapshot
	SkillSourceCatalog                 = skillsource.Catalog
	SkillSourceChange                  = skillsource.Change
	SkillSourceWatcher                 = skillsource.Watcher
	ClawHubRegistry                    = clawhub.Registry
	ClawHubClient                      = clawhub.ClawHubClient
	ClawHubSkillReference              = clawhub.SkillReference
	ClawHubSearchRequest               = clawhub.SearchRequest
	ClawHubExploreRequest              = clawhub.ExploreRequest
	ClawHubSkillPage                   = clawhub.SkillPage
	ClawHubSkillSummary                = clawhub.SkillSummary
	ClawHubSkillDetail                 = clawhub.SkillDetail
	ClawHubInstallRequest              = clawhub.InstallRequest
	ClawHubInstalledSkill              = clawhub.InstalledSkill
	ClawHubVerification                = clawhub.Verification
	ClawHubVersionPage                 = clawhub.VersionPage
	ClawHubVersionDetail               = clawhub.VersionDetail
	ClawHubDownloadedArchive           = clawhub.DownloadedArchive
)

// PersistentKernelStore is the complete durable control-plane contract for an
// embedded OpenSeal engine. Enterprise adapters implement this interface so
// runtime state, agent definitions/deployments, and skill bindings share one
// authoritative persistence boundary.
type PersistentKernelStore interface {
	runtime.KernelStore
	runtime.CollaborationStore
	runtime.RunDependencyStore
	runtime.ConversationStore
	runtime.ArtifactStore
	kernelagent.Store
	kernelteam.Store
	skill.CatalogStore
}

var _ PersistentKernelStore = (*runtime.PostgresStore)(nil)

var (
	ErrRunNotFound                         = runtime.ErrRunNotFound
	ErrRevisionConflict                    = runtime.ErrRevisionConflict
	ErrInvalidRunTransition                = runtime.ErrInvalidRunTransition
	ErrRunIdempotency                      = runtime.ErrRunIdempotency
	ErrInvalidAgentRun                     = runtime.ErrInvalidAgentRun
	ErrInvalidRunCommand                   = runtime.ErrInvalidRunCommand
	ErrInvalidScope                        = runtime.ErrInvalidScope
	ErrInvalidOwner                        = runtime.ErrInvalidOwner
	ErrObjectiveNotFound                   = runtime.ErrObjectiveNotFound
	ErrObjectiveIdempotency                = runtime.ErrObjectiveIdempotency
	ErrInvalidObjectiveTransition          = runtime.ErrInvalidObjectiveTransition
	ErrBudgetExhausted                     = runtime.ErrBudgetExhausted
	ErrAgentRequestNotFound                = runtime.ErrAgentRequestNotFound
	ErrInvalidAgentRequestState            = runtime.ErrInvalidAgentRequestState
	ErrAgentRequestUnauthorized            = runtime.ErrAgentRequestUnauthorized
	ErrAgentRequestIdempotency             = runtime.ErrAgentRequestIdempotency
	ErrUnsafeSharedContext                 = runtime.ErrUnsafeSharedContext
	ErrInvalidArtifact                     = runtime.ErrInvalidArtifact
	ErrArtifactNotFound                    = runtime.ErrArtifactNotFound
	ErrArtifactImmutable                   = runtime.ErrArtifactImmutable
	ErrArtifactVersionConflict             = runtime.ErrArtifactVersionConflict
	ErrInvalidArtifactRecord               = runtime.ErrInvalidArtifactRecord
	ErrRunDependencyNotFound               = runtime.ErrRunDependencyNotFound
	ErrDependencyGroupNotFound             = runtime.ErrDependencyGroupNotFound
	ErrInvalidRunDependency                = runtime.ErrInvalidRunDependency
	ErrDependencyConflict                  = runtime.ErrDependencyConflict
	ErrConversationNotFound                = runtime.ErrConversationNotFound
	ErrChannelMessageNotFound              = runtime.ErrChannelMessageNotFound
	ErrParticipationRoundNotFound          = runtime.ErrParticipationRoundNotFound
	ErrInvalidConversation                 = runtime.ErrInvalidConversation
	ErrMessageConflict                     = runtime.ErrMessageConflict
	ErrConversationCursorConflict          = runtime.ErrConversationCursorConflict
	ErrConversationPresenceConflict        = runtime.ErrConversationPresenceConflict
	ErrConversationCoordinationUnavailable = runtime.ErrConversationCoordinationUnavailable
	ErrNoConversationParticipants          = runtime.ErrNoConversationParticipants
	ErrTeamDefinitionNotFound              = kernelteam.ErrDefinitionNotFound
	ErrTeamDeploymentNotFound              = kernelteam.ErrDeploymentNotFound
	ErrTeamDeploymentRevisionConflict      = kernelteam.ErrRevisionConflict
)

func NewToolActionDispatcher(invoker runtime.ToolInvoker) (*runtime.ToolActionDispatcher, error) {
	return runtime.NewToolActionDispatcher(invoker)
}

func ValidateCredentialFreeContext(value interface{}) error {
	return runtime.ValidateCredentialFreeContext(value)
}

func EvaluateBudget(policy BudgetPolicy, usage BudgetUsage) (BudgetState, []string, error) {
	return runtime.EvaluateBudget(policy, usage)
}

func DefaultConversationArbitrationPolicy() runtime.ConversationArbitrationPolicy {
	return runtime.DefaultConversationArbitrationPolicy()
}

func DefaultConversationCoordinatorConfig() runtime.ConversationCoordinatorConfig {
	return runtime.DefaultConversationCoordinatorConfig()
}

func ArbitrateParticipation(roundID string, proposals []runtime.ParticipationProposal, recent []*runtime.ChannelMessage, policy runtime.ConversationArbitrationPolicy) (*runtime.ConversationArbitration, error) {
	return runtime.ArbitrateParticipation(roundID, proposals, recent, policy)
}

func ConversationMessageFingerprint(content string) string {
	return runtime.ConversationMessageFingerprint(content)
}

// NewPostgresStore opens the production-grade shared persistence adapter and
// applies OpenSeal's versioned schema migrations.
func NewPostgresStore(ctx context.Context, dsn string, options ...runtime.PostgresStoreOption) (*runtime.PostgresStore, error) {
	return runtime.NewPostgresStore(ctx, dsn, options...)
}

func WithPostgresSchema(schema string) runtime.PostgresStoreOption {
	return runtime.WithPostgresSchema(schema)
}

func CompileOpenClawSkill(bundle skillopenclaw.Bundle) (*skillopenclaw.Compilation, error) {
	return skillopenclaw.Compile(bundle)
}

func ExportOpenClawSkill(compilation *skillopenclaw.Compilation) (skillopenclaw.Bundle, error) {
	return skillopenclaw.ExportBundle(compilation)
}

func NewSkillSourceCatalog() *skillsource.Catalog {
	return skillsource.NewCatalog()
}

func NewSkillSourceWatcher(catalog *skillsource.Catalog, roots []skillsource.Root, interval time.Duration) (*skillsource.Watcher, error) {
	return skillsource.NewWatcher(catalog, roots, interval)
}

const (
	OwnerTypeAgent = runtime.OwnerTypeAgent
	OwnerTypeTeam  = runtime.OwnerTypeTeam

	ObjectiveStatusDraft     = runtime.ObjectiveStatusDraft
	ObjectiveStatusActive    = runtime.ObjectiveStatusActive
	ObjectiveStatusPaused    = runtime.ObjectiveStatusPaused
	ObjectiveStatusSatisfied = runtime.ObjectiveStatusSatisfied
	ObjectiveStatusFailed    = runtime.ObjectiveStatusFailed
	ObjectiveStatusRetired   = runtime.ObjectiveStatusRetired
	ObjectiveCadenceInterval = runtime.ObjectiveCadenceInterval
	ObjectiveCadenceDaily    = runtime.ObjectiveCadenceDaily
	ObjectiveCadenceWeekly   = runtime.ObjectiveCadenceWeekly

	RunSourceManual    = runtime.RunSourceManual
	RunSourceChat      = runtime.RunSourceChat
	RunSourceSchedule  = runtime.RunSourceSchedule
	RunSourceEvent     = runtime.RunSourceEvent
	RunSourceWebhook   = runtime.RunSourceWebhook
	RunSourceRequest   = runtime.RunSourceRequest
	RunSourceHandoff   = runtime.RunSourceHandoff
	RunSourceObjective = runtime.RunSourceObjective

	RunKindAgentWork    = runtime.RunKindAgentWork
	RunKindConversation = runtime.RunKindConversation

	TeamCoordinationDynamic           = kernelteam.CoordinationDynamic
	TeamCoordinationPeer              = kernelteam.CoordinationPeer
	TeamCoordinationLeaderFacilitated = kernelteam.CoordinationLeaderFacilitated
	TeamRoleChannelActive             = kernelteam.RoleChannelActive
	TeamRoleChannelObserveOnly        = kernelteam.RoleChannelObserveOnly
	TeamRoleChannelDisabled           = kernelteam.RoleChannelDisabled
	TeamDeploymentDraft               = kernelteam.DeploymentDraft
	TeamDeploymentActive              = kernelteam.DeploymentActive
	TeamDeploymentPaused              = kernelteam.DeploymentPaused
	TeamDeploymentArchived            = kernelteam.DeploymentArchived
	TeamAmendmentEvaluating           = kernelteam.AmendmentEvaluating
	TeamAmendmentAwaitingApproval     = kernelteam.AmendmentAwaitingApproval
	TeamAmendmentReady                = kernelteam.AmendmentReady
	TeamAmendmentApproved             = kernelteam.AmendmentApproved
	TeamAmendmentRejected             = kernelteam.AmendmentRejected
	TeamAmendmentEvaluationFailed     = kernelteam.AmendmentEvaluationFailed
	TeamAmendmentActivated            = kernelteam.AmendmentActivated

	AgentRunStatusQueued               = runtime.AgentRunStatusQueued
	AgentRunStatusPlanning             = runtime.AgentRunStatusPlanning
	AgentRunStatusRunning              = runtime.AgentRunStatusRunning
	AgentRunStatusPaused               = runtime.AgentRunStatusPaused
	AgentRunStatusSleeping             = runtime.AgentRunStatusSleeping
	AgentRunStatusWaitingForDependency = runtime.AgentRunStatusWaitingForDependency
	AgentRunStatusWaitingForAgent      = runtime.AgentRunStatusWaitingForAgent
	AgentRunStatusWaitingForApproval   = runtime.AgentRunStatusWaitingForApproval
	AgentRunStatusWaitingForEvent      = runtime.AgentRunStatusWaitingForEvent
	AgentRunStatusCompleted            = runtime.AgentRunStatusCompleted
	AgentRunStatusFailed               = runtime.AgentRunStatusFailed
	AgentRunStatusCanceled             = runtime.AgentRunStatusCanceled

	BudgetStateActive    = runtime.BudgetStateActive
	BudgetStateWarning   = runtime.BudgetStateWarning
	BudgetStateExhausted = runtime.BudgetStateExhausted

	AgentRunCommandPause     = runtime.AgentRunCommandPause
	AgentRunCommandResume    = runtime.AgentRunCommandResume
	AgentRunCommandCancel    = runtime.AgentRunCommandCancel
	AgentRunCommandIntervene = runtime.AgentRunCommandIntervene

	ActivitySeverityDebug   = runtime.ActivitySeverityDebug
	ActivitySeverityInfo    = runtime.ActivitySeverityInfo
	ActivitySeverityWarning = runtime.ActivitySeverityWarning
	ActivitySeverityError   = runtime.ActivitySeverityError

	ActivityVisibilityPrivate = runtime.ActivityVisibilityPrivate
	ActivityVisibilityTeam    = runtime.ActivityVisibilityTeam
	ActivityVisibilityScope   = runtime.ActivityVisibilityScope

	AgentRequestKindRequest = runtime.AgentRequestKindRequest
	AgentRequestKindHandoff = runtime.AgentRequestKindHandoff

	AgentRequestStatusPending                = runtime.AgentRequestStatusPending
	AgentRequestStatusClarificationRequested = runtime.AgentRequestStatusClarificationRequested
	AgentRequestStatusAccepted               = runtime.AgentRequestStatusAccepted
	AgentRequestStatusCompleted              = runtime.AgentRequestStatusCompleted
	AgentRequestStatusRejected               = runtime.AgentRequestStatusRejected
	AgentRequestStatusCanceled               = runtime.AgentRequestStatusCanceled

	AgentRequestDecisionAccept               = runtime.AgentRequestDecisionAccept
	AgentRequestDecisionReject               = runtime.AgentRequestDecisionReject
	AgentRequestDecisionRequestClarification = runtime.AgentRequestDecisionRequestClarification
	AgentRequestDecisionProvideClarification = runtime.AgentRequestDecisionProvideClarification

	ConversationStatusActive   = runtime.ConversationStatusActive
	ConversationStatusArchived = runtime.ConversationStatusArchived

	ConversationParticipantUser    = runtime.ConversationParticipantUser
	ConversationParticipantAgent   = runtime.ConversationParticipantAgent
	ConversationParticipantTeam    = runtime.ConversationParticipantTeam
	ConversationParticipantService = runtime.ConversationParticipantService

	MessageIntentQuestion        = runtime.MessageIntentQuestion
	MessageIntentAnswer          = runtime.MessageIntentAnswer
	MessageIntentUpdate          = runtime.MessageIntentUpdate
	MessageIntentProposal        = runtime.MessageIntentProposal
	MessageIntentDecision        = runtime.MessageIntentDecision
	MessageIntentObjection       = runtime.MessageIntentObjection
	MessageIntentHandoff         = runtime.MessageIntentHandoff
	MessageIntentApprovalRequest = runtime.MessageIntentApprovalRequest
	MessageIntentAcknowledgment  = runtime.MessageIntentAcknowledgment
	MessageIntentSystem          = runtime.MessageIntentSystem

	ConversationAudienceChannel      = runtime.ConversationAudienceChannel
	ConversationAudienceParticipants = runtime.ConversationAudienceParticipants
	ConversationAudienceRoles        = runtime.ConversationAudienceRoles

	ConversationReferenceObjective      = runtime.ConversationReferenceObjective
	ConversationReferenceRun            = runtime.ConversationReferenceRun
	ConversationReferenceRequest        = runtime.ConversationReferenceRequest
	ConversationReferenceApproval       = runtime.ConversationReferenceApproval
	ConversationReferenceArtifact       = runtime.ConversationReferenceArtifact
	ConversationReferenceActivity       = runtime.ConversationReferenceActivity
	ConversationReferenceExternalSource = runtime.ConversationReferenceExternalSource

	ConversationPresenceTyping  = runtime.ConversationPresenceTyping
	ConversationPresenceWorking = runtime.ConversationPresenceWorking

	ParticipationSpeak    = runtime.ParticipationSpeak
	ParticipationSilent   = runtime.ParticipationSilent
	ParticipationDeferred = runtime.ParticipationDeferred

	ParticipationReasonDirectMention        = runtime.ParticipationReasonDirectMention
	ParticipationReasonAnswersQuestion      = runtime.ParticipationReasonAnswersQuestion
	ParticipationReasonNewInformation       = runtime.ParticipationReasonNewInformation
	ParticipationReasonRoleRelevant         = runtime.ParticipationReasonRoleRelevant
	ParticipationReasonEvidenceBacked       = runtime.ParticipationReasonEvidenceBacked
	ParticipationReasonResolvesWork         = runtime.ParticipationReasonResolvesWork
	ParticipationReasonCoordinatesWork      = runtime.ParticipationReasonCoordinatesWork
	ParticipationReasonSubstantiveObjection = runtime.ParticipationReasonSubstantiveObjection
	ParticipationReasonRequestedSilence     = runtime.ParticipationReasonRequestedSilence
	ParticipationReasonNoNewInformation     = runtime.ParticipationReasonNoNewInformation
	ParticipationReasonLowRelevance         = runtime.ParticipationReasonLowRelevance
	ParticipationReasonDuplicate            = runtime.ParticipationReasonDuplicate
	ParticipationReasonBackpressure         = runtime.ParticipationReasonBackpressure
	ParticipationReasonAcknowledgmentOnly   = runtime.ParticipationReasonAcknowledgmentOnly

	ParticipationRoundCommitted = runtime.ParticipationRoundCommitted

	RunDependencyKindRun          = runtime.RunDependencyKindRun
	RunDependencyKindAgentRequest = runtime.RunDependencyKindAgentRequest
	RunDependencyKindHandoff      = runtime.RunDependencyKindHandoff
	RunDependencyKindAction       = runtime.RunDependencyKindAction
	RunDependencyKindApproval     = runtime.RunDependencyKindApproval

	RunDependencyStatePending   = runtime.RunDependencyStatePending
	RunDependencyStateRunning   = runtime.RunDependencyStateRunning
	RunDependencyStateSatisfied = runtime.RunDependencyStateSatisfied
	RunDependencyStateFailed    = runtime.RunDependencyStateFailed
	RunDependencyStateCanceled  = runtime.RunDependencyStateCanceled

	FanInModeAll    = runtime.FanInModeAll
	FanInModeAny    = runtime.FanInModeAny
	FanInModeQuorum = runtime.FanInModeQuorum

	DependencyFailureFailFast = runtime.DependencyFailureFailFast
	DependencyFailureWait     = runtime.DependencyFailureWait

	RunDependencyGroupOpen      = runtime.RunDependencyGroupOpen
	RunDependencyGroupWaiting   = runtime.RunDependencyGroupWaiting
	RunDependencyGroupSatisfied = runtime.RunDependencyGroupSatisfied
	RunDependencyGroupFailed    = runtime.RunDependencyGroupFailed
	RunDependencyGroupCanceled  = runtime.RunDependencyGroupCanceled

	AgentTurnStatusRunning   = runtime.AgentTurnStatusRunning
	AgentTurnStatusCompleted = runtime.AgentTurnStatusCompleted
	AgentTurnStatusFailed    = runtime.AgentTurnStatusFailed
	AgentTurnStatusCanceled  = runtime.AgentTurnStatusCanceled

	SkillRiskRead        = skill.RiskLevelRead
	SkillRiskWrite       = skill.RiskLevelWrite
	SkillRiskExternal    = skill.RiskLevelExternal
	SkillRiskProduction  = skill.RiskLevelProduction
	SkillRiskDestructive = skill.RiskLevelDestructive

	SkillSideEffectNone        = skill.SideEffectNone
	SkillSideEffectRead        = skill.SideEffectRead
	SkillSideEffectWrite       = skill.SideEffectWrite
	SkillSideEffectExternal    = skill.SideEffectExternal
	SkillSideEffectDestructive = skill.SideEffectDestructive

	SkillIdempotencyNone      = skill.IdempotencyNone
	SkillIdempotencySupported = skill.IdempotencySupported
	SkillIdempotencyRequired  = skill.IdempotencyRequired

	SkillAdapterAvailable   = skill.AdapterStateAvailable
	SkillAdapterUnavailable = skill.AdapterStateUnavailable
	SkillAdapterDegraded    = skill.AdapterStateDegraded

	SkillAdapterLocal           = skill.AdapterLocal
	SkillAdapterGit             = skill.AdapterGit
	SkillAdapterPlugin          = skill.AdapterPlugin
	SkillAdapterInstaller       = skill.AdapterInstaller
	SkillAdapterWatcher         = skill.AdapterWatcher
	SkillAdapterRemoteNode      = skill.AdapterRemoteNode
	SkillAdapterResourceStaging = skill.AdapterResourceStaging

	ActionDispositionAllow           = runtime.ActionDispositionAllow
	ActionDispositionDeny            = runtime.ActionDispositionDeny
	ActionDispositionRequireApproval = runtime.ActionDispositionRequireApproval

	ActionCallStatusReady           = runtime.ActionCallStatusReady
	ActionCallStatusWaitingApproval = runtime.ActionCallStatusWaitingApproval
	ActionCallStatusDenied          = runtime.ActionCallStatusDenied
	ActionCallStatusRunning         = runtime.ActionCallStatusRunning
	ActionCallStatusSucceeded       = runtime.ActionCallStatusSucceeded
	ActionCallStatusFailed          = runtime.ActionCallStatusFailed

	ApprovalStatusPending  = runtime.ApprovalStatusPending
	ApprovalStatusApproved = runtime.ApprovalStatusApproved
	ApprovalStatusRejected = runtime.ApprovalStatusRejected
	ApprovalStatusExpired  = runtime.ApprovalStatusExpired

	SkillSourceWorkspace    = skillsource.RootWorkspace
	SkillSourceProjectAgent = skillsource.RootProjectAgent
	SkillSourcePersonal     = skillsource.RootPersonal
	SkillSourceManaged      = skillsource.RootManaged
	SkillSourceBundled      = skillsource.RootBundled
	SkillSourcePlugin       = skillsource.RootPlugin
	SkillSourceExtra        = skillsource.RootExtra

	AgentRolloutPending  = kernelagent.RolloutPending
	AgentRolloutActive   = kernelagent.RolloutActive
	AgentRolloutDegraded = kernelagent.RolloutDegraded
	AgentRolloutPaused   = kernelagent.RolloutPaused
	AgentRolloutRetired  = kernelagent.RolloutRetired

	AgentAmendmentEvaluating       = kernelagent.AmendmentEvaluating
	AgentAmendmentAwaitingApproval = kernelagent.AmendmentAwaitingApproval
	AgentAmendmentReady            = kernelagent.AmendmentReady
	AgentAmendmentApproved         = kernelagent.AmendmentApproved
	AgentAmendmentRejected         = kernelagent.AmendmentRejected
	AgentAmendmentEvaluationFailed = kernelagent.AmendmentEvaluationFailed
	AgentAmendmentActivated        = kernelagent.AmendmentActivated

	ArtifactClassPublic       = runtime.ArtifactClassificationPublic
	ArtifactClassInternal     = runtime.ArtifactClassificationInternal
	ArtifactClassConfidential = runtime.ArtifactClassificationConfidential
	ArtifactClassRestricted   = runtime.ArtifactClassificationRestricted

	ArtifactEvidenceDerivedFrom = runtime.EvidenceRelationDerivedFrom
	ArtifactEvidenceSupports    = runtime.EvidenceRelationSupports
	ArtifactEvidenceContradicts = runtime.EvidenceRelationContradicts
	ArtifactEvidenceCites       = runtime.EvidenceRelationCites
	ArtifactEvidenceInputTo     = runtime.EvidenceRelationInputTo
	ArtifactEvidenceOutputOf    = runtime.EvidenceRelationOutputOf

	ArtifactEvidenceTargetArtifact       = runtime.EvidenceTargetArtifact
	ArtifactEvidenceTargetExternalSource = runtime.EvidenceTargetExternalSource
	ArtifactEvidenceTargetRun            = runtime.EvidenceTargetRun
	ArtifactEvidenceTargetActivity       = runtime.EvidenceTargetActivity
	ArtifactEvidenceTargetRequest        = runtime.EvidenceTargetRequest
	ArtifactEvidenceTargetAction         = runtime.EvidenceTargetAction
	ArtifactEvidenceTargetTurn           = runtime.EvidenceTargetTurn
	ArtifactEvidenceTargetClaim          = runtime.EvidenceTargetClaim
)

// Engine is the primary entry point for OpenSeal.
// It wires together the registry, execution store, worker pool, and scheduler.
type Engine struct {
	registry                      *executor.Registry
	store                         runtime.KernelStore
	pool                          *runtime.WorkerPool
	scheduler                     *runtime.Scheduler
	portfolio                     *runtime.PortfolioService
	activity                      *runtime.RunActivityService
	dependencies                  *runtime.DependencyCoordinator
	conversations                 *runtime.ConversationService
	conversationCoordinator       *runtime.ConversationCoordinator
	conversationParticipants      runtime.ConversationParticipantSource
	participationProposals        runtime.ParticipationProposalProvider
	conversationCoordinatorConfig runtime.ConversationCoordinatorConfig
	conversationRunScheduler      *runtime.ConversationRunScheduler
	conversationRunReconciler     *runtime.ConversationRunReconciler
	conversationRunConfig         *ConversationRunConfig
	conversationRunScopes         runtime.WorkerScopeSource
	conversationChanges           *runtime.ConversationChangeService
	collaboration                 *runtime.CollaborationService
	turns                         *runtime.AgentTurnService
	turnsRun                      *runtime.TurnCoordinator
	runQueue                      *runtime.AgentRunScheduler
	wake                          *runtime.AgentRunWakeService
	actions                       *runtime.ActionCoordinator
	approvals                     *runtime.ApprovalCoordinator
	artifacts                     *runtime.ArtifactCatalog
	actionPolicy                  runtime.ActionPolicyEvaluator
	approvalAuth                  runtime.ApprovalAuthorizer
	clawHub                       *clawhub.InstallManager
	clawHubRegistry               clawhub.Registry
	agentPoolSpecs                []agentRunWorkerSpec
	agentPools                    []*runtime.AgentRunWorkerPool
	agentSupervisorSpecs          []agentRunWorkerSupervisorSpec
	agentSupervisors              []*runtime.AgentRunWorkerSupervisor
	actionPoolSpecs               []actionWorkerSpec
	actionPools                   []*runtime.ActionWorkerPool
	actionSupervisorSpecs         []actionWorkerSupervisorSpec
	actionSupervisors             []*runtime.ActionWorkerSupervisor
	skills                        *skill.Catalog
	agents                        *kernelagent.Registry
	teams                         *kernelteam.Registry
	logger                        *zap.SugaredLogger
}

type agentRunWorkerSpec struct {
	config   runtime.AgentRunWorkerConfig
	resolver runtime.TurnRunnerResolver
}

type agentRunWorkerSupervisorSpec struct {
	config   runtime.DynamicAgentRunWorkerConfig
	source   runtime.WorkerScopeSource
	resolver runtime.TurnRunnerResolver
}

// ConversationRunConfig makes Team channel participation durable and
// server-owned. Worker concurrency controls channels processed per scope;
// each channel itself is always serialized across replicas.
type ConversationRunConfig struct {
	Scheduler  runtime.ConversationRunSchedulerConfig
	Runner     runtime.ConversationRunTurnRunnerConfig
	Reconciler runtime.ConversationRunReconcilerConfig
	Workers    runtime.DynamicAgentRunWorkerConfig
}

type actionWorkerSpec struct {
	config      runtime.ActionWorkerConfig
	credentials runtime.CredentialResolver
	dispatcher  runtime.ActionDispatcher
}

type actionWorkerSupervisorSpec struct {
	config      runtime.DynamicActionWorkerConfig
	source      runtime.WorkerScopeSource
	credentials runtime.CredentialResolver
	dispatcher  runtime.ActionDispatcher
}

// Option configures an Engine.
type Option func(*Engine) error

// New creates an Engine with the given options.
// Defaults: MemoryStore (100 runs), 4 workers, default retry policy.
func New(opts ...Option) (*Engine, error) {
	logger, _ := zap.NewProduction()
	sugar := logger.Sugar()

	reg := executor.NewRegistry(nil)
	store := runtime.NewMemoryStore(100)
	pool := runtime.NewWorkerPool(
		executor.NewPipelineExecutor(reg, sugar),
		store,
		sugar,
		4,
		runtime.DefaultRetryPolicy(),
	)
	conversationChanges, _ := runtime.NewConversationChangeService(store, store)

	agentRegistry := kernelagent.NewRegistry()
	e := &Engine{
		registry:            reg,
		store:               store,
		pool:                pool,
		scheduler:           runtime.NewScheduler(pool, store),
		portfolio:           runtime.NewPortfolioService(store),
		activity:            runtime.NewRunActivityService(store, store),
		dependencies:        runtime.NewDependencyCoordinator(store),
		conversations:       runtime.NewConversationService(store),
		conversationChanges: conversationChanges,
		collaboration:       runtime.NewCollaborationService(store),
		turns:               runtime.NewAgentTurnService(store, store),
		turnsRun:            runtime.NewTurnCoordinator(store, store, store),
		runQueue:            runtime.NewAgentRunScheduler(store),
		wake:                runtime.NewAgentRunWakeService(store, store),
		artifacts:           runtime.NewArtifactCatalog(store),
		skills:              skill.NewCatalog(),
		agents:              agentRegistry,
		teams:               kernelteam.NewRegistry(agentRegistry),
		actionPolicy:        runtime.NewDefaultActionPolicy(),
		approvalAuth:        runtime.EligibleApprovalAuthorizer{},
		logger:              sugar,
	}

	for _, opt := range opts {
		if err := opt(e); err != nil {
			return nil, fmt.Errorf("engine option: %w", err)
		}
	}
	if err := e.rebuildConversationCoordinator(); err != nil {
		return nil, fmt.Errorf("conversation coordinator configuration: %w", err)
	}
	if err := e.rebuildConversationRuns(); err != nil {
		return nil, fmt.Errorf("conversation Run configuration: %w", err)
	}
	if err := e.restoreClawHubSkills(); err != nil {
		return nil, fmt.Errorf("restore ClawHub skills: %w", err)
	}
	e.rebuildGovernance()
	if err := e.rebuildActionWorkerPools(); err != nil {
		return nil, fmt.Errorf("action worker configuration: %w", err)
	}
	if err := e.rebuildActionWorkerSupervisors(); err != nil {
		return nil, fmt.Errorf("dynamic action worker configuration: %w", err)
	}
	if err := e.rebuildAgentWorkerPools(); err != nil {
		return nil, fmt.Errorf("agent worker configuration: %w", err)
	}
	if err := e.rebuildAgentWorkerSupervisors(); err != nil {
		return nil, fmt.Errorf("dynamic agent worker configuration: %w", err)
	}

	return e, nil
}

// Start begins background goroutines (worker pool).
func (e *Engine) Start(ctx context.Context) {
	e.pool.Start(ctx)
	if e.conversationRunReconciler != nil {
		e.conversationRunReconciler.Start(ctx)
	}
	for _, pool := range e.agentPools {
		pool.Start(ctx)
	}
	for _, supervisor := range e.agentSupervisors {
		supervisor.Start(ctx)
	}
	for _, pool := range e.actionPools {
		pool.Start(ctx)
	}
	for _, supervisor := range e.actionSupervisors {
		supervisor.Start(ctx)
	}
}

// Stop gracefully shuts down background goroutines.
func (e *Engine) Stop() {
	if e.conversationRunReconciler != nil {
		e.conversationRunReconciler.Stop()
	}
	for _, supervisor := range e.agentSupervisors {
		supervisor.Stop()
	}
	for _, supervisor := range e.actionSupervisors {
		supervisor.Stop()
	}
	for _, pool := range e.actionPools {
		pool.Stop()
	}
	for _, pool := range e.agentPools {
		pool.Stop()
	}
	e.pool.Stop()
}

// ExecuteWorkflow runs a workflow synchronously and returns the result.
// For async execution, use ScheduleWorkflow.
func (e *Engine) ExecuteWorkflow(
	ctx context.Context,
	nodes []*executor.NodeDefinition,
	connections []*executor.ConnectionDefinition,
	startNodeID string,
	triggerData map[string]interface{},
) (*executor.ExecutionResult, error) {
	pe := executor.NewPipelineExecutor(e.registry, e.logger)
	return pe.Execute(ctx, 0, nodes, connections, startNodeID, triggerData, nil)
}

// ScheduleWorkflow enqueues a workflow for async execution.
// Returns the run ID immediately. Check the store for completion status.
func (e *Engine) ScheduleWorkflow(
	ctx context.Context,
	workflowName string,
	nodes []*executor.NodeDefinition,
	connections []*executor.ConnectionDefinition,
	startNodeID string,
	triggerData map[string]interface{},
) (int, error) {
	entry := runtime.WorkflowEntry{
		Name:        workflowName,
		Nodes:       nodes,
		Connections: connections,
		StartNodeID: startNodeID,
	}
	return e.scheduler.Schedule(ctx, entry, triggerData)
}

// Registry returns the executor registry for registering custom executors.
func (e *Engine) Registry() *executor.Registry {
	return e.registry
}

// Store returns the execution store.
func (e *Engine) Store() runtime.KernelStore {
	return e.store
}

// Logger returns the engine's logger.
func (e *Engine) Logger() *zap.SugaredLogger {
	return e.logger
}

// WithRegistry replaces the default registry.
func WithRegistry(reg *executor.Registry) Option {
	return func(e *Engine) error {
		e.registry = reg
		// Rebuild pool with new registry
		pe := executor.NewPipelineExecutor(reg, e.logger)
		e.pool = runtime.NewWorkerPool(pe, e.store, e.logger, 4, runtime.DefaultRetryPolicy())
		e.scheduler = runtime.NewScheduler(e.pool, e.store)
		return nil
	}
}

// WithStore replaces the default in-memory store.
func WithStore(store runtime.KernelStore) Option {
	return func(e *Engine) error {
		e.store = store
		e.pool.SetStore(store)
		e.scheduler = runtime.NewScheduler(e.pool, store)
		e.portfolio = runtime.NewPortfolioService(store)
		e.activity = runtime.NewRunActivityService(store, store)
		if dependencyStore, ok := store.(runtime.DependencyKernelStore); ok {
			e.dependencies = runtime.NewDependencyCoordinator(dependencyStore)
		} else {
			e.dependencies = nil
		}
		if conversationStore, ok := store.(runtime.ConversationStore); ok {
			e.conversations = runtime.NewConversationService(conversationStore)
			e.conversationChanges, _ = runtime.NewConversationChangeService(conversationStore, store)
		} else {
			e.conversations = nil
			e.conversationChanges = nil
		}
		if collaborationStore, ok := store.(runtime.CollaborationKernelStore); ok {
			e.collaboration = runtime.NewCollaborationService(collaborationStore)
		} else {
			e.collaboration = nil
		}
		e.turns = runtime.NewAgentTurnService(store, store)
		e.turnsRun = runtime.NewTurnCoordinator(store, store, store)
		e.runQueue = runtime.NewAgentRunScheduler(store)
		e.wake = runtime.NewAgentRunWakeService(store, store)
		if artifactStore, ok := store.(runtime.ArtifactStore); ok {
			e.artifacts = runtime.NewArtifactCatalog(artifactStore)
		} else {
			e.artifacts = nil
		}
		if agentStore, ok := store.(kernelagent.Store); ok {
			e.agents = kernelagent.NewRegistryWithStore(agentStore)
		}
		if teamStore, ok := store.(kernelteam.Store); ok && e.agents != nil {
			e.teams = kernelteam.NewRegistryWithStore(teamStore, e.agents)
		}
		if skillStore, ok := store.(skill.CatalogStore); ok {
			e.skills = skill.NewCatalogWithStore(skillStore)
		}
		return nil
	}
}

// WithPersistentStore wires the full durable kernel contract. Use this option
// for production deployments; WithStore remains available for runtime-only
// embeddings and tests.
func WithPersistentStore(store PersistentKernelStore) Option {
	return WithStore(store)
}

// WithConversationCoordinator connects portable channel arbitration to a
// host's authorized Agent roster and proposal runtime. The host keeps tenant,
// credential, model, and deployment concerns behind these interfaces.
func WithConversationCoordinator(
	participants runtime.ConversationParticipantSource,
	proposals runtime.ParticipationProposalProvider,
	config runtime.ConversationCoordinatorConfig,
) Option {
	return func(e *Engine) error {
		e.conversationParticipants = participants
		e.participationProposals = proposals
		e.conversationCoordinatorConfig = config
		return nil
	}
}

// WithDynamicConversationRuns moves channel participation from an interactive
// caller into durable Run workers for every active host scope. The same host
// participant/proposal adapters configured by WithConversationCoordinator are
// used by the Run turn runner.
func WithDynamicConversationRuns(config ConversationRunConfig, scopes runtime.WorkerScopeSource) Option {
	return func(e *Engine) error {
		if scopes == nil {
			return fmt.Errorf("conversation Run scope source is required")
		}
		e.conversationRunConfig = &config
		e.conversationRunScopes = scopes
		return nil
	}
}

func (e *Engine) rebuildConversationCoordinator() error {
	e.conversationCoordinator = nil
	if e.conversationParticipants == nil && e.participationProposals == nil {
		return nil
	}
	coordinator, err := runtime.NewConversationCoordinator(
		e.conversations, e.conversationParticipants, e.participationProposals, e.conversationCoordinatorConfig,
	)
	if err != nil {
		return err
	}
	e.conversationCoordinator = coordinator
	return nil
}

func (e *Engine) rebuildConversationRuns() error {
	e.conversationRunScheduler = nil
	e.conversationRunReconciler = nil
	if e.conversationRunConfig == nil && e.conversationRunScopes == nil {
		return nil
	}
	if e.conversationRunConfig == nil || e.conversationRunScopes == nil || e.conversationCoordinator == nil {
		return runtime.ErrConversationCoordinationUnavailable
	}
	conversationStore, ok := e.store.(runtime.ConversationStore)
	if !ok {
		return fmt.Errorf("persistent store does not implement conversation storage")
	}
	config := *e.conversationRunConfig
	if config.Workers.Kind == "" {
		config.Workers.Kind = runtime.RunKindConversation
	}
	if config.Workers.Kind != runtime.RunKindConversation {
		return fmt.Errorf("conversation workers require Run kind %q", runtime.RunKindConversation)
	}
	if config.Workers.MaxActiveForConcurrencyKey == 0 {
		config.Workers.MaxActiveForConcurrencyKey = 1
	}
	if config.Workers.MaxActiveForConcurrencyKey != 1 {
		return fmt.Errorf("conversation workers require exactly one active Run per channel")
	}
	if strings.TrimSpace(config.Workers.WorkerIDPrefix) == "" {
		config.Workers.WorkerIDPrefix = "conversation-run-worker"
	}
	scheduler, err := runtime.NewConversationRunScheduler(conversationStore, e.store, config.Scheduler)
	if err != nil {
		return err
	}
	runner, err := runtime.NewConversationRunTurnRunner(conversationStore, e.conversationCoordinator, config.Runner)
	if err != nil {
		return err
	}
	reconciler, err := runtime.NewConversationRunReconciler(scheduler, e.conversationRunScopes, e.logger, config.Reconciler)
	if err != nil {
		return err
	}
	e.conversationRunScheduler = scheduler
	e.conversationRunReconciler = reconciler
	e.agentSupervisorSpecs = append(e.agentSupervisorSpecs, agentRunWorkerSupervisorSpec{
		config: config.Workers, source: e.conversationRunScopes, resolver: runner,
	})
	return nil
}

// WithLogger replaces the default logger.
func WithLogger(logger *zap.SugaredLogger) Option {
	return func(e *Engine) error {
		e.logger = logger
		return nil
	}
}

// WithWorkerPool configures the async worker pool.
func WithWorkerPool(concurrency int, retry *runtime.RetryPolicy) Option {
	return func(e *Engine) error {
		pe := executor.NewPipelineExecutor(e.registry, e.logger)
		e.pool = runtime.NewWorkerPool(pe, e.store, e.logger, concurrency, retry)
		e.scheduler = runtime.NewScheduler(e.pool, e.store)
		return nil
	}
}

// WithAgentRunWorkers adds a scope-bound autonomous portfolio worker pool.
// The option may be repeated for additional scopes or agent deployments.
func WithAgentRunWorkers(config runtime.AgentRunWorkerConfig, resolver runtime.TurnRunnerResolver) Option {
	return func(e *Engine) error {
		if resolver == nil {
			return fmt.Errorf("turn runner resolver is required")
		}
		e.agentPoolSpecs = append(e.agentPoolSpecs, agentRunWorkerSpec{config: config, resolver: resolver})
		return nil
	}
}

// WithDynamicAgentRunWorkers reconciles one kind-filtered autonomous Run pool
// per active host scope. The option may be repeated for independent execution
// kinds or scope sources.
func WithDynamicAgentRunWorkers(
	config runtime.DynamicAgentRunWorkerConfig,
	source runtime.WorkerScopeSource,
	resolver runtime.TurnRunnerResolver,
) Option {
	return func(e *Engine) error {
		if source == nil || resolver == nil {
			return fmt.Errorf("worker scope source and turn runner resolver are required")
		}
		e.agentSupervisorSpecs = append(e.agentSupervisorSpecs, agentRunWorkerSupervisorSpec{
			config: config, source: source, resolver: resolver,
		})
		return nil
	}
}

func WithSkillCatalog(catalog *skill.Catalog) Option {
	return func(e *Engine) error {
		if catalog == nil {
			return fmt.Errorf("skill catalog is required")
		}
		e.skills = catalog
		return nil
	}
}

func WithActionPolicy(policy runtime.ActionPolicyEvaluator) Option {
	return func(e *Engine) error {
		if policy == nil {
			return fmt.Errorf("action policy is required")
		}
		e.actionPolicy = policy
		return nil
	}
}

func WithActionWorkers(config runtime.ActionWorkerConfig, credentials runtime.CredentialResolver, dispatcher runtime.ActionDispatcher) Option {
	return func(e *Engine) error {
		if dispatcher == nil {
			return fmt.Errorf("action dispatcher is required")
		}
		e.actionPoolSpecs = append(e.actionPoolSpecs, actionWorkerSpec{config: config, credentials: credentials, dispatcher: dispatcher})
		return nil
	}
}

// WithDynamicActionWorkers reconciles one isolated worker pool per active
// scope supplied by the embedding control plane. The option may be repeated
// for independent scope sources or transport hosts.
func WithDynamicActionWorkers(config runtime.DynamicActionWorkerConfig, source runtime.WorkerScopeSource, credentials runtime.CredentialResolver, dispatcher runtime.ActionDispatcher) Option {
	return func(e *Engine) error {
		if source == nil {
			return fmt.Errorf("action worker scope source is required")
		}
		if dispatcher == nil {
			return fmt.Errorf("action dispatcher is required")
		}
		e.actionSupervisorSpecs = append(e.actionSupervisorSpecs, actionWorkerSupervisorSpec{
			config: config, source: source, credentials: credentials, dispatcher: dispatcher,
		})
		return nil
	}
}

func WithClawHubRegistry(registryID string, registry clawhub.Registry, workspace string) Option {
	return func(e *Engine) error {
		manager, err := clawhub.NewInstallManager(registryID, registry, workspace)
		if err != nil {
			return err
		}
		e.clawHub = manager
		e.clawHubRegistry = registry
		return nil
	}
}

func WithApprovalAuthorizer(authorizer runtime.ApprovalAuthorizer) Option {
	return func(e *Engine) error {
		if authorizer == nil {
			return fmt.Errorf("approval authorizer is required")
		}
		e.approvalAuth = authorizer
		return nil
	}
}

func (e *Engine) rebuildGovernance() {
	e.actions = runtime.NewActionCoordinator(e.store, e.store, e.skills, e.actionPolicy)
	e.approvals = runtime.NewApprovalCoordinator(e.store, e.store, e.approvalAuth)
}

func (e *Engine) restoreClawHubSkills() error {
	if e.clawHub == nil {
		return nil
	}
	installed, err := e.clawHub.LoadInstalled()
	if err != nil {
		return err
	}
	for _, item := range installed {
		if err := e.activateInstalledSkill(context.Background(), item); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) activateInstalledSkill(ctx context.Context, installed *clawhub.InstalledSkill) error {
	if installed == nil || installed.Compilation == nil || installed.Compilation.Definition == nil {
		return fmt.Errorf("installed skill compilation is required")
	}
	definition := installed.Compilation.Definition
	if err := e.validateClawHubCompilation(installed.Compilation); err != nil {
		return err
	}
	existing, err := e.skills.GetDefinition(ctx, definition.ID, definition.Version)
	if err != nil {
		return err
	}
	if existing != nil {
		if existing.Source != nil && definition.Source != nil && existing.Source.Digest == definition.Source.Digest {
			return nil
		}
		return fmt.Errorf("skill %s@%s conflicts with an active immutable definition", definition.ID, definition.Version)
	}
	return e.skills.Register(ctx, definition)
}

func (e *Engine) validateClawHubCompilation(compilation *skillopenclaw.Compilation) error {
	if compilation == nil || compilation.Definition == nil {
		return fmt.Errorf("compiled skill definition is required")
	}
	definition := compilation.Definition
	validationCatalog := skill.NewCatalog()
	if err := validationCatalog.Register(context.Background(), definition); err != nil {
		return err
	}
	existing, err := e.skills.GetDefinition(context.Background(), definition.ID, definition.Version)
	if err != nil {
		return err
	}
	if existing != nil && (existing.Source == nil || definition.Source == nil || existing.Source.Digest != definition.Source.Digest) {
		return fmt.Errorf("skill %s@%s conflicts with an active immutable definition", definition.ID, definition.Version)
	}
	return nil
}

func (e *Engine) rebuildActionWorkerPools() error {
	e.actionPools = make([]*runtime.ActionWorkerPool, 0, len(e.actionPoolSpecs))
	for _, spec := range e.actionPoolSpecs {
		pool, err := runtime.NewActionWorkerPool(e.store, e.skills, spec.credentials, spec.dispatcher, e.logger, spec.config)
		if err != nil {
			return err
		}
		e.actionPools = append(e.actionPools, pool)
	}
	return nil
}

func (e *Engine) rebuildActionWorkerSupervisors() error {
	e.actionSupervisors = make([]*runtime.ActionWorkerSupervisor, 0, len(e.actionSupervisorSpecs))
	for _, spec := range e.actionSupervisorSpecs {
		supervisor, err := runtime.NewActionWorkerSupervisor(e.store, e.skills, spec.credentials, spec.dispatcher, spec.source, e.logger, spec.config)
		if err != nil {
			return err
		}
		e.actionSupervisors = append(e.actionSupervisors, supervisor)
	}
	return nil
}

func (e *Engine) rebuildAgentWorkerPools() error {
	e.agentPools = make([]*runtime.AgentRunWorkerPool, 0, len(e.agentPoolSpecs))
	for _, spec := range e.agentPoolSpecs {
		pool, err := runtime.NewAgentRunWorkerPool(e.store, spec.resolver, e.logger, spec.config)
		if err != nil {
			return err
		}
		e.agentPools = append(e.agentPools, pool)
	}
	return nil
}

func (e *Engine) rebuildAgentWorkerSupervisors() error {
	e.agentSupervisors = make([]*runtime.AgentRunWorkerSupervisor, 0, len(e.agentSupervisorSpecs))
	for _, spec := range e.agentSupervisorSpecs {
		supervisor, err := runtime.NewAgentRunWorkerSupervisor(e.store, spec.resolver, spec.source, e.logger, spec.config)
		if err != nil {
			return err
		}
		e.agentSupervisors = append(e.agentSupervisors, supervisor)
	}
	return nil
}

func (e *Engine) WakeAgentWorkers() {
	for _, pool := range e.agentPools {
		pool.Wake()
	}
	for _, supervisor := range e.agentSupervisors {
		supervisor.Wake()
	}
}

func (e *Engine) WakeActionWorkers() {
	for _, pool := range e.actionPools {
		pool.Wake()
	}
	for _, supervisor := range e.actionSupervisors {
		supervisor.Wake()
	}
}

// BuildGraph is a convenience wrapper for executor.BuildGraph.
func BuildGraph(nodes []*executor.NodeDefinition, connections []*executor.ConnectionDefinition) (*executor.ExecutionGraph, error) {
	return executor.BuildGraph(nodes, connections)
}

func (e *Engine) CreateObjective(ctx context.Context, req runtime.CreateObjectiveRequest) (*runtime.Objective, error) {
	return e.portfolio.CreateObjective(ctx, req)
}

func (e *Engine) CreateObjectiveIdempotent(ctx context.Context, req runtime.CreateObjectiveRequest) (*runtime.CreateObjectiveResult, error) {
	return e.portfolio.CreateObjectiveIdempotent(ctx, req)
}

func (e *Engine) GetObjective(ctx context.Context, scope runtime.Scope, objectiveID string) (*runtime.Objective, error) {
	return e.portfolio.GetObjective(ctx, scope, objectiveID)
}

func (e *Engine) ListObjectives(ctx context.Context, filter runtime.ObjectiveFilter) ([]*runtime.Objective, error) {
	return e.portfolio.ListObjectives(ctx, filter)
}

func (e *Engine) UpdateObjective(ctx context.Context, scope runtime.Scope, objectiveID string, req runtime.UpdateObjectiveRequest) (*runtime.Objective, error) {
	return e.portfolio.UpdateObjective(ctx, scope, objectiveID, req)
}

func (e *Engine) ReconcileObjectiveSchedules(ctx context.Context, scope runtime.Scope, limit int) (*runtime.ObjectiveScheduleResult, error) {
	return runtime.NewObjectiveScheduler(e.store).ReconcileScope(ctx, scope, limit)
}

func (e *Engine) ReconcileAllObjectiveSchedules(ctx context.Context, limitPerScope int) (*runtime.ObjectiveScheduleResult, error) {
	return runtime.NewObjectiveScheduler(e.store).ReconcileAll(ctx, limitPerScope)
}

func (e *Engine) CreateAgentRun(ctx context.Context, req runtime.CreateAgentRunRequest) (*runtime.AgentRun, error) {
	result, err := e.CreateAgentRunCommand(ctx, req)
	if err != nil {
		return nil, err
	}
	return result.Run, nil
}

// CreateAgentRunCommand returns both the durable run and its creation event.
// Idempotent replays return the existing run with a nil event.
func (e *Engine) CreateAgentRunCommand(ctx context.Context, req runtime.CreateAgentRunRequest) (*runtime.AgentRunCommandResult, error) {
	return runtime.NewRunCommandService(e.store).CreateAgentRun(ctx, req)
}

func (e *Engine) CommandAgentRun(ctx context.Context, req runtime.AgentRunCommandRequest) (*runtime.AgentRunCommandResult, error) {
	return runtime.NewRunCommandService(e.store).CommandAgentRun(ctx, req)
}

func (e *Engine) GetAgentRun(ctx context.Context, scope runtime.Scope, runID string) (*runtime.AgentRun, error) {
	return e.portfolio.GetAgentRun(ctx, scope, runID)
}

func (e *Engine) ListAgentRuns(ctx context.Context, filter runtime.AgentRunFilter) ([]*runtime.AgentRun, error) {
	return e.portfolio.ListAgentRuns(ctx, filter)
}

func (e *Engine) RegisterArtifact(ctx context.Context, request runtime.RegisterArtifactRequest) (*runtime.ArtifactRegistrationResult, error) {
	if e.artifacts == nil {
		return nil, fmt.Errorf("artifact store is not configured")
	}
	return e.artifacts.Register(ctx, request)
}

func (e *Engine) GetArtifact(ctx context.Context, scope runtime.Scope, artifactID string, version int64) (*runtime.Artifact, error) {
	if e.artifacts == nil {
		return nil, fmt.Errorf("artifact store is not configured")
	}
	return e.artifacts.Get(ctx, scope, artifactID, version)
}

func (e *Engine) ListArtifacts(ctx context.Context, filter runtime.ArtifactFilter) ([]*runtime.Artifact, error) {
	if e.artifacts == nil {
		return nil, fmt.Errorf("artifact store is not configured")
	}
	return e.artifacts.List(ctx, filter)
}

func (e *Engine) TransitionAgentRun(ctx context.Context, scope runtime.Scope, runID string, req runtime.RunTransitionRequest) (*runtime.AgentRun, *runtime.ActivityEvent, error) {
	return e.activity.TransitionRun(ctx, scope, runID, req)
}

func (e *Engine) AppendActivity(ctx context.Context, event *runtime.ActivityEvent) (*runtime.ActivityEvent, error) {
	return e.activity.AppendActivity(ctx, event)
}

func (e *Engine) ListActivity(ctx context.Context, filter runtime.ActivityFilter) ([]*runtime.ActivityEvent, error) {
	return e.activity.ListActivity(ctx, filter)
}

func (e *Engine) ListActivityFeed(ctx context.Context, request runtime.ActivityFeedRequest) (*runtime.ActivityFeedPage, error) {
	return e.activity.ListActivityFeed(ctx, request)
}

func (e *Engine) CreateConversation(ctx context.Context, request runtime.CreateConversationRequest) (*runtime.Conversation, bool, error) {
	if e.conversations == nil {
		return nil, false, fmt.Errorf("conversation store is not configured")
	}
	return e.conversations.CreateConversation(ctx, request)
}

func (e *Engine) GetConversation(ctx context.Context, scope runtime.Scope, conversationID string) (*runtime.Conversation, error) {
	if e.conversations == nil {
		return nil, fmt.Errorf("conversation store is not configured")
	}
	return e.conversations.GetConversation(ctx, scope, conversationID)
}

func (e *Engine) ListConversations(ctx context.Context, filter runtime.ConversationFilter) ([]*runtime.Conversation, error) {
	if e.conversations == nil {
		return nil, fmt.Errorf("conversation store is not configured")
	}
	return e.conversations.ListConversations(ctx, filter)
}

// ImportConversationArchive migrates user-visible historical collaboration
// facts without scheduling new conversation Runs for old messages.
func (e *Engine) ImportConversationArchive(ctx context.Context, request runtime.ImportConversationArchiveRequest) (*runtime.ConversationArchiveImportResult, error) {
	if e.conversations == nil {
		return nil, fmt.Errorf("conversation store is not configured")
	}
	return e.conversations.ImportArchive(ctx, request)
}

func (e *Engine) PostChannelMessage(ctx context.Context, request runtime.PostChannelMessageRequest) (*runtime.ChannelMessageCommitResult, error) {
	if e.conversations == nil {
		return nil, fmt.Errorf("conversation store is not configured")
	}
	result, err := e.conversations.PostChannelMessage(ctx, request)
	if err != nil || e.conversationRunScheduler == nil {
		return result, err
	}
	scheduled, _, err := e.conversationRunScheduler.ScheduleMessage(ctx, result.Message.Scope, result.Message.ConversationID, result.Message.ID)
	if err != nil {
		return nil, err
	}
	if scheduled != nil {
		result.Run = scheduled.Run
	}
	if e.conversationRunReconciler != nil {
		e.conversationRunReconciler.Wake()
	}
	return result, nil
}

func (e *Engine) GetChannelMessage(ctx context.Context, scope runtime.Scope, conversationID, messageID string) (*runtime.ChannelMessage, error) {
	if e.conversations == nil {
		return nil, fmt.Errorf("conversation store is not configured")
	}
	return e.conversations.GetChannelMessage(ctx, scope, conversationID, messageID)
}

func (e *Engine) ListChannelMessages(ctx context.Context, filter runtime.ChannelMessageFilter) ([]*runtime.ChannelMessage, error) {
	if e.conversations == nil {
		return nil, fmt.Errorf("conversation store is not configured")
	}
	return e.conversations.ListChannelMessages(ctx, filter)
}

func (e *Engine) ListConversationChanges(ctx context.Context, request runtime.ConversationChangeRequest) (*runtime.ConversationChangeSet, error) {
	if e.conversationChanges == nil {
		return nil, fmt.Errorf("conversation change service is not configured")
	}
	return e.conversationChanges.ListChanges(ctx, request)
}

// ConversationChangesAvailable reports whether reconnect-safe channel change
// projections can be served by the configured persistent kernel store.
func (e *Engine) ConversationChangesAvailable() bool {
	return e != nil && e.conversationChanges != nil
}

func (e *Engine) CoordinateParticipation(ctx context.Context, request runtime.CoordinateParticipationRequest) (*runtime.ParticipationRoundResult, error) {
	if e.conversations == nil {
		return nil, fmt.Errorf("conversation store is not configured")
	}
	return e.conversations.CoordinateParticipation(ctx, request)
}

// CoordinateConversation asks every eligible Agent for a bounded proposal,
// persists truthful working/read state, and atomically commits deterministic
// arbitration. It is available only when the embedding host configured the
// participant and proposal boundaries.
func (e *Engine) CoordinateConversation(ctx context.Context, request runtime.ConversationCoordinationRequest) (*runtime.ParticipationRoundResult, error) {
	if e.conversationCoordinator == nil {
		return nil, runtime.ErrConversationCoordinationUnavailable
	}
	return e.conversationCoordinator.Coordinate(ctx, request)
}

func (e *Engine) ConversationCoordinationAvailable() bool {
	return e != nil && e.conversationCoordinator != nil
}

// ConversationRunsAvailable reports whether committed Team messages are
// automatically projected into durable, recoverable conversation Runs.
func (e *Engine) ConversationRunsAvailable() bool {
	return e != nil && e.conversationRunScheduler != nil && e.conversationRunReconciler != nil
}

func (e *Engine) GetParticipationRound(ctx context.Context, scope runtime.Scope, conversationID, roundID string) (*runtime.ParticipationRoundResult, error) {
	if e.conversations == nil {
		return nil, fmt.Errorf("conversation store is not configured")
	}
	return e.conversations.GetParticipationRound(ctx, scope, conversationID, roundID)
}

func (e *Engine) ListParticipationRounds(ctx context.Context, filter runtime.ParticipationRoundFilter) ([]*runtime.ParticipationRoundResult, error) {
	if e.conversations == nil {
		return nil, fmt.Errorf("conversation store is not configured")
	}
	return e.conversations.ListParticipationRounds(ctx, filter)
}

func (e *Engine) AdvanceConversationCursor(ctx context.Context, request runtime.AdvanceConversationCursorRequest) (*runtime.ConversationCursor, bool, error) {
	if e.conversations == nil {
		return nil, false, fmt.Errorf("conversation store is not configured")
	}
	return e.conversations.AdvanceCursor(ctx, request)
}

func (e *Engine) GetConversationCursor(ctx context.Context, scope runtime.Scope, conversationID string, participant runtime.ConversationParticipant) (*runtime.ConversationCursor, error) {
	if e.conversations == nil {
		return nil, fmt.Errorf("conversation store is not configured")
	}
	return e.conversations.GetCursor(ctx, scope, conversationID, participant)
}

func (e *Engine) SetConversationPresence(ctx context.Context, request runtime.SetConversationPresenceRequest) (*runtime.ConversationPresence, error) {
	if e.conversations == nil {
		return nil, fmt.Errorf("conversation store is not configured")
	}
	return e.conversations.SetPresence(ctx, request)
}

func (e *Engine) ReleaseConversationPresence(ctx context.Context, request runtime.ReleaseConversationPresenceRequest) error {
	if e.conversations == nil {
		return fmt.Errorf("conversation store is not configured")
	}
	return e.conversations.ReleasePresence(ctx, request)
}

func (e *Engine) ListConversationPresence(ctx context.Context, scope runtime.Scope, conversationID string) ([]*runtime.ConversationPresence, error) {
	if e.conversations == nil {
		return nil, fmt.Errorf("conversation store is not configured")
	}
	return e.conversations.ListPresence(ctx, scope, conversationID)
}

func (e *Engine) CreateRunDependencyGroup(ctx context.Context, request runtime.CreateRunDependencyGroupRequest) (*runtime.RunDependencyResult, error) {
	if e.dependencies == nil {
		return nil, fmt.Errorf("run dependency store is not configured")
	}
	return e.dependencies.CreateRunDependencyGroup(ctx, request)
}

func (e *Engine) ResolveRunDependency(ctx context.Context, request runtime.ResolveRunDependencyRequest) (*runtime.RunDependencyResult, error) {
	if e.dependencies == nil {
		return nil, fmt.Errorf("run dependency store is not configured")
	}
	return e.dependencies.ResolveRunDependency(ctx, request)
}

func (e *Engine) GetRunDependencyGroup(ctx context.Context, scope runtime.Scope, groupID string) (*runtime.RunDependencyGroup, error) {
	if e.dependencies == nil {
		return nil, fmt.Errorf("run dependency store is not configured")
	}
	return e.dependencies.GetRunDependencyGroup(ctx, scope, groupID)
}

func (e *Engine) FindRunDependencyGroupByIdempotencyKey(ctx context.Context, scope runtime.Scope, key string) (*runtime.RunDependencyGroup, error) {
	if e.dependencies == nil {
		return nil, fmt.Errorf("run dependency store is not configured")
	}
	return e.dependencies.FindRunDependencyGroupByIdempotencyKey(ctx, scope, key)
}

func (e *Engine) ListRunDependencies(ctx context.Context, scope runtime.Scope, groupID string) ([]*runtime.RunDependency, error) {
	if e.dependencies == nil {
		return nil, fmt.Errorf("run dependency store is not configured")
	}
	return e.dependencies.ListRunDependencies(ctx, scope, groupID)
}

func (e *Engine) CreateAgentRequest(ctx context.Context, request runtime.CreateAgentRequestRequest) (*runtime.AgentRequestResult, error) {
	if e.collaboration == nil {
		return nil, fmt.Errorf("collaboration store is not configured")
	}
	return e.collaboration.CreateAgentRequest(ctx, request)
}

func (e *Engine) CreateAgentRequestGroup(ctx context.Context, request runtime.CreateAgentRequestGroupRequest) (*runtime.AgentRequestGroupResult, error) {
	if e.collaboration == nil {
		return nil, fmt.Errorf("collaboration store is not configured")
	}
	return e.collaboration.CreateAgentRequestGroup(ctx, request)
}

func (e *Engine) RespondAgentRequest(ctx context.Context, request runtime.RespondAgentRequestRequest) (*runtime.AgentRequestResult, error) {
	if e.collaboration == nil {
		return nil, fmt.Errorf("collaboration store is not configured")
	}
	return e.collaboration.RespondAgentRequest(ctx, request)
}

func (e *Engine) CompleteAgentRequest(ctx context.Context, request runtime.CompleteAgentRequestRequest) (*runtime.AgentRequestResult, error) {
	if e.collaboration == nil {
		return nil, fmt.Errorf("collaboration store is not configured")
	}
	return e.collaboration.CompleteAgentRequest(ctx, request)
}

func (e *Engine) GetAgentRequest(ctx context.Context, scope runtime.Scope, requestID string) (*runtime.AgentRequest, error) {
	if e.collaboration == nil {
		return nil, fmt.Errorf("collaboration store is not configured")
	}
	return e.collaboration.GetAgentRequest(ctx, scope, requestID)
}

func (e *Engine) ListAgentRequests(ctx context.Context, filter runtime.AgentRequestFilter) ([]*runtime.AgentRequest, error) {
	if e.collaboration == nil {
		return nil, fmt.Errorf("collaboration store is not configured")
	}
	return e.collaboration.ListAgentRequests(ctx, filter)
}

func (e *Engine) BeginAgentTurn(ctx context.Context, req runtime.BeginAgentTurnRequest) (*runtime.AgentTurn, error) {
	return e.turns.BeginTurn(ctx, req)
}

func (e *Engine) FinishAgentTurn(ctx context.Context, scope runtime.Scope, turnID string, req runtime.FinishAgentTurnRequest) (*runtime.AgentTurn, error) {
	return e.turns.FinishTurn(ctx, scope, turnID, req)
}

func (e *Engine) GetAgentTurn(ctx context.Context, scope runtime.Scope, turnID string) (*runtime.AgentTurn, error) {
	return e.turns.GetTurn(ctx, scope, turnID)
}

func (e *Engine) ListAgentTurns(ctx context.Context, filter runtime.AgentTurnFilter) ([]*runtime.AgentTurn, error) {
	return e.turns.ListTurns(ctx, filter)
}

// AdvanceAgentRun performs or reconciles one bounded, provider-neutral agent
// turn. The runner proposes actions; governed side effects execute separately.
func (e *Engine) AdvanceAgentRun(ctx context.Context, req runtime.AdvanceAgentRunRequest, runner runtime.TurnRunner) (*runtime.AdvanceAgentRunResult, error) {
	return e.turnsRun.Advance(ctx, req, runner)
}

func (e *Engine) ClaimNextAgentRun(ctx context.Context, req runtime.AgentRunClaimRequest) (*runtime.AgentRun, error) {
	return e.runQueue.ClaimNext(ctx, req)
}

func (e *Engine) RenewAgentRunLease(ctx context.Context, scope runtime.Scope, runID, workerID string, leaseDuration time.Duration) (*runtime.AgentRun, error) {
	return e.runQueue.RenewLease(ctx, scope, runID, workerID, leaseDuration)
}

func (e *Engine) WakeAgentRuns(ctx context.Context, signal runtime.WakeSignal) (*runtime.WakeResult, error) {
	return e.wake.Wake(ctx, signal)
}

func (e *Engine) WakeDueAgentRuns(ctx context.Context, scope runtime.Scope, at time.Time) (*runtime.WakeResult, error) {
	return e.wake.WakeDueTimers(ctx, scope, at)
}

func (e *Engine) RegisterSkill(ctx context.Context, definition *skill.Definition) error {
	return e.skills.Register(ctx, definition)
}

func (e *Engine) GetSkillDefinition(ctx context.Context, skillID, version string) (*skill.Definition, error) {
	return e.skills.GetDefinition(ctx, skillID, version)
}

func (e *Engine) BindSkill(ctx context.Context, binding *skill.Binding) error {
	return e.skills.Bind(ctx, binding)
}

func (e *Engine) ListModelSkillActions(ctx context.Context, scope skill.ScopeReference, deploymentID string) ([]skill.ModelAction, error) {
	return e.skills.ListModelActions(ctx, scope, deploymentID)
}

func (e *Engine) ListModelSkillPrompts(ctx context.Context, scope skill.ScopeReference, deploymentID string) ([]skill.ModelPrompt, error) {
	return e.skills.ListModelPrompts(ctx, scope, deploymentID)
}

func (e *Engine) ResolveSkillPrompt(ctx context.Context, scope skill.ScopeReference, deploymentID, skillID, version string) (*skill.PromptModule, error) {
	return e.skills.ResolvePrompt(ctx, scope, deploymentID, skillID, version)
}

func (e *Engine) ResolveSkillAction(ctx context.Context, scope skill.ScopeReference, deploymentID, skillID, version, action string) (*skill.BoundAction, error) {
	return e.skills.Resolve(ctx, scope, deploymentID, skillID, version, action)
}

func (e *Engine) ActivateSkills(ctx context.Context, scope skill.ScopeReference, deploymentID string, host skill.HostCapabilityState) (*skill.ActivationSnapshot, error) {
	return e.skills.Activate(ctx, scope, deploymentID, host)
}

func (e *Engine) RegisterAgentDefinition(ctx context.Context, definition *kernelagent.AgentDefinition) (*kernelagent.AgentDefinition, error) {
	return e.agents.RegisterDefinition(ctx, definition)
}

func (e *Engine) GetAgentDefinition(ctx context.Context, id, version string) (*kernelagent.AgentDefinition, error) {
	return e.agents.GetDefinition(ctx, id, version)
}

func (e *Engine) ListAgentDefinitionVersions(ctx context.Context, id string) ([]*kernelagent.AgentDefinition, error) {
	return e.agents.ListDefinitionVersions(ctx, id)
}

func (e *Engine) CreateAgentDeployment(ctx context.Context, deployment *kernelagent.AgentDeployment, actorType, actorID, reason string) (*kernelagent.AgentDeployment, *kernelagent.DefinitionActivation, error) {
	return e.agents.CreateDeployment(ctx, deployment, actorType, actorID, reason)
}

func (e *Engine) GetAgentDeployment(ctx context.Context, scope skill.ScopeReference, deploymentID string) (*kernelagent.AgentDeployment, error) {
	return e.agents.GetDeployment(ctx, scope, deploymentID)
}

func (e *Engine) ActivateAgentDefinition(ctx context.Context, scope skill.ScopeReference, deploymentID, version string, expectedRevision int64, actorType, actorID, reason string) (*kernelagent.AgentDeployment, *kernelagent.DefinitionActivation, error) {
	return e.agents.ActivateDefinition(ctx, scope, deploymentID, version, expectedRevision, actorType, actorID, reason)
}

func (e *Engine) RollbackAgentDefinition(ctx context.Context, scope skill.ScopeReference, deploymentID string, expectedRevision int64, actorType, actorID, reason string) (*kernelagent.AgentDeployment, *kernelagent.DefinitionActivation, error) {
	return e.agents.RollbackDefinition(ctx, scope, deploymentID, expectedRevision, actorType, actorID, reason)
}

func (e *Engine) ListAgentDefinitionActivations(ctx context.Context, scope skill.ScopeReference, deploymentID string) ([]kernelagent.DefinitionActivation, error) {
	return e.agents.ListActivations(ctx, scope, deploymentID)
}

func (e *Engine) ProposeAgentDefinitionAmendment(ctx context.Context, request kernelagent.ProposeAmendmentRequest) (*kernelagent.DefinitionAmendment, error) {
	return e.agents.ProposeAmendment(ctx, request)
}

func (e *Engine) GetAgentDefinitionAmendment(ctx context.Context, scope skill.ScopeReference, amendmentID string) (*kernelagent.DefinitionAmendment, error) {
	return e.agents.GetAmendment(ctx, scope, amendmentID)
}

func (e *Engine) SubmitAgentDefinitionAmendmentEvaluation(ctx context.Context, request kernelagent.SubmitAmendmentEvaluationRequest) (*kernelagent.DefinitionAmendment, error) {
	return e.agents.SubmitAmendmentEvaluation(ctx, request)
}

func (e *Engine) ResolveAgentDefinitionAmendment(ctx context.Context, request kernelagent.ResolveAmendmentRequest) (*kernelagent.DefinitionAmendment, error) {
	return e.agents.ResolveAmendment(ctx, request)
}

func (e *Engine) ActivateAgentDefinitionAmendment(ctx context.Context, scope skill.ScopeReference, amendmentID string, expectedRevision int64, actorType, actorID, reason string) (*kernelagent.DefinitionAmendment, *kernelagent.AgentDeployment, *kernelagent.DefinitionActivation, error) {
	return e.agents.ActivateAmendment(ctx, scope, amendmentID, expectedRevision, actorType, actorID, reason)
}

func (e *Engine) RegisterTeamDefinition(ctx context.Context, definition *kernelteam.Definition) (*kernelteam.Definition, error) {
	return e.teams.RegisterDefinition(ctx, definition)
}

func (e *Engine) GetTeamDefinition(ctx context.Context, id, version string) (*kernelteam.Definition, error) {
	return e.teams.GetDefinition(ctx, id, version)
}

func (e *Engine) ListTeamDefinitionVersions(ctx context.Context, id string) ([]*kernelteam.Definition, error) {
	return e.teams.ListDefinitionVersions(ctx, id)
}

func (e *Engine) CreateTeamDeployment(ctx context.Context, deployment *kernelteam.Deployment, actorType, actorID, reason string) (*kernelteam.Deployment, *workforce.DefinitionActivation, error) {
	return e.teams.CreateDeployment(ctx, deployment, actorType, actorID, reason)
}

func (e *Engine) GetTeamDeployment(ctx context.Context, scope skill.ScopeReference, deploymentID string) (*kernelteam.Deployment, error) {
	return e.teams.GetDeployment(ctx, scope, deploymentID)
}

func (e *Engine) UpdateTeamDeployment(ctx context.Context, deployment *kernelteam.Deployment, expectedRevision int64, actorType, actorID, reason string) (*kernelteam.Deployment, *workforce.DefinitionActivation, error) {
	return e.teams.UpdateDeployment(ctx, deployment, expectedRevision, actorType, actorID, reason)
}

func (e *Engine) ActivateTeamDefinition(ctx context.Context, scope skill.ScopeReference, deploymentID, version string, expectedRevision int64, actorType, actorID, reason string) (*kernelteam.Deployment, *workforce.DefinitionActivation, error) {
	return e.teams.ActivateDefinition(ctx, scope, deploymentID, version, expectedRevision, actorType, actorID, reason)
}

func (e *Engine) ListTeamDefinitionActivations(ctx context.Context, scope skill.ScopeReference, deploymentID string) ([]workforce.DefinitionActivation, error) {
	return e.teams.ListActivations(ctx, scope, deploymentID)
}

func (e *Engine) ProposeTeamDefinitionAmendment(ctx context.Context, request kernelteam.ProposeAmendmentRequest) (*kernelteam.DefinitionAmendment, error) {
	return e.teams.ProposeAmendment(ctx, request)
}

func (e *Engine) GetTeamDefinitionAmendment(ctx context.Context, scope skill.ScopeReference, amendmentID string) (*kernelteam.DefinitionAmendment, error) {
	return e.teams.GetAmendment(ctx, scope, amendmentID)
}

func (e *Engine) SubmitTeamDefinitionAmendmentEvaluation(ctx context.Context, request kernelteam.SubmitAmendmentEvaluationRequest) (*kernelteam.DefinitionAmendment, error) {
	return e.teams.SubmitAmendmentEvaluation(ctx, request)
}

func (e *Engine) ResolveTeamDefinitionAmendment(ctx context.Context, request kernelteam.ResolveAmendmentRequest) (*kernelteam.DefinitionAmendment, error) {
	return e.teams.ResolveAmendment(ctx, request)
}

func (e *Engine) ActivateTeamDefinitionAmendment(ctx context.Context, scope skill.ScopeReference, amendmentID string, expectedRevision int64, actorType, actorID, reason string) (*kernelteam.DefinitionAmendment, *kernelteam.Deployment, *workforce.DefinitionActivation, error) {
	return e.teams.ActivateAmendment(ctx, scope, amendmentID, expectedRevision, actorType, actorID, reason)
}

func (e *Engine) ValidateSkillInput(ctx context.Context, action *skill.BoundAction, input map[string]interface{}) error {
	return e.skills.ValidateInput(ctx, action, input)
}

func (e *Engine) ValidateSkillOutput(ctx context.Context, action *skill.BoundAction, output map[string]interface{}) error {
	return e.skills.ValidateOutput(ctx, action, output)
}

func (e *Engine) ProposeAction(ctx context.Context, req runtime.ProposeActionRequest) (*runtime.ActionProposalResult, error) {
	return e.actions.Propose(ctx, req)
}

func (e *Engine) ResolveApproval(ctx context.Context, req runtime.ResolveApprovalRequest) (*runtime.ApprovalResolutionResult, error) {
	return e.approvals.Resolve(ctx, req)
}

func (e *Engine) GetActionCall(ctx context.Context, scope runtime.Scope, actionID string) (*runtime.ActionCall, error) {
	return e.store.GetActionCall(ctx, scope, actionID)
}

func (e *Engine) ListActionCalls(ctx context.Context, filter runtime.ActionFilter) ([]*runtime.ActionCall, error) {
	return e.store.ListActionCalls(ctx, filter)
}

func (e *Engine) GetApproval(ctx context.Context, scope runtime.Scope, approvalID string) (*runtime.ApprovalCheckpoint, error) {
	return e.store.GetApproval(ctx, scope, approvalID)
}

func (e *Engine) ListApprovals(ctx context.Context, filter runtime.ApprovalFilter) ([]*runtime.ApprovalCheckpoint, error) {
	return e.store.ListApprovals(ctx, filter)
}

func NewClawHubClient(baseURL string) *clawhub.ClawHubClient {
	return clawhub.NewClawHubClient(baseURL)
}

func (e *Engine) SearchClawHubSkills(ctx context.Context, request clawhub.SearchRequest) (*clawhub.SkillPage, error) {
	if e.clawHubRegistry == nil {
		return nil, fmt.Errorf("ClawHub registry is not configured")
	}
	return e.clawHubRegistry.SearchSkills(ctx, request)
}

func (e *Engine) ExploreClawHubSkills(ctx context.Context, request clawhub.ExploreRequest) (*clawhub.SkillPage, error) {
	if e.clawHubRegistry == nil {
		return nil, fmt.Errorf("ClawHub registry is not configured")
	}
	return e.clawHubRegistry.ExploreSkills(ctx, request)
}

func (e *Engine) InspectClawHubSkill(ctx context.Context, reference clawhub.SkillReference) (*clawhub.SkillDetail, error) {
	if e.clawHubRegistry == nil {
		return nil, fmt.Errorf("ClawHub registry is not configured")
	}
	return e.clawHubRegistry.InspectSkill(ctx, reference)
}

func (e *Engine) InstallClawHubSkill(ctx context.Context, request clawhub.InstallRequest) (*clawhub.InstalledSkill, error) {
	if e.clawHub == nil {
		return nil, fmt.Errorf("ClawHub registry is not configured")
	}
	installed, err := e.clawHub.InstallValidated(ctx, request, e.validateClawHubCompilation)
	if err != nil {
		return nil, err
	}
	if err := e.activateInstalledSkill(ctx, installed); err != nil {
		return nil, err
	}
	return installed, nil
}

func (e *Engine) UpdateClawHubSkill(ctx context.Context, slug string) (*clawhub.InstalledSkill, error) {
	if e.clawHub == nil {
		return nil, fmt.Errorf("ClawHub registry is not configured")
	}
	installed, err := e.clawHub.UpdateValidated(ctx, slug, e.validateClawHubCompilation)
	if err != nil {
		return nil, err
	}
	if err := e.activateInstalledSkill(ctx, installed); err != nil {
		return nil, err
	}
	return installed, nil
}

func (e *Engine) VerifyInstalledClawHubSkill(ctx context.Context, slug string) (*clawhub.Verification, error) {
	if e.clawHub == nil {
		return nil, fmt.Errorf("ClawHub registry is not configured")
	}
	return e.clawHub.VerifyInstalled(ctx, slug)
}

func (e *Engine) PinClawHubSkill(slug, reason string) error {
	if e.clawHub == nil {
		return fmt.Errorf("ClawHub registry is not configured")
	}
	return e.clawHub.Pin(slug, reason)
}

func (e *Engine) UnpinClawHubSkill(slug string) error {
	if e.clawHub == nil {
		return fmt.Errorf("ClawHub registry is not configured")
	}
	return e.clawHub.Unpin(slug)
}
