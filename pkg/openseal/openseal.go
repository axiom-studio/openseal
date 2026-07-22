// Package openseal provides the stable public API for embedding OpenSeal
// as a library. Downstream consumers should import this package
// rather than reaching into individual pkg/ subpackages.
package openseal

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/authoring"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/executor"
	"github.com/axiom-studio/openseal/pkg/httpaction"
	"github.com/axiom-studio/openseal/pkg/kernelapi"
	"github.com/axiom-studio/openseal/pkg/progression"
	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/axiom-studio/openseal/pkg/runtime"
	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/axiom-studio/openseal/pkg/skill/clawhub"
	skillopenclaw "github.com/axiom-studio/openseal/pkg/skill/openclaw"
	skillsource "github.com/axiom-studio/openseal/pkg/skill/source"
	"github.com/axiom-studio/openseal/pkg/skill/sourceartifact"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
	"github.com/axiom-studio/openseal/pkg/workforce"
	"go.uber.org/zap"
)

// Re-export key types so consumers only import this package.
type (
	NodeDefinition           = executor.NodeDefinition
	ConnectionDefinition     = executor.ConnectionDefinition
	ExecutionResult          = executor.ExecutionResult
	NodeResult               = executor.NodeResult
	Registry                 = executor.Registry
	StepExecutor             = executor.StepExecutor
	ExecutionGraph           = executor.ExecutionGraph
	RunbookDefinition        = runbook.Definition
	RunbookStep              = runbook.Step
	RunbookStepKind          = runbook.StepKind
	RunbookActionStep        = runbook.ActionStep
	RunbookDelegateStep      = runbook.DelegateStep
	RunbookBudgetAllocation  = runbook.BudgetAllocation
	RunbookDelegateMode      = runbook.DelegateMode
	RunbookDecisionStep      = runbook.DecisionStep
	RunbookDecisionCase      = runbook.DecisionCase
	RunbookTransformStep     = runbook.TransformStep
	RunbookWaitStep          = runbook.WaitStep
	RunbookForkStep          = runbook.ForkStep
	RunbookJoinStep          = runbook.JoinStep
	RunbookJoinMode          = runbook.JoinMode
	RunbookForEachStep       = runbook.ForEachStep
	RunbookLoopReturnStep    = runbook.LoopReturnStep
	RunbookEndStep           = runbook.EndStep
	RunbookValue             = runbook.Value
	RunbookTemplateSegment   = runbook.TemplateSegment
	RunbookPredicate         = runbook.Predicate
	RunbookPredicateOperator = runbook.PredicateOperator
	RunbookDiagnostic        = runbook.Diagnostic
	RunbookTurnRunner        = runtime.RunbookTurnRunner

	AgentDefinition                           = kernelagent.AgentDefinition
	AgentDefinitionCompilation                = kernelagent.DefinitionCompilation
	AgentCompilationSource                    = kernelagent.CompilationSource
	AgentCompilationDiagnostic                = kernelagent.CompilationDiagnostic
	AgentCompilationStatus                    = kernelagent.CompilationStatus
	AgentSkillRequirement                     = kernelagent.SkillRequirement
	AgentAuthorityPolicy                      = kernelagent.AuthorityPolicy
	AgentMemoryPolicy                         = kernelagent.MemoryPolicy
	AgentEscalationPolicy                     = kernelagent.EscalationPolicy
	AgentObjectiveTemplate                    = kernelagent.ObjectiveTemplate
	AgentEvaluationCriterion                  = kernelagent.EvaluationCriterion
	AgentAmendmentPolicy                      = kernelagent.AmendmentPolicy
	AgentDefinitionProvenance                 = kernelagent.DefinitionProvenance
	AgentDeployment                           = kernelagent.AgentDeployment
	AgentDeploymentRestrictions               = kernelagent.DeploymentRestrictions
	AgentDeploymentCapacity                   = kernelagent.DeploymentCapacity
	AgentDeploymentHealth                     = kernelagent.DeploymentHealth
	AgentDefinitionActivation                 = kernelagent.DefinitionActivation
	AgentRolloutStatus                        = kernelagent.RolloutStatus
	AgentDefinitionAmendment                  = kernelagent.DefinitionAmendment
	AgentAmendmentStatus                      = kernelagent.AmendmentStatus
	AgentDefinitionFieldChange                = kernelagent.DefinitionFieldChange
	AgentAmendmentEvaluation                  = kernelagent.AmendmentEvaluation
	AgentAmendmentDecision                    = kernelagent.AmendmentDecision
	ProposeAgentAmendmentRequest              = kernelagent.ProposeAmendmentRequest
	SubmitAgentAmendmentEvaluationRequest     = kernelagent.SubmitAmendmentEvaluationRequest
	ResolveAgentAmendmentRequest              = kernelagent.ResolveAmendmentRequest
	AgentRegistryStore                        = kernelagent.Store
	KernelAgentDeploymentList                 = kernelapi.AgentDeploymentList
	KernelAgentDeploymentCatalogEntry         = kernelapi.AgentDeploymentCatalogEntry
	KernelUpdateAgentDeploymentRequest        = kernelapi.UpdateAgentDeploymentRequest
	KernelAgentDeploymentUpdateResult         = kernelapi.AgentDeploymentUpdateResult
	KernelActivateAgentDefinitionRequest      = kernelapi.ActivateAgentDefinitionRequest
	KernelRollbackAgentDefinitionRequest      = kernelapi.RollbackAgentDefinitionRequest
	KernelAgentDefinitionActivationResult     = kernelapi.AgentDefinitionActivationResult
	KernelAgentDefinitionAmendmentList        = kernelapi.AgentDefinitionAmendmentList
	KernelActivateAgentAmendmentRequest       = kernelapi.ActivateAgentDefinitionAmendmentRequest
	KernelAgentAmendmentActivationResult      = kernelapi.AgentDefinitionAmendmentActivationResult
	KernelAgentDefinitionCompilationHistory   = kernelapi.AgentDefinitionCompilationHistory
	TeamDefinition                            = kernelteam.Definition
	TeamRoleSlot                              = kernelteam.RoleSlot
	TeamRoleSkillGrant                        = kernelteam.RoleSkillGrant
	TeamRoleChannelParticipation              = kernelteam.RoleChannelParticipation
	TeamCoordinationMode                      = kernelteam.CoordinationMode
	TeamCoordinationPolicy                    = kernelteam.CoordinationPolicy
	TeamDelegationPolicy                      = kernelteam.DelegationPolicy
	TeamApprovalPolicy                        = kernelteam.ApprovalPolicy
	TeamDeployment                            = kernelteam.Deployment
	TeamDeploymentStatus                      = kernelteam.DeploymentStatus
	TeamRosterAssignment                      = kernelteam.RosterAssignment
	TeamDeploymentRestrictions                = kernelteam.DeploymentRestrictions
	TeamDefinitionAmendment                   = kernelteam.DefinitionAmendment
	TeamAmendmentStatus                       = kernelteam.AmendmentStatus
	TeamAmendmentEvaluation                   = kernelteam.AmendmentEvaluation
	TeamAmendmentDecision                     = kernelteam.AmendmentDecision
	ProposeTeamAmendmentRequest               = kernelteam.ProposeAmendmentRequest
	SubmitTeamAmendmentEvaluationRequest      = kernelteam.SubmitAmendmentEvaluationRequest
	ResolveTeamAmendmentRequest               = kernelteam.ResolveAmendmentRequest
	TeamRegistryStore                         = kernelteam.Store
	AuthorityProgressionOwnerKind             = progression.OwnerKind
	AuthorityProgressionDirection             = progression.Direction
	AuthorityProgressionCriterionOutcome      = progression.CriterionOutcome
	AuthorityProgressionEvaluationPolicy      = progression.EvaluationPolicy
	AuthorityProgressionEvaluationRequest     = progression.EvaluationRequest
	AuthorityProgressionRecommendation        = progression.Recommendation
	AgentAuthorityProgressionCeiling          = progression.AgentPolicyCeiling
	TeamAuthorityProgressionCeiling           = progression.TeamPolicyCeiling
	ProposeAgentAuthorityProgressionRequest   = progression.ProposeAgentRequest
	ProposeTeamAuthorityProgressionRequest    = progression.ProposeTeamRequest
	WorkforceSharedContextPolicy              = workforce.SharedContextPolicy
	WorkforceObjectiveTemplate                = workforce.ObjectiveTemplate
	WorkforceEvaluationCriterion              = workforce.EvaluationCriterion
	WorkforceAmendmentPolicy                  = workforce.AmendmentPolicy
	WorkforceDefinitionProvenance             = workforce.DefinitionProvenance
	WorkforceDefinitionActivation             = workforce.DefinitionActivation
	DeploymentChangeKind                      = workforce.DeploymentChangeKind
	WorkforceAuthoringMode                    = authoring.Mode
	WorkforceSkillCapability                  = authoring.SkillCapability
	WorkforceSkillCredential                  = authoring.SkillCredential
	WorkforceSourcePolicyCapability           = authoring.SourcePolicyCapability
	WorkforceSourcePolicySourceCapability     = authoring.SourcePolicySourceCapability
	WorkforceCapabilityNeed                   = authoring.CapabilityNeed
	WorkforceCapabilitySourceScopeRequirement = authoring.CapabilitySourceScopeRequirement
	WorkforceAuthorityConstraint              = authoring.AuthorityConstraint
	WorkforceCapabilityCatalog                = authoring.CapabilityCatalog
	WorkforceAssignment                       = authoring.Assignment
	WorkforceCandidate                        = authoring.WorkforceCandidate
	WorkforceInitiativeBlueprint              = authoring.InitiativeBlueprint
	WorkforceInitiativeOwnerReference         = authoring.InitiativeOwnerReference
	WorkforceInitiativeMilestoneBlueprint     = authoring.InitiativeMilestoneBlueprint
	WorkforceInitiativeHypothesisBlueprint    = authoring.InitiativeHypothesisBlueprint
	WorkforceInitiativeSourceMonitorBlueprint = authoring.InitiativeSourceMonitorBlueprint
	WorkforceInitiativeDeliverableBlueprint   = authoring.InitiativeDeliverableBlueprint
	WorkforceAuthoringRequest                 = authoring.GenerateRequest
	WorkforceAuthoringGenerator               = authoring.Generator
	WorkforceAuthoringResult                  = authoring.CompileResult
	WorkforceAuthoringValidationIssue         = authoring.ValidationIssue
	WorkforceAuthoringMissingRequirement      = authoring.MissingRequirement
	WorkforceAuthoringRiskChange              = authoring.RiskChange
	WorkforceAuthoringFieldDiff               = authoring.FieldDiff
	WorkforceChangeSet                        = authoring.ChangeSet
	WorkforceChangeSetGeneration              = authoring.ChangeSetGeneration
	WorkforceChangeSetStatus                  = authoring.ChangeSetStatus
	WorkforceChangeSetActor                   = authoring.ChangeSetActor
	WorkforceChangeSetPlacement               = authoring.ChangeSetPlacement
	WorkforceObjectivePlacement               = authoring.ObjectivePlacement
	WorkforceChangeSetPolicyFinding           = authoring.ChangeSetPolicyFinding
	WorkforceChangeSetApprovalRequirement     = authoring.ChangeSetApprovalRequirement
	WorkforceChangeSetEvaluation              = authoring.ChangeSetEvaluation
	WorkforceChangeSetApprovalDecision        = authoring.ChangeSetApprovalDecision
	WorkforceChangeSetPlacementUpdate         = authoring.ChangeSetPlacementUpdate
	WorkforceChangeSetLifecycleEvent          = authoring.ChangeSetLifecycleEvent
	CreateWorkforceChangeSetRequest           = authoring.CreateChangeSetRequest
	SubmitWorkforceChangeSetEvaluationRequest = authoring.SubmitChangeSetEvaluationRequest
	ResolveWorkforceChangeSetApprovalRequest  = authoring.ResolveChangeSetApprovalRequest
	UpdateWorkforceChangeSetPlacementRequest  = authoring.UpdateChangeSetPlacementRequest
	ApplyWorkforceChangeSetRequest            = authoring.ApplyChangeSetRequest
	RetryWorkforceChangeSetGenerationRequest  = authoring.RetryChangeSetGenerationRequest
	WorkforceChangeSetApplyReceipt            = authoring.ChangeSetApplyReceipt
	WorkforceAppliedResourceReference         = authoring.AppliedResourceReference
	WorkforceChangeSetStore                   = authoring.ChangeSetStore
	PendingWorkforceChangeSetGenerationStore  = authoring.PendingChangeSetGenerationStore
	AtomicWorkforceChangeSetStore             = authoring.AtomicChangeSetStore
	WorkforceAuthoringRunStore                = runtime.WorkforceAuthoringRunStore
	WorkforceAuthoringRunService              = runtime.WorkforceAuthoringRunService
	WorkforceAuthoringWorkerConfig            = runtime.WorkforceAuthoringWorkerConfig
	WorkforceAuthoringWorker                  = runtime.WorkforceAuthoringWorker

	RunRecord                          = runtime.RunRecord
	RetryPolicy                        = runtime.RetryPolicy
	ExecutionStore                     = runtime.ExecutionStore
	PortfolioStore                     = runtime.PortfolioStore
	KernelStore                        = runtime.KernelStore
	PostgresStore                      = runtime.PostgresStore
	PostgresStoreOption                = runtime.PostgresStoreOption
	PostgresPoolConfig                 = runtime.PostgresPoolConfig
	PostgresPoolStats                  = runtime.PostgresPoolStats
	WorkerLimiter                      = runtime.WorkerLimiter
	WorkerLimiterStats                 = runtime.WorkerLimiterStats
	Scope                              = runtime.Scope
	ObjectiveOwner                     = runtime.ObjectiveOwner
	Objective                          = runtime.Objective
	ObjectiveStatus                    = runtime.ObjectiveStatus
	ObjectiveCadence                   = runtime.ObjectiveCadence
	ObjectiveCadenceType               = runtime.ObjectiveCadenceType
	ObjectiveRunTemplate               = runtime.ObjectiveRunTemplate
	ObjectiveCapabilityInvocation      = runtime.ObjectiveCapabilityInvocation
	ObjectiveScheduleResult            = runtime.ObjectiveScheduleResult
	ObjectiveScheduleState             = runtime.ObjectiveScheduleState
	ObjectiveScheduleCondition         = runtime.ObjectiveScheduleCondition
	ObjectiveFilter                    = runtime.ObjectiveFilter
	Initiative                         = runtime.Initiative
	InitiativeStatus                   = runtime.InitiativeStatus
	InitiativeFilter                   = runtime.InitiativeFilter
	InitiativeStore                    = runtime.InitiativeStore
	CreateInitiativeRequest            = runtime.CreateInitiativeRequest
	UpdateInitiativeRequest            = runtime.UpdateInitiativeRequest
	InitiativeResourceReference        = runtime.ResourceReference
	InitiativeResourceKind             = runtime.ResourceKind
	InitiativeMilestone                = runtime.InitiativeMilestone
	InitiativeMilestoneStatus          = runtime.MilestoneStatus
	InitiativeHypothesis               = runtime.InitiativeHypothesis
	InitiativeHypothesisStatus         = runtime.HypothesisStatus
	InitiativeSourceMonitorReference   = runtime.SourceMonitorReference
	InitiativeDeliverable              = runtime.InitiativeDeliverable
	InitiativeDeliverableStatus        = runtime.DeliverableStatus
	OutreachThread                     = runtime.OutreachThread
	OutreachThreadStatus               = runtime.OutreachThreadStatus
	OutreachIdentity                   = runtime.OutreachIdentity
	OutreachMessage                    = runtime.OutreachMessage
	OutreachMessageDirection           = runtime.OutreachMessageDirection
	OutreachMessageIntent              = runtime.OutreachMessageIntent
	OutreachMessageStatus              = runtime.OutreachMessageStatus
	OutreachCapability                 = runtime.OutreachCapability
	OutreachReceipt                    = runtime.OutreachReceipt
	OutreachThreadFilter               = runtime.OutreachThreadFilter
	OutreachStore                      = runtime.OutreachStore
	OutreachActionReader               = runtime.OutreachActionReader
	CreateOutreachThreadRequest        = runtime.CreateOutreachThreadRequest
	LinkOutreachActionRequest          = runtime.LinkOutreachActionRequest
	RecordOutreachDeliveryRequest      = runtime.RecordOutreachDeliveryRequest
	ResolveOutreachMessageRequest      = runtime.ResolveOutreachMessageRequest
	ReconcileOutreachActionRequest     = runtime.ReconcileOutreachActionRequest
	ReconcileOutreachActionResult      = runtime.ReconcileOutreachActionResult
	OutreachTurnLifecycle              = runtime.OutreachTurnLifecycle
	OutreachTurnRunner                 = runtime.OutreachTurnRunner
	OutreachActionProposalObserver     = runtime.OutreachActionProposalObserver
	AgentRun                           = runtime.AgentRun
	AgentRunIntervention               = runtime.AgentRunIntervention
	BudgetPolicy                       = runtime.BudgetPolicy
	BudgetUsage                        = runtime.BudgetUsage
	BudgetReservation                  = runtime.BudgetReservation
	BudgetState                        = runtime.BudgetState
	AgentRunStatus                     = runtime.AgentRunStatus
	AgentRunFilter                     = runtime.AgentRunFilter
	AgentRunOrder                      = runtime.AgentRunOrder
	AgentRunOwnerSummary               = runtime.AgentRunOwnerSummary
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
	ConversationViewer                 = runtime.ConversationViewer
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
	ArtifactContentInspector           = runtime.ArtifactContentInspector
	ArtifactContentDeleter             = runtime.ArtifactContentDeleter
	ArtifactContentAvailability        = runtime.ArtifactContentAvailability
	ArtifactContentResolver            = runtime.ArtifactContentResolver
	ArtifactContentWrite               = runtime.ArtifactContentWrite
	ArtifactStoredContent              = runtime.ArtifactStoredContent
	ArtifactContentResolutionRequest   = runtime.ArtifactContentResolutionRequest
	ArtifactContentResolution          = runtime.ArtifactContentResolution
	ArtifactRetentionService           = runtime.ArtifactRetentionService
	ArtifactRetentionSweepResult       = runtime.ArtifactRetentionSweepResult
	ArtifactRetentionDeletion          = runtime.ArtifactRetentionDeletion
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
	TurnBudgetPlanner                  = runtime.TurnBudgetPlanner
	TurnOutcome                        = runtime.TurnOutcome
	HostedSkillPrompt                  = runtime.HostedSkillPrompt
	HostedSkillSelection               = runtime.HostedSkillSelection
	HostedSkillDisposition             = runtime.HostedSkillDisposition
	HostedTurnRequest                  = runtime.HostedTurnRequest
	HostedTurnResponse                 = runtime.HostedTurnResponse
	HostedTurnModelInput               = runtime.HostedTurnModelInput
	HostedRunBudget                    = runtime.HostedRunBudget
	TurnHost                           = runtime.TurnHost
	HostedTurnRunnerConfig             = runtime.HostedTurnRunnerConfig
	HostedTurnRunner                   = runtime.HostedTurnRunner
	EvidenceClaim                      = runtime.EvidenceClaim
	EvidenceGroundingFindingStatus     = runtime.EvidenceGroundingFindingStatus
	EvidenceGroundingFinding           = runtime.EvidenceGroundingFinding
	EvidenceGroundingReview            = runtime.EvidenceGroundingReview
	EvidenceGroundingRequest           = runtime.EvidenceGroundingRequest
	EvidenceGroundingReviewer          = runtime.EvidenceGroundingReviewer
	EvidenceSnapshot                   = runtime.EvidenceSnapshot
	EvidenceSnapshotObservation        = runtime.EvidenceSnapshotObservation
	CapabilityInvocationTurnRunner     = runtime.CapabilityInvocationTurnRunner
	AdvanceAgentRunRequest             = runtime.AdvanceAgentRunRequest
	AdvanceAgentRunResult              = runtime.AdvanceAgentRunResult
	AgentRunScheduleStore              = runtime.AgentRunScheduleStore
	AgentRunClaimRequest               = runtime.AgentRunClaimRequest
	AgentRunWorkerConfig               = runtime.AgentRunWorkerConfig
	DynamicAgentRunWorkerConfig        = runtime.DynamicAgentRunWorkerConfig
	TurnRunnerBinding                  = runtime.TurnRunnerBinding
	PreparedSkillRuntime               = runtime.PreparedSkillRuntime
	TurnRunnerResolver                 = runtime.TurnRunnerResolver
	TurnRunnerResolverFunc             = runtime.TurnRunnerResolverFunc
	AgentTurnCatalog                   = runtime.AgentTurnCatalog
	CatalogTurnResolverConfig          = runtime.CatalogTurnResolverConfig
	SkillHostCapabilityResolver        = runtime.SkillHostCapabilityResolver
	SkillHostCapabilityResolverFunc    = runtime.SkillHostCapabilityResolverFunc
	WorkerScopeSource                  = runtime.WorkerScopeSource
	WorkerScopeSourceFunc              = runtime.WorkerScopeSourceFunc
	WakeSignal                         = runtime.WakeSignal
	WokenRun                           = runtime.WokenRun
	WakeResult                         = runtime.WakeResult
	ObjectiveEventRules                = runtime.ObjectiveEventRules
	ObjectiveEventRule                 = runtime.ObjectiveEventRule
	EventEnvelope                      = runtime.EventEnvelope
	EventRoute                         = runtime.EventRoute
	EventRouteResult                   = runtime.EventRouteResult
	SkillCatalog                       = skill.Catalog
	SkillDefinition                    = skill.Definition
	SkillAction                        = skill.Action
	SkillBinding                       = skill.Binding
	SkillBindingReference              = skill.BindingReference
	SkillBindingActor                  = skill.BindingActor
	SkillBindingLifecycleAction        = skill.BindingLifecycleAction
	SkillBindingLifecycleEntry         = skill.BindingLifecycleEntry
	UpsertSkillBindingRequest          = skill.UpsertBindingRequest
	DisableSkillBindingRequest         = skill.DisableBindingRequest
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
	SkillRequirements                  = skill.Requirements
	SkillInstaller                     = skill.Installer
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
	SkillRuntimePreparationState       = skill.RuntimePreparationState
	SkillRuntimePreparationRequest     = skill.RuntimePreparationRequest
	SkillPreparedRuntime               = skill.PreparedRuntime
	SkillRuntimePreparationResult      = skill.RuntimePreparationResult
	SkillRuntimePreparer               = skill.RuntimePreparer
	SkillAvailabilityReason            = skill.AvailabilityReason
	SkillBindingActivationPreview      = skill.BindingActivationPreview
	ActivatedSkill                     = skill.ActivatedSkill
	UnavailableSkill                   = skill.UnavailableSkill
	SkillActivationSnapshot            = skill.ActivationSnapshot
	SkillDiscoveryReadiness            = skill.DiscoveryReadiness
	SkillDiscoveryRequest              = skill.DiscoveryRequest
	SkillDiscoveryAction               = skill.DiscoveryAction
	SkillDiscoveryCredential           = skill.DiscoveryCredential
	SkillDiscoveryCompatibility        = skill.DiscoveryCompatibility
	SkillDiscoveryCandidate            = skill.DiscoveryCandidate
	SkillDiscoveryPage                 = skill.DiscoveryPage
	SkillDiscoveryProvider             = skill.DiscoveryProvider
	SkillDiscoveryProviderFunc         = skill.DiscoveryProviderFunc
	HTTPActionParameter                = httpaction.Parameter
	HTTPActionInvocation               = httpaction.Invocation
	HTTPActionRequestPolicy            = httpaction.RequestPolicy
	HTTPActionRequestPolicyFunc        = httpaction.RequestPolicyFunc
	HTTPActionExecutor                 = httpaction.Executor
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
	ActionProposalValidator            = runtime.ActionProposalValidator
	ActionProposalValidationInput      = runtime.ActionProposalValidationInput
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
	RunForkBranch                      = runtime.RunForkBranch
	TurnForkProposal                   = runtime.TurnForkProposal
	TurnDelegationProposal             = runtime.TurnDelegationProposal
	CreateRunForkRequest               = runtime.CreateRunForkRequest
	RunForkResult                      = runtime.RunForkResult
	RunForkCoordinator                 = runtime.RunForkCoordinator
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
	TeamRoleActionValidator            = runtime.TeamRoleActionValidator
	TeamRoleActionDispatcher           = runtime.TeamRoleActionDispatcher
	TeamSkillActionValidator           = runtime.TeamSkillActionValidator
	TeamSkillAuthorityCatalog          = runtime.TeamSkillAuthorityCatalog
	SkillBindingActionValidator        = runtime.SkillBindingActionValidator
	SkillBindingActionDispatcher       = runtime.SkillBindingActionDispatcher
	OpenClawSkillSource                = skillopenclaw.Source
	OpenClawSkillFile                  = skillopenclaw.File
	OpenClawSkillBundle                = skillopenclaw.Bundle
	OpenClawSkillDiagnostic            = skillopenclaw.Diagnostic
	OpenClawSkillCompilation           = skillopenclaw.Compilation
	SkillSourceArtifact                = sourceartifact.Artifact
	SkillSourceArtifactFile            = sourceartifact.File
	SkillSourceArtifactOrigin          = sourceartifact.Origin
	SkillSourceArtifactReference       = sourceartifact.Reference
	SkillSourceArtifactKey             = sourceartifact.Key
	SkillSourceArtifactStore           = sourceartifact.Store
	SkillSourceArtifactImportRequest   = sourceartifact.ImportOpenClawRequest
	SkillSourceArtifactGCReport        = sourceartifact.GarbageCollectionReport
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
	ClawHubPreviewRequest              = clawhub.PreviewRequest
	ClawHubCompilationReceipt          = clawhub.CompilationReceipt
	ClawHubCompilationPreview          = clawhub.CompilationPreview
	ClawHubInstallRequest              = clawhub.InstallRequest
	ClawHubInstalledSkill              = clawhub.InstalledSkill
	ClawHubVerification                = clawhub.Verification
	ClawHubVersionPage                 = clawhub.VersionPage
	ClawHubVersionSummary              = clawhub.VersionSummary
	ClawHubVersionDetail               = clawhub.VersionDetail
	ClawHubFileEntry                   = clawhub.FileEntry
	ClawHubSecurityStatus              = clawhub.SecurityStatus
	ClawHubDownloadedArchive           = clawhub.DownloadedArchive
	ClawHubLockEntry                   = clawhub.LockEntry
	ClawHubLockfile                    = clawhub.Lockfile
	ClawHubLifecycleOperation          = clawhub.LifecycleOperation
	ClawHubLifecycleOutcome            = clawhub.LifecycleOutcome
	ClawHubLifecycleErrorCode          = clawhub.LifecycleErrorCode
	ClawHubLifecycleCapability         = clawhub.LifecycleCapability
	ClawHubLifecycleResult             = clawhub.LifecycleResult
	ClawHubLifecycleBatchResult        = clawhub.LifecycleBatchResult
	ClawHubInstalledState              = clawhub.InstalledState
)

// Credential lease aliases are kept in their own group so extending the
// security protocol does not reformat the facade's much larger alias catalog.
type (
	ActionCredentialLeaseIssuer            = runtime.ActionCredentialLeaseIssuer
	ActionCredentialLeaseIssuerFunc        = runtime.ActionCredentialLeaseIssuerFunc
	ActionCredentialLeaseIssueRequest      = runtime.ActionCredentialLeaseIssueRequest
	ActionLeaseIdentity                    = runtime.ActionLeaseIdentity
	ActionCredentialFieldReference         = runtime.ActionCredentialFieldReference
	ActionCredentialLease                  = runtime.ActionCredentialLease
	ActionCredentialLeaseSignature         = runtime.ActionCredentialLeaseSignature
	SignedActionCredentialLease            = runtime.SignedActionCredentialLease
	CreateActionCredentialLeaseRequest     = runtime.CreateActionCredentialLeaseRequest
	ActionCredentialLeaseSigner            = runtime.ActionCredentialLeaseSigner
	ActionCredentialLeaseSignatureVerifier = runtime.ActionCredentialLeaseSignatureVerifier
	ActionCredentialLeaseValidationRequest = runtime.ActionCredentialLeaseValidationRequest
	ActionCredentialLeaseRedemptionRequest = runtime.ActionCredentialLeaseRedemptionRequest
	ActionCredentialLeaseRedeemer          = runtime.ActionCredentialLeaseRedeemer
	ActionCredentialLeaseValidator         = runtime.ActionCredentialLeaseValidator
)

const (
	SkillSourceArtifactFormatOpenClawV1 = sourceartifact.FormatOpenClawSkillV1
	ClawHubCompilationPreviewAPIVersion = clawhub.CompilationPreviewAPIVersion
	ActionCredentialLeaseVersion        = runtime.ActionCredentialLeaseVersion
	MaximumActionCredentialLeaseTTL     = runtime.MaximumActionCredentialLeaseTTL
)

type InitiativeSourceMonitorDeduplication = runtime.SourceMonitorDeduplication

type (
	SourceObservation                     = runtime.SourceObservation
	SourceMonitorCheckpoint               = runtime.SourceMonitorCheckpoint
	SourceObservationFilter               = runtime.SourceObservationFilter
	SourceMonitorStore                    = runtime.SourceMonitorStore
	IngestSourceObservationRequest        = runtime.IngestSourceObservationRequest
	SourceObservationIngestResult         = runtime.SourceObservationIngestResult
	AdvanceSourceMonitorCheckpointRequest = runtime.AdvanceSourceMonitorCheckpointRequest
	SourceMonitorCheckpointResult         = runtime.SourceMonitorCheckpointResult
	EventSourceCheckpoint                 = runtime.EventSourceCheckpoint
	EventSourceCheckpointStore            = runtime.EventSourceCheckpointStore
	AdvanceEventSourceCheckpointRequest   = runtime.AdvanceEventSourceCheckpointRequest
	KernelCapability                      = kernelapi.Capability
	KernelCapabilityDocument              = kernelapi.CapabilityDocument
	KernelCapabilityContext               = kernelapi.CapabilityContext
	KernelCapabilityBlockingRequirement   = kernelapi.CapabilityBlockingRequirement
	CredentialBindingChoice               = capability.CredentialBindingChoice
	KernelApprovalRequirementReference    = kernelapi.ApprovalRequirementReference
	KernelSkillActionList                 = kernelapi.SkillActionList
	KernelSkillBindingList                = kernelapi.SkillBindingList
	KernelSkillBindingMutationResult      = kernelapi.SkillBindingMutationResult
	KernelTeamDeploymentList              = kernelapi.TeamDeploymentList
	KernelTeamDeploymentCatalogEntry      = kernelapi.TeamDeploymentCatalogEntry
	ChannelCapabilityFeatures             = kernelapi.ChannelCapabilityFeatures
	AgentDefinitionCapabilityFeatures     = kernelapi.AgentDefinitionCapabilityFeatures
	TeamDefinitionCapabilityFeatures      = kernelapi.TeamDefinitionCapabilityFeatures
	WorkforceAuthoringCapabilityFeatures  = kernelapi.WorkforceAuthoringCapabilityFeatures
	ActionApprovalCapabilityFeatures      = kernelapi.ActionApprovalCapabilityFeatures
)

const (
	DeploymentChangeConfigurationUpdated    = workforce.DeploymentChangeConfigurationUpdated
	HostedTurnAPIVersion                    = runtime.HostedTurnAPIVersion
	HostedTurnProtocolInputReserveTokens    = runtime.HostedTurnProtocolInputReserveTokens
	HostedTurnBudgetEnvelopeReserveTokens   = runtime.HostedTurnBudgetEnvelopeReserveTokens
	HostedTurnMinimumOutputTokens           = runtime.HostedTurnMinimumOutputTokens
	EvidenceGroundingAPIVersion             = runtime.EvidenceGroundingAPIVersion
	EvidenceGroundingReviewOutputLimit      = runtime.EvidenceGroundingReviewOutputLimit
	EvidenceGroundingSupported              = runtime.EvidenceGroundingSupported
	EvidenceGroundingUnsupported            = runtime.EvidenceGroundingUnsupported
	EvidenceGroundingUncertain              = runtime.EvidenceGroundingUncertain
	KernelAPIVersion                        = kernelapi.APIVersion
	ChannelsCapabilityID                    = kernelapi.ChannelsCapabilityID
	ChannelsCapabilityVersion               = kernelapi.ChannelsCapabilityVersion
	ActivityCapabilityID                    = kernelapi.ActivityCapabilityID
	ActivityCapabilityVersion               = kernelapi.ActivityCapabilityVersion
	SkillActionsCapabilityID                = kernelapi.SkillActionsCapabilityID
	SkillActionsCapabilityVersion           = kernelapi.SkillActionsCapabilityVersion
	SkillBindingsCapabilityID               = kernelapi.SkillBindingsCapabilityID
	SkillBindingsCapabilityVersion          = kernelapi.SkillBindingsCapabilityVersion
	EventRoutingCapabilityID                = kernelapi.EventRoutingCapabilityID
	EventRoutingCapabilityVersion           = kernelapi.EventRoutingCapabilityVersion
	KernelOperationList                     = kernelapi.OperationList
	KernelOperationGet                      = kernelapi.OperationGet
	KernelOperationUpdate                   = kernelapi.OperationUpdate
	KernelOperationPause                    = kernelapi.OperationPause
	KernelOperationResume                   = kernelapi.OperationResume
	KernelOperationRetire                   = kernelapi.OperationRetire
	KernelOperationListCompilations         = kernelapi.OperationListCompilations
	KernelOperationActivate                 = kernelapi.OperationActivate
	KernelOperationRollback                 = kernelapi.OperationRollback
	KernelOperationListActivations          = kernelapi.OperationListActivations
	KernelOperationProposeAmendment         = kernelapi.OperationProposeAmendment
	KernelOperationListAmendments           = kernelapi.OperationListAmendments
	KernelOperationGetAmendment             = kernelapi.OperationGetAmendment
	KernelOperationEvaluateAmendment        = kernelapi.OperationEvaluateAmendment
	KernelOperationResolveAmendment         = kernelapi.OperationResolveAmendment
	KernelOperationActivateAmendment        = kernelapi.OperationActivateAmendment
	KernelOperationRoute                    = kernelapi.OperationRoute
	KernelOperationUpsert                   = kernelapi.OperationUpsert
	KernelOperationDisable                  = kernelapi.OperationDisable
	ChannelOperationCreate                  = kernelapi.OperationCreate
	ChannelOperationGet                     = kernelapi.OperationGet
	ChannelOperationList                    = kernelapi.OperationList
	ChannelOperationPost                    = kernelapi.OperationPost
	ChannelOperationRead                    = kernelapi.OperationRead
	ChannelOperationPresence                = kernelapi.OperationPresence
	ChannelOperationAudit                   = kernelapi.OperationAudit
	ChannelOperationCoordinate              = kernelapi.OperationCoordinate
	ChannelOperationCoordinateAutomatically = kernelapi.OperationCoordinateAuto
	ChannelOperationReceipts                = kernelapi.OperationReceipts
	ChannelOperationChanges                 = kernelapi.OperationChanges
	ChannelOperationStream                  = kernelapi.OperationStream
	KernelOperationUpload                   = kernelapi.OperationUpload
	KernelOperationDownload                 = kernelapi.OperationDownload
	KernelOperationResolve                  = kernelapi.OperationResolve
	KernelOperationRespond                  = kernelapi.OperationRespond
	KernelOperationComplete                 = kernelapi.OperationComplete
	KernelOperationPropose                  = kernelapi.OperationPropose
	KernelOperationEvaluate                 = kernelapi.OperationEvaluate
	KernelOperationApprove                  = kernelapi.OperationApprove
	KernelOperationApply                    = kernelapi.OperationApply
	KernelOperationRetry                    = kernelapi.OperationRetry
	KernelOperationRefine                   = kernelapi.OperationRefine
	KernelOperationPatch                    = kernelapi.OperationPatch
	KernelOperationDeliver                  = kernelapi.OperationDeliver
	HostedSkillApplied                      = runtime.HostedSkillApplied
	HostedSkillNotApplied                   = runtime.HostedSkillNotApplied
	RunbookAPIVersion                       = runbook.APIVersion
	RunbookStepAction                       = runbook.StepAction
	RunbookStepDelegate                     = runbook.StepDelegate
	RunbookDelegateBehavior                 = runbook.DelegateBehavior
	RunbookDelegateReason                   = runbook.DelegateReason
	RunbookStepDecision                     = runbook.StepDecision
	RunbookStepTransform                    = runbook.StepTransform
	RunbookStepWait                         = runbook.StepWait
	RunbookStepFork                         = runbook.StepFork
	RunbookStepJoin                         = runbook.StepJoin
	RunbookStepForEach                      = runbook.StepForEach
	RunbookStepLoopReturn                   = runbook.StepLoopReturn
	RunbookStepEnd                          = runbook.StepEnd
	RunbookJoinAll                          = runbook.JoinAll
	RunbookJoinAny                          = runbook.JoinAny
	RunbookPredicateEqual                   = runbook.PredicateEqual
	RunbookPredicateNotEqual                = runbook.PredicateNotEqual
	RunbookPredicateExists                  = runbook.PredicateExists
	RunbookPredicateTruthy                  = runbook.PredicateTruthy
	RunbookPredicateGreater                 = runbook.PredicateGreater
	RunbookPredicateAtLeast                 = runbook.PredicateAtLeast
	RunbookPredicateLess                    = runbook.PredicateLess
	RunbookPredicateAtMost                  = runbook.PredicateAtMost
	RunbookPredicateContains                = runbook.PredicateContains
	RunbookPredicateAll                     = runbook.PredicateAll
	RunbookPredicateAny                     = runbook.PredicateAny
	RunbookPredicateNot                     = runbook.PredicateNot
)

// ChannelCapability returns the canonical versioned descriptor for the
// channel services an embedding host has actually wired.
func ChannelCapability(features ChannelCapabilityFeatures) KernelCapability {
	return kernelapi.ChannelsCapability(features)
}

func AgentRunsCapability() KernelCapability {
	return kernelapi.AgentRunsCapability()
}

func ObjectivesCapability() KernelCapability {
	return kernelapi.ObjectivesCapability()
}

func EventRoutingCapability() KernelCapability {
	return kernelapi.EventRoutingCapability()
}

func InitiativesCapability() KernelCapability {
	return kernelapi.InitiativesCapability()
}

func SourceMonitorsCapability() KernelCapability {
	return kernelapi.SourceMonitorsCapability()
}

func OutreachLifecycleCapability() KernelCapability {
	return kernelapi.OutreachCapability()
}

func SkillActionsCapability() KernelCapability {
	return kernelapi.SkillActionsCapability()
}

func SkillBindingsCapability(management bool) KernelCapability {
	return kernelapi.SkillBindingsCapability(management)
}

func ActivityCapability() KernelCapability {
	return kernelapi.ActivityCapability()
}

func AgentDefinitionsCapability(features ...AgentDefinitionCapabilityFeatures) KernelCapability {
	return kernelapi.AgentDefinitionsCapability(features...)
}

func AgentRequestsCapability() KernelCapability {
	return kernelapi.AgentRequestsCapability()
}

func ActionApprovalsCapability(features ActionApprovalCapabilityFeatures) KernelCapability {
	return kernelapi.ActionApprovalsCapability(features)
}

func ArtifactsCapability(contentOperations ...string) KernelCapability {
	return kernelapi.ArtifactCapability(contentOperations...)
}

func TeamDefinitionsCapability(features TeamDefinitionCapabilityFeatures) KernelCapability {
	return kernelapi.TeamDefinitionsCapability(features)
}

func WorkforceAuthoringCapability(features WorkforceAuthoringCapabilityFeatures) KernelCapability {
	return kernelapi.WorkforceAuthoringCapability(features)
}

// NewKernelCapabilityDocument composes the exact capability envelope consumed
// by OpenSeal's TUI and embedding-host user interfaces.
func NewKernelCapabilityDocument(capabilities ...KernelCapability) KernelCapabilityDocument {
	return kernelapi.NewCapabilityDocument(capabilities...)
}

// ValidateWorkforceCredentialPlacement applies the canonical kind-keyed,
// secret-safe credential placement contract at an embedding host boundary.
func ValidateWorkforceCredentialPlacement(candidate *WorkforceCandidate, required map[string][]string, placement WorkforceChangeSetPlacement, choices []CredentialBindingChoice) error {
	return authoring.ValidateCredentialPlacement(candidate, required, placement, choices)
}

var (
	NewHostedTurnRunner                = runtime.NewHostedTurnRunner
	MarshalHostedTurnModelInput        = runtime.MarshalHostedTurnModelInput
	MarshalEvidenceGroundingModelInput = runtime.MarshalEvidenceGroundingModelInput
	EstimateHostedTurnInputTokens      = runtime.EstimateHostedTurnInputTokens
	NewCapabilityInvocationTurnRunner  = runtime.NewCapabilityInvocationTurnRunner
	NewOutreachTurnRunner              = runtime.NewOutreachTurnRunner
	NewOutreachActionProposalObserver  = runtime.NewOutreachActionProposalObserver
	ResolveCatalogTurnRunner           = runtime.ResolveCatalogTurnRunner
	NewArtifactRetentionService        = runtime.NewArtifactRetentionService
	ErrTurnHostUnavailable             = runtime.ErrTurnHostUnavailable
	ErrTurnHostConfiguration           = runtime.ErrTurnHostConfiguration
	ValidateRunbook                    = runbook.Validate
	NewRunbookTurnRunner               = runtime.NewRunbookTurnRunner
	NewRunForkCoordinator              = runtime.NewRunForkCoordinator
	ObjectiveManagementSkill           = runtime.ObjectiveManagementSkill
	NewObjectiveActionValidator        = runtime.NewObjectiveActionValidator
	NewObjectiveActionDispatcher       = runtime.NewObjectiveActionDispatcher
	TeamManagementSkill                = runtime.TeamManagementSkill
	NewTeamRoleActionValidator         = runtime.NewTeamRoleActionValidator
	NewTeamRoleActionDispatcher        = runtime.NewTeamRoleActionDispatcher
	NewTeamSkillActionValidator        = runtime.NewTeamSkillActionValidator
	AuthorizeTeamSkillActivation       = runtime.AuthorizeTeamSkillActivation
	SkillManagementSkill               = runtime.SkillManagementSkill
	NewSkillBindingActionValidator     = runtime.NewSkillBindingActionValidator
	NewSkillBindingActionDispatcher    = runtime.NewSkillBindingActionDispatcher
	NewHTTPActionExecutor              = httpaction.NewExecutor
	DecodeHTTPActionInvocation         = httpaction.DecodeInvocation
)

// WorkforceObjectiveKey returns the canonical placement key for an objective
// template owned by an agent or team definition.
func WorkforceObjectiveKey(ownerType, definitionID, templateID string) string {
	return authoring.WorkforceObjectiveKey(ownerType, definitionID, templateID)
}

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
	runtime.InitiativeStore
	runtime.SourceMonitorStore
	kernelagent.Store
	kernelteam.Store
	skill.CatalogStore
	sourceartifact.Store
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
	ErrSourceObservationNotFound           = runtime.ErrSourceObservationNotFound
	ErrSourceObservationConflict           = runtime.ErrSourceObservationConflict
	ErrSourceMonitorCheckpoint             = runtime.ErrSourceMonitorCheckpoint
	ErrInvalidSourceObservation            = runtime.ErrInvalidSourceObservation
	ErrEventSourceCheckpointConflict       = runtime.ErrEventSourceCheckpointConflict
	ErrInvalidEventSourceCheckpoint        = runtime.ErrInvalidEventSourceCheckpoint
	ErrInvalidOwner                        = runtime.ErrInvalidOwner
	ErrInitiativeNotFound                  = runtime.ErrInitiativeNotFound
	ErrInitiativeConflict                  = runtime.ErrInitiativeConflict
	ErrInitiativeIdempotency               = runtime.ErrInitiativeIdempotency
	ErrInitiativeNoChanges                 = runtime.ErrInitiativeNoChanges
	ErrInvalidInitiative                   = runtime.ErrInvalidInitiative
	ErrObjectiveNotFound                   = runtime.ErrObjectiveNotFound
	ErrObjectiveIdempotency                = runtime.ErrObjectiveIdempotency
	ErrInvalidObjectiveTransition          = runtime.ErrInvalidObjectiveTransition
	ErrBudgetExhausted                     = runtime.ErrBudgetExhausted
	ErrTurnNotFound                        = runtime.ErrTurnNotFound
	ErrActionNotFound                      = runtime.ErrActionNotFound
	ErrApprovalNotFound                    = runtime.ErrApprovalNotFound
	ErrApprovalResolved                    = runtime.ErrApprovalResolved
	ErrActionIdempotencyConflict           = runtime.ErrIdempotencyConflict
	ErrActionCredentialLeaseInvalid        = runtime.ErrActionCredentialLeaseInvalid
	ErrActionCredentialLeaseExpired        = runtime.ErrActionCredentialLeaseExpired
	ErrActionCredentialLeaseMismatch       = runtime.ErrActionCredentialLeaseMismatch
	ErrActionCredentialLeaseReplay         = runtime.ErrActionCredentialLeaseReplay
	ErrAgentRequestNotFound                = runtime.ErrAgentRequestNotFound
	ErrInvalidAgentRequestState            = runtime.ErrInvalidAgentRequestState
	ErrAgentRequestUnauthorized            = runtime.ErrAgentRequestUnauthorized
	ErrAgentRequestAssignment              = runtime.ErrAgentRequestAssignment
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
	ErrAgentDefinitionNotFound             = kernelagent.ErrDefinitionNotFound
	ErrAgentDeploymentNotFound             = kernelagent.ErrDeploymentNotFound
	ErrAgentAmendmentNotFound              = kernelagent.ErrAmendmentNotFound
	ErrAgentDeploymentRevisionConflict     = kernelagent.ErrRevisionConflict
	ErrTeamDefinitionNotFound              = kernelteam.ErrDefinitionNotFound
	ErrTeamDeploymentNotFound              = kernelteam.ErrDeploymentNotFound
	ErrTeamDeploymentRevisionConflict      = kernelteam.ErrRevisionConflict
	ErrTeamAmendmentNotFound               = kernelteam.ErrAmendmentNotFound
)

func NewToolActionDispatcher(invoker runtime.ToolInvoker) (*runtime.ToolActionDispatcher, error) {
	return runtime.NewToolActionDispatcher(invoker)
}

func NewActionCredentialLease(request CreateActionCredentialLeaseRequest) (*ActionCredentialLease, error) {
	return runtime.NewActionCredentialLease(request)
}

func SignActionCredentialLease(ctx context.Context, lease ActionCredentialLease, signer ActionCredentialLeaseSigner) (*SignedActionCredentialLease, error) {
	return runtime.SignActionCredentialLease(ctx, lease, signer)
}

func NewActionCredentialLeaseValidator(verifier ActionCredentialLeaseSignatureVerifier, redeemer ActionCredentialLeaseRedeemer) (*ActionCredentialLeaseValidator, error) {
	return runtime.NewActionCredentialLeaseValidator(verifier, redeemer)
}

func MatchActionCredentialLease(lease ActionCredentialLease, call *ActionCall, run *AgentRun, transport string, credentialFields map[string][]string) error {
	return runtime.MatchActionCredentialLease(lease, call, run, transport, credentialFields)
}

func MatchActionCredentialLeaseReferences(envelope *SignedActionCredentialLease, call *ActionCall, run *AgentRun, transport string) error {
	return runtime.MatchActionCredentialLeaseReferences(envelope, call, run, transport)
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

type PostgresMigrationEvent = runtime.PostgresMigrationEvent
type PostgresMigrationPhase = runtime.PostgresMigrationPhase
type PostgresMigrationStats = runtime.PostgresMigrationStats

const (
	PostgresMigrationWaiting  = runtime.PostgresMigrationWaiting
	PostgresMigrationAcquired = runtime.PostgresMigrationAcquired
	PostgresMigrationComplete = runtime.PostgresMigrationComplete
	PostgresMigrationTimeout  = runtime.PostgresMigrationTimeout
)

var ErrPostgresMigrationLockTimeout = runtime.ErrPostgresMigrationLockTimeout

// NewSQLiteStore opens the standalone durable kernel store.
func NewSQLiteStore(path string) (*runtime.SQLiteStore, error) {
	return runtime.NewSQLiteStore(path)
}

func WithPostgresSchema(schema string) runtime.PostgresStoreOption {
	return runtime.WithPostgresSchema(schema)
}

func DefaultPostgresPoolConfig() runtime.PostgresPoolConfig {
	return runtime.DefaultPostgresPoolConfig()
}

func WithPostgresPool(pool runtime.PostgresPoolConfig) runtime.PostgresStoreOption {
	return runtime.WithPostgresPool(pool)
}

func WithPostgresMigrationLock(timeout, pollInterval time.Duration) runtime.PostgresStoreOption {
	return runtime.WithPostgresMigrationLock(timeout, pollInterval)
}

func WithPostgresMigrationObserver(observer runtime.PostgresMigrationObserver) runtime.PostgresStoreOption {
	return runtime.WithPostgresMigrationObserver(observer)
}

func CompileOpenClawSkill(bundle skillopenclaw.Bundle) (*skillopenclaw.Compilation, error) {
	return skillopenclaw.Compile(bundle)
}

// CanonicalOpenClawTrust projects mutable registry verification responses into
// stable immutable Skill provenance while retained source artifacts preserve
// the exact original response.
func CanonicalOpenClawTrust(input map[string]interface{}) map[string]interface{} {
	return skillopenclaw.CanonicalTrust(input)
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
	WorkforceAuthoringCreate                             = authoring.ModeCreate
	WorkforceAuthoringAmend                              = authoring.ModeAmend
	WorkforceChangeSetBlocked                            = authoring.ChangeSetBlocked
	WorkforceChangeSetReview                             = authoring.ChangeSetReview
	WorkforceChangeSetEvaluating                         = authoring.ChangeSetEvaluating
	WorkforceChangeSetAwaitingApproval                   = authoring.ChangeSetAwaitingApproval
	WorkforceChangeSetReady                              = authoring.ChangeSetReady
	WorkforceChangeSetApplied                            = authoring.ChangeSetApplied
	WorkforceChangeSetRejected                           = authoring.ChangeSetRejected
	WorkforceChangeSetFailed                             = authoring.ChangeSetFailed
	WorkforceInitiativeOwnerAgent                        = authoring.InitiativeOwnerAgent
	WorkforceInitiativeOwnerTeam                         = authoring.InitiativeOwnerTeam
	WorkforceInitiativeDeduplicateStableSource           = authoring.InitiativeDeduplicateStableSource
	WorkforceInitiativeDeduplicateContentDigest          = authoring.InitiativeDeduplicateContentDigest
	WorkforceInitiativeDeduplicateStableSourceAndContent = authoring.InitiativeDeduplicateStableSourceAndContent

	OwnerTypeAgent = runtime.OwnerTypeAgent
	OwnerTypeTeam  = runtime.OwnerTypeTeam

	ObjectiveStatusDraft             = runtime.ObjectiveStatusDraft
	ObjectiveStatusActive            = runtime.ObjectiveStatusActive
	ObjectiveStatusPaused            = runtime.ObjectiveStatusPaused
	ObjectiveStatusSatisfied         = runtime.ObjectiveStatusSatisfied
	ObjectiveStatusFailed            = runtime.ObjectiveStatusFailed
	ObjectiveStatusRetired           = runtime.ObjectiveStatusRetired
	ObjectiveManagementSkillID       = runtime.ObjectiveManagementSkillID
	ObjectiveManagementSkillVersion  = runtime.ObjectiveManagementSkillVersion
	ObjectiveActionCreate            = runtime.ObjectiveActionCreate
	ObjectiveActionUpdate            = runtime.ObjectiveActionUpdate
	ObjectiveActionPause             = runtime.ObjectiveActionPause
	TeamManagementSkillID            = runtime.TeamManagementSkillID
	TeamManagementSkillVersion       = runtime.TeamManagementSkillVersion
	TeamActionUpdateRole             = runtime.TeamActionUpdateRole
	SkillManagementSkillID           = runtime.SkillManagementSkillID
	SkillManagementSkillVersion      = runtime.SkillManagementSkillVersion
	SkillActionDiscover              = runtime.SkillActionDiscoverBinding
	SkillActionUpsertBinding         = runtime.SkillActionUpsertBinding
	SkillActionDisableBinding        = runtime.SkillActionDisableBinding
	ObjectiveCadenceInterval         = runtime.ObjectiveCadenceInterval
	ObjectiveCadenceDaily            = runtime.ObjectiveCadenceDaily
	ObjectiveCadenceWeekly           = runtime.ObjectiveCadenceWeekly
	ObjectiveCadenceCron             = runtime.ObjectiveCadenceCron
	ObjectiveScheduleBackpressured   = runtime.ObjectiveScheduleBackpressured
	ObjectiveScheduleSuspended       = runtime.ObjectiveScheduleSuspended
	ObjectiveScheduleBudgetExhausted = runtime.ObjectiveScheduleBudgetExhausted

	InitiativeStatusDraft                             = runtime.InitiativeStatusDraft
	InitiativeStatusActive                            = runtime.InitiativeStatusActive
	InitiativeStatusPaused                            = runtime.InitiativeStatusPaused
	InitiativeStatusCompleted                         = runtime.InitiativeStatusCompleted
	InitiativeStatusFailed                            = runtime.InitiativeStatusFailed
	InitiativeStatusCanceled                          = runtime.InitiativeStatusCanceled
	InitiativeStatusArchived                          = runtime.InitiativeStatusArchived
	InitiativeMilestonePending                        = runtime.MilestonePending
	InitiativeMilestoneInProgress                     = runtime.MilestoneInProgress
	InitiativeMilestoneCompleted                      = runtime.MilestoneCompleted
	InitiativeMilestoneBlocked                        = runtime.MilestoneBlocked
	InitiativeMilestoneCanceled                       = runtime.MilestoneCanceled
	InitiativeHypothesisOpen                          = runtime.HypothesisOpen
	InitiativeHypothesisSupported                     = runtime.HypothesisSupported
	InitiativeHypothesisContradicted                  = runtime.HypothesisContradicted
	InitiativeHypothesisInconclusive                  = runtime.HypothesisInconclusive
	InitiativeDeliverablePlanned                      = runtime.DeliverablePlanned
	InitiativeDeliverableInProgress                   = runtime.DeliverableInProgress
	InitiativeDeliverableReview                       = runtime.DeliverableReview
	InitiativeDeliverableDelivered                    = runtime.DeliverableDelivered
	InitiativeDeliverableCanceled                     = runtime.DeliverableCanceled
	InitiativeSourceDeduplicateStableSource           = runtime.SourceMonitorDeduplicateStableSource
	InitiativeSourceDeduplicateContentDigest          = runtime.SourceMonitorDeduplicateContentDigest
	InitiativeSourceDeduplicateStableSourceAndContent = runtime.SourceMonitorDeduplicateStableSourceAndContent
	OutreachThreadOpen                                = runtime.OutreachThreadOpen
	OutreachThreadClosed                              = runtime.OutreachThreadClosed
	OutreachThreadCanceled                            = runtime.OutreachThreadCanceled
	OutreachMessageOutbound                           = runtime.OutreachMessageOutbound
	OutreachMessageInbound                            = runtime.OutreachMessageInbound
	OutreachIntentClarify                             = runtime.OutreachIntentClarify
	OutreachIntentRequestFeedback                     = runtime.OutreachIntentRequestFeedback
	OutreachIntentAnswer                              = runtime.OutreachIntentAnswer
	OutreachIntentFollowUp                            = runtime.OutreachIntentFollowUp
	OutreachMessageDraft                              = runtime.OutreachMessageDraft
	OutreachMessagePendingApproval                    = runtime.OutreachMessagePendingApproval
	OutreachMessageReady                              = runtime.OutreachMessageReady
	OutreachMessageDelivered                          = runtime.OutreachMessageDelivered
	OutreachMessageReceived                           = runtime.OutreachMessageReceived
	OutreachMessageDeclined                           = runtime.OutreachMessageDeclined
	OutreachMessageFailed                             = runtime.OutreachMessageFailed
	OutreachMessageCanceled                           = runtime.OutreachMessageCanceled
	OutreachInvocationContextKey                      = runtime.OutreachInvocationContextKey

	RunSourceManual          = runtime.RunSourceManual
	RunSourceChat            = runtime.RunSourceChat
	RunSourceSchedule        = runtime.RunSourceSchedule
	RunSourceEvent           = runtime.RunSourceEvent
	RunSourceWebhook         = runtime.RunSourceWebhook
	RunSourceRequest         = runtime.RunSourceRequest
	RunSourceHandoff         = runtime.RunSourceHandoff
	RunSourceObjective       = runtime.RunSourceObjective
	RunSourceFork            = runtime.RunSourceFork
	DelegationModeContextKey = runtime.DelegationModeContextKey

	RunKindAgentWork          = runtime.RunKindAgentWork
	RunKindConversation       = runtime.RunKindConversation
	RunKindWorkforceAuthoring = runtime.RunKindWorkforceAuthoring

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
	AuthorityProgressionOwnerAgent    = progression.OwnerAgent
	AuthorityProgressionOwnerTeam     = progression.OwnerTeam
	AuthorityProgressionPromote       = progression.DirectionPromote
	AuthorityProgressionNoChange      = progression.DirectionNoChange
	AuthorityProgressionRegress       = progression.DirectionRegress

	AgentRunStatusQueued               = runtime.AgentRunStatusQueued
	AgentRunOrderScheduler             = runtime.AgentRunOrderScheduler
	AgentRunOrderCreatedDesc           = runtime.AgentRunOrderCreatedDesc
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
	AgentRequestStatusFailed                 = runtime.AgentRequestStatusFailed
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

	SkillDiscoveryBindable          = skill.DiscoveryReadinessBindable
	SkillDiscoveryNeedsInstallation = skill.DiscoveryReadinessNeedsInstallation
	SkillDiscoveryUnavailable       = skill.DiscoveryReadinessUnavailable

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
	SkillAdapterPreparedRuntime = skill.AdapterPreparedRuntime
	SkillAdapterHTTPAction      = skill.AdapterHTTPAction
	HTTPActionTransportName     = httpaction.TransportName

	SkillRuntimePreparing   = skill.RuntimePreparationPreparing
	SkillRuntimeReady       = skill.RuntimePreparationReady
	SkillRuntimeUnavailable = skill.RuntimePreparationUnavailable

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

	AgentRolloutPending    = kernelagent.RolloutPending
	AgentRolloutActive     = kernelagent.RolloutActive
	AgentRolloutDegraded   = kernelagent.RolloutDegraded
	AgentRolloutPaused     = kernelagent.RolloutPaused
	AgentRolloutRetired    = kernelagent.RolloutRetired
	AgentCompilationClean  = kernelagent.CompilationClean
	AgentCompilationFailed = kernelagent.CompilationFailed

	AgentAmendmentEvaluating       = kernelagent.AmendmentEvaluating
	AgentAmendmentAwaitingApproval = kernelagent.AmendmentAwaitingApproval
	AgentAmendmentReady            = kernelagent.AmendmentReady
	AgentAmendmentApproved         = kernelagent.AmendmentApproved
	AgentAmendmentRejected         = kernelagent.AmendmentRejected
	AgentAmendmentEvaluationFailed = kernelagent.AmendmentEvaluationFailed
	AgentAmendmentActivated        = kernelagent.AmendmentActivated

	ArtifactClassPublic        = runtime.ArtifactClassificationPublic
	ArtifactClassInternal      = runtime.ArtifactClassificationInternal
	ArtifactClassConfidential  = runtime.ArtifactClassificationConfidential
	ArtifactClassRestricted    = runtime.ArtifactClassificationRestricted
	ArtifactContentAvailable   = runtime.ArtifactContentAvailable
	ArtifactContentUnavailable = runtime.ArtifactContentUnavailable

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

const (
	SourceMonitorDeduplicateStableSource           = runtime.SourceMonitorDeduplicateStableSource
	SourceMonitorDeduplicateContentDigest          = runtime.SourceMonitorDeduplicateContentDigest
	SourceMonitorDeduplicateStableSourceAndContent = runtime.SourceMonitorDeduplicateStableSourceAndContent
)

var (
	ErrWorkforceChangeSetNotFound    = authoring.ErrChangeSetNotFound
	ErrWorkforceChangeSetIdempotency = authoring.ErrChangeSetIdempotency
	ErrWorkforceChangeSetRevision    = authoring.ErrChangeSetRevision
	ErrWorkforceChangeSetTransition  = authoring.ErrChangeSetTransition
	ErrSkillDefinitionImmutable      = skill.ErrDefinitionImmutable
	ErrSkillDefinitionAmbiguous      = skill.ErrDefinitionAmbiguous
	ErrSkillBindingAmbiguous         = skill.ErrBindingAmbiguous
	ErrSkillBindingRevisionConflict  = skill.ErrBindingRevisionConflict
	ErrSkillBindingNotFound          = skill.ErrBindingNotFound
	ErrSkillBindingAlreadyDisabled   = skill.ErrBindingAlreadyDisabled
	ErrSkillSourceArtifactNotFound   = sourceartifact.ErrNotFound
	ErrSkillSourceArtifactImmutable  = sourceartifact.ErrImmutable
	ErrSkillSourceReferenceConflict  = sourceartifact.ErrReferenceConflict
	ErrSkillSourceOriginAmbiguous    = sourceartifact.ErrAmbiguousOrigin
	ErrOutreachThreadNotFound        = runtime.ErrOutreachThreadNotFound
	ErrOutreachThreadConflict        = runtime.ErrOutreachThreadConflict
	ErrOutreachThreadIdempotency     = runtime.ErrOutreachThreadIdempotency
	ErrInvalidOutreachThread         = runtime.ErrInvalidOutreachThread
	ErrInvalidObjectiveEventRules    = runtime.ErrInvalidObjectiveEventRules
	ErrInvalidAuthorityEvaluation    = progression.ErrInvalidEvaluation
	ErrAuthorityPolicyCeiling        = progression.ErrPolicyCeiling
	ErrAuthorityRecommendationStale  = progression.ErrRecommendationStale
	ErrAuthorityProgressionNoChange  = progression.ErrNoChange
)

// Engine is the primary entry point for OpenSeal.
// It wires together the registry, execution store, worker pool, and scheduler.
type Engine struct {
	registry                      *executor.Registry
	store                         runtime.KernelStore
	pool                          *runtime.WorkerPool
	scheduler                     *runtime.Scheduler
	portfolio                     *runtime.PortfolioService
	initiatives                   *runtime.InitiativeService
	sourceMonitors                *runtime.SourceMonitorService
	eventSources                  *runtime.EventSourceCheckpointService
	outreach                      *runtime.OutreachService
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
	actionValidators              []runtime.ActionProposalValidator
	teamManagementActions         bool
	skillManagementActions        bool
	skillDiscovery                skill.DiscoveryProvider
	approvalAuth                  runtime.ApprovalAuthorizer
	clawHub                       *clawhub.InstallManager
	clawHubPreview                *clawhub.PreviewManager
	clawHubRegistry               clawhub.Registry
	skillSources                  *sourceartifact.Service
	clawHubSourceScope            skill.ScopeReference
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
	progression                   *progression.Service
	authoring                     *authoring.Compiler
	authoringChanges              *authoring.ChangeSetService
	authoringRuns                 *runtime.WorkforceAuthoringRunService
	logger                        *zap.SugaredLogger
	workerLimiter                 *runtime.WorkerLimiter
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
	leaseIssuer runtime.ActionCredentialLeaseIssuer
	dispatcher  runtime.ActionDispatcher
}

type actionWorkerSupervisorSpec struct {
	config      runtime.DynamicActionWorkerConfig
	source      runtime.WorkerScopeSource
	credentials runtime.CredentialResolver
	leaseIssuer runtime.ActionCredentialLeaseIssuer
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
		initiatives:         runtime.NewInitiativeService(store, store),
		sourceMonitors:      runtime.NewSourceMonitorService(store, store, store, store),
		eventSources:        runtime.NewEventSourceCheckpointService(store),
		outreach:            runtime.NewOutreachService(store, store, store, store),
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
	e.progression = progression.NewService(e.agents, e.teams)

	for _, opt := range opts {
		if err := opt(e); err != nil {
			return nil, fmt.Errorf("engine option: %w", err)
		}
	}
	if err := e.rebuildAuthoringChangeSets(); err != nil {
		return nil, fmt.Errorf("workforce change set configuration: %w", err)
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
	// Guard the host dispatcher at the final external execution boundary before
	// adding kernel-owned management routes. Skill and Team management actions
	// enforce their own resource authority and must retain their fallthrough to
	// the host chain; ordinary Agent and Team Skill calls still pass through this
	// guard immediately before the embedding host is invoked.
	for index := range e.actionPoolSpecs {
		dispatcher, dispatchErr := runtime.NewTeamSkillActionDispatcher(e, e.actionPoolSpecs[index].dispatcher)
		if dispatchErr != nil {
			return nil, fmt.Errorf("Team Skill action worker configuration: %w", dispatchErr)
		}
		e.actionPoolSpecs[index].dispatcher = dispatcher
	}
	for index := range e.actionSupervisorSpecs {
		dispatcher, dispatchErr := runtime.NewTeamSkillActionDispatcher(e, e.actionSupervisorSpecs[index].dispatcher)
		if dispatchErr != nil {
			return nil, fmt.Errorf("Team Skill dynamic action worker configuration: %w", dispatchErr)
		}
		e.actionSupervisorSpecs[index].dispatcher = dispatcher
	}
	if err := e.configureSkillManagementActions(); err != nil {
		return nil, fmt.Errorf("Skill management action configuration: %w", err)
	}
	if err := e.configureTeamManagementActions(); err != nil {
		return nil, fmt.Errorf("Team management action configuration: %w", err)
	}
	teamSkillValidator, err := runtime.NewTeamSkillActionValidator(e)
	if err != nil {
		return nil, fmt.Errorf("Team Skill authority configuration: %w", err)
	}
	e.actionValidators = append(e.actionValidators, teamSkillValidator)
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
	return e.Schedule(ctx, entry, triggerData)
}

// Schedule enqueues an already-built deterministic workflow entry. It lets
// compatibility API and trigger adapters share the Engine-owned worker pool
// instead of constructing a second scheduler and lifecycle.
func (e *Engine) Schedule(ctx context.Context, entry runtime.WorkflowEntry, triggerData map[string]interface{}) (int, error) {
	if e == nil || e.scheduler == nil {
		return 0, errors.New("workflow scheduler is unavailable")
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

// WorkerConcurrencyStats exposes the process-wide worker admission state for
// host metrics and rollout diagnostics.
func (e *Engine) WorkerConcurrencyStats() runtime.WorkerLimiterStats {
	return e.workerLimiter.Stats()
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
		e.pool.SetWorkerLimiter(e.workerLimiter)
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
		e.eventSources = runtime.NewEventSourceCheckpointService(store)
		if initiativeStore, ok := store.(runtime.InitiativeStore); ok {
			e.initiatives = runtime.NewInitiativeService(initiativeStore, store)
			if sourceMonitorStore, supported := store.(runtime.SourceMonitorStore); supported {
				artifactStore, _ := store.(runtime.ArtifactStore)
				e.sourceMonitors = runtime.NewSourceMonitorService(sourceMonitorStore, initiativeStore, store, artifactStore)
				if outreachStore, outreachSupported := store.(runtime.OutreachStore); outreachSupported {
					actionReader, actionsSupported := store.(runtime.OutreachActionReader)
					if actionsSupported {
						e.outreach = runtime.NewOutreachService(outreachStore, initiativeStore, sourceMonitorStore, actionReader)
					} else {
						e.outreach = nil
					}
				} else {
					e.outreach = nil
				}
			} else {
				e.sourceMonitors = nil
				e.outreach = nil
			}
		} else {
			e.initiatives = nil
			e.sourceMonitors = nil
			e.outreach = nil
		}
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
		e.progression = progression.NewService(e.agents, e.teams)
		if skillStore, ok := store.(skill.CatalogStore); ok {
			e.skills = skill.NewCatalogWithStore(skillStore)
		}
		if sourceStore, ok := store.(sourceartifact.Store); ok {
			e.skillSources, _ = sourceartifact.NewService(sourceStore)
		}
		return nil
	}
}

// WithSkillSourceArtifactStore configures immutable source persistence
// independently from runtime state. Hosts may use this for governed blob or
// filesystem adapters while retaining the same portable Engine API.
func WithSkillSourceArtifactStore(store sourceartifact.Store) Option {
	return func(e *Engine) error {
		service, err := sourceartifact.NewService(store)
		if err != nil {
			return err
		}
		e.skillSources = service
		return nil
	}
}

// WithClawHubSourceArtifactScope makes verified lifecycle operations retain
// source bytes under one host-authorized scope. Discovery remains read-only.
func WithClawHubSourceArtifactScope(scope skill.ScopeReference) Option {
	return func(e *Engine) error {
		if strings.TrimSpace(scope.Kind) == "" || strings.TrimSpace(scope.ID) == "" {
			return errors.New("ClawHub source artifact scope is required")
		}
		e.clawHubSourceScope = scope
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
		e.pool.SetWorkerLimiter(e.workerLimiter)
		e.scheduler = runtime.NewScheduler(e.pool, e.store)
		return nil
	}
}

// WithWorkerConcurrencyLimit bounds all workflow, Agent, conversation, and
// action execution admitted by one Engine process. Replica safety remains
// durable in the store; this protects shared host resources as scope count
// grows and while rolling deployments overlap.
func WithWorkerConcurrencyLimit(limit int) Option {
	return func(e *Engine) error {
		limiter, err := runtime.NewWorkerLimiter(limit)
		if err != nil {
			return err
		}
		e.workerLimiter = limiter
		e.pool.SetWorkerLimiter(limiter)
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

// WithWorkforceAuthoringGenerator enables prompt-first workforce compilation.
// The generator is the only probabilistic boundary; OpenSeal validates every
// returned Agent and Team candidate and never activates compiled state.
func WithWorkforceAuthoringGenerator(generator authoring.Generator) Option {
	return func(e *Engine) error {
		compiler, err := authoring.NewCompiler(generator)
		if err != nil {
			return err
		}
		e.authoring = compiler
		return nil
	}
}

func (e *Engine) rebuildAuthoringChangeSets() error {
	e.authoringChanges = nil
	e.authoringRuns = nil
	if e.authoring == nil {
		return nil
	}
	store, ok := e.store.(authoring.ChangeSetStore)
	if !ok {
		return nil
	}
	service, err := authoring.NewChangeSetService(e.authoring, store)
	if err != nil {
		return err
	}
	e.authoringChanges = service
	if runStore, ok := e.store.(runtime.WorkforceAuthoringRunStore); ok {
		runService, err := runtime.NewWorkforceAuthoringRunService(e.authoring, runStore)
		if err != nil {
			return err
		}
		e.authoringRuns = runService
	}
	return nil
}

// NewOpenAICompatibleWorkforceGenerator creates the portable chat-completions
// adapter used by both standalone OpenSeal and embedding hosts.
func NewOpenAICompatibleWorkforceGenerator(endpoint, apiKey, model string, client *http.Client) (authoring.Generator, error) {
	return authoring.NewOpenAICompatibleGenerator(endpoint, apiKey, model, client)
}

// OpenAICompatibleWorkforceGeneratorOptions contains explicit capabilities
// negotiated by an embedding host for its selected provider model.
type OpenAICompatibleWorkforceGeneratorOptions = authoring.OpenAICompatibleGeneratorOptions

type OpenAICompatibleWorkforceThinkingMode = authoring.OpenAICompatibleThinkingMode

const (
	OpenAICompatibleWorkforceThinkingDefault  = authoring.OpenAICompatibleThinkingDefault
	OpenAICompatibleWorkforceThinkingEnabled  = authoring.OpenAICompatibleThinkingEnabled
	OpenAICompatibleWorkforceThinkingDisabled = authoring.OpenAICompatibleThinkingDisabled
)

// NewOpenAICompatibleWorkforceGeneratorWithOptions creates the portable
// adapter with explicit provider capabilities. The default constructor remains
// capability-neutral and emits no vendor-specific transport fields.
func NewOpenAICompatibleWorkforceGeneratorWithOptions(endpoint, apiKey, model string, client *http.Client, options OpenAICompatibleWorkforceGeneratorOptions) (authoring.Generator, error) {
	return authoring.NewOpenAICompatibleGeneratorWithOptions(endpoint, apiKey, model, client, options)
}

func NewWorkforceAuthoringRunService(generator authoring.Generator, store runtime.WorkforceAuthoringRunStore) (*runtime.WorkforceAuthoringRunService, error) {
	compiler, err := authoring.NewCompiler(generator)
	if err != nil {
		return nil, err
	}
	return runtime.NewWorkforceAuthoringRunService(compiler, store)
}

func NewWorkforceAuthoringWorker(service *runtime.WorkforceAuthoringRunService, logger *zap.SugaredLogger, config runtime.WorkforceAuthoringWorkerConfig) (*runtime.WorkforceAuthoringWorker, error) {
	return runtime.NewWorkforceAuthoringWorker(service, logger, config)
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

// WithActionProposalValidators adds deterministic, kernel-owned validation to
// the governed action proposal path. Validators run before policy evaluation
// and before an ApprovalCheckpoint is persisted.
func WithActionProposalValidators(validators ...runtime.ActionProposalValidator) Option {
	return func(e *Engine) error {
		for _, validator := range validators {
			if validator == nil {
				return fmt.Errorf("action proposal validator is required")
			}
			e.actionValidators = append(e.actionValidators, validator)
		}
		return nil
	}
}

// WithTeamManagementActions enables the portable, governed Team action layer.
// The Engine owns its Team registry, so embedding hosts never need to import
// internal registry implementations or duplicate dispatcher composition.
func WithTeamManagementActions() Option {
	return func(e *Engine) error {
		e.teamManagementActions = true
		return nil
	}
}

// WithSkillManagementActions enables the portable, governed Agent Skill
// binding action layer. The Engine owns both catalog and durable runtime, so
// hosts do not compose private validators or dispatchers.
func WithSkillManagementActions(discovery ...skill.DiscoveryProvider) Option {
	return func(e *Engine) error {
		if len(discovery) > 1 {
			return fmt.Errorf("only one Skill discovery provider can be configured")
		}
		if len(discovery) == 1 {
			if discovery[0] == nil {
				return fmt.Errorf("Skill discovery provider is required when configured")
			}
			e.skillDiscovery = discovery[0]
		}
		e.skillManagementActions = true
		return nil
	}
}

func (e *Engine) configureSkillManagementActions() error {
	if !e.skillManagementActions {
		return nil
	}
	if err := e.skills.Register(context.Background(), runtime.SkillManagementSkill()); err != nil && !errors.Is(err, skill.ErrDefinitionImmutable) {
		return err
	}
	validator, err := runtime.NewSkillBindingActionValidator(e.skills)
	if err != nil {
		return err
	}
	e.actionValidators = append(e.actionValidators, validator)
	for index := range e.actionPoolSpecs {
		dispatcher, dispatchErr := runtime.NewSkillBindingActionDispatcher(e.store, e.skills, e.actionPoolSpecs[index].dispatcher, optionalSkillDiscovery(e.skillDiscovery)...)
		if dispatchErr != nil {
			return dispatchErr
		}
		e.actionPoolSpecs[index].dispatcher = dispatcher
	}
	for index := range e.actionSupervisorSpecs {
		dispatcher, dispatchErr := runtime.NewSkillBindingActionDispatcher(e.store, e.skills, e.actionSupervisorSpecs[index].dispatcher, optionalSkillDiscovery(e.skillDiscovery)...)
		if dispatchErr != nil {
			return dispatchErr
		}
		e.actionSupervisorSpecs[index].dispatcher = dispatcher
	}
	return nil
}

func optionalSkillDiscovery(provider skill.DiscoveryProvider) []skill.DiscoveryProvider {
	if provider == nil {
		return nil
	}
	return []skill.DiscoveryProvider{provider}
}

func (e *Engine) configureTeamManagementActions() error {
	if !e.teamManagementActions {
		return nil
	}
	validator, err := runtime.NewTeamRoleActionValidator(e.teams)
	if err != nil {
		return err
	}
	e.actionValidators = append(e.actionValidators, validator)
	for index := range e.actionPoolSpecs {
		dispatcher, dispatchErr := runtime.NewTeamRoleActionDispatcher(e.store, e.teams, e.actionPoolSpecs[index].dispatcher)
		if dispatchErr != nil {
			return dispatchErr
		}
		e.actionPoolSpecs[index].dispatcher = dispatcher
	}
	for index := range e.actionSupervisorSpecs {
		dispatcher, dispatchErr := runtime.NewTeamRoleActionDispatcher(e.store, e.teams, e.actionSupervisorSpecs[index].dispatcher)
		if dispatchErr != nil {
			return dispatchErr
		}
		e.actionSupervisorSpecs[index].dispatcher = dispatcher
	}
	return nil
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

// WithActionCredentialLeaseWorkers dispatches credentialed actions with a
// signed opaque lease instead of resolving plaintext values inside OpenSeal.
func WithActionCredentialLeaseWorkers(config runtime.ActionWorkerConfig, issuer runtime.ActionCredentialLeaseIssuer, dispatcher runtime.ActionDispatcher) Option {
	return func(e *Engine) error {
		if issuer == nil || dispatcher == nil {
			return fmt.Errorf("action credential lease issuer and dispatcher are required")
		}
		e.actionPoolSpecs = append(e.actionPoolSpecs, actionWorkerSpec{config: config, leaseIssuer: issuer, dispatcher: dispatcher})
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

// WithDynamicActionCredentialLeaseWorkers is the multi-tenant counterpart of
// WithActionCredentialLeaseWorkers. Each scope keeps independent durable
// action claims while the issuer derives one signed lease per execution.
func WithDynamicActionCredentialLeaseWorkers(config runtime.DynamicActionWorkerConfig, source runtime.WorkerScopeSource, issuer runtime.ActionCredentialLeaseIssuer, dispatcher runtime.ActionDispatcher) Option {
	return func(e *Engine) error {
		if source == nil || issuer == nil || dispatcher == nil {
			return fmt.Errorf("action worker scope source, credential lease issuer, and dispatcher are required")
		}
		e.actionSupervisorSpecs = append(e.actionSupervisorSpecs, actionWorkerSupervisorSpec{
			config: config, source: source, leaseIssuer: issuer, dispatcher: dispatcher,
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
		preview, err := clawhub.NewPreviewManager(registryID, registry)
		if err != nil {
			return err
		}
		e.clawHub, e.clawHubPreview = manager, preview
		e.clawHubRegistry = registry
		return nil
	}
}

// WithClawHubRegistryPreview enables catalog inspection plus verified,
// credential-free compilation previews without configuring an installation
// workspace. Engine construction therefore never restores, installs, or
// activates Skills. Preview archive staging is bounded by the canonical
// compiler limits and removed before the preview call returns.
func WithClawHubRegistryPreview(registryID string, registry clawhub.Registry) Option {
	return func(e *Engine) error {
		manager, err := clawhub.NewPreviewManager(registryID, registry)
		if err != nil {
			return err
		}
		e.clawHubPreview, e.clawHubRegistry = manager, registry
		return nil
	}
}

// WithClawHubRegistryClient enables side-effect-free catalog discovery,
// version/file/security inspection, and registry verification without an
// installation workspace.
func WithClawHubRegistryClient(registry clawhub.Registry) Option {
	return func(e *Engine) error {
		if registry == nil {
			return errors.New("ClawHub registry is required")
		}
		e.clawHubRegistry = registry
		return nil
	}
}

// WithClawHubRegistrySkillsDirectory lets an embedding host keep lifecycle
// metadata in a governed workspace while adopting an existing managed Skills
// mount. It is the migration-safe variant of WithClawHubRegistry.
func WithClawHubRegistrySkillsDirectory(registryID string, registry clawhub.Registry, workspace, skillsDirectory string) Option {
	return func(e *Engine) error {
		manager, err := clawhub.NewInstallManagerWithSkillsDirectory(registryID, registry, workspace, skillsDirectory)
		if err != nil {
			return err
		}
		preview, err := clawhub.NewPreviewManager(registryID, registry)
		if err != nil {
			return err
		}
		e.clawHub, e.clawHubPreview, e.clawHubRegistry = manager, preview, registry
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
	e.actions = runtime.NewActionCoordinator(e.store, e.store, e.skills, e.actionPolicy, e.actionValidators...)
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
	if strings.TrimSpace(installed.SourceIdentity) == "" {
		return fmt.Errorf("installed skill source identity is required")
	}
	definitionCopy := *installed.Compilation.Definition
	if definitionCopy.Source == nil {
		return fmt.Errorf("installed skill source provenance is required")
	}
	sourceCopy := *definitionCopy.Source
	sourceCopy.Identity = strings.TrimSpace(installed.SourceIdentity)
	definitionCopy.Source = &sourceCopy
	installed.Compilation.Definition = &definitionCopy
	definition := &definitionCopy
	if err := e.validateClawHubCompilation(installed.Compilation); err != nil {
		return err
	}
	if e.skillSources != nil && e.clawHubSourceScope.Kind != "" {
		if _, _, err := e.skillSources.ImportOpenClaw(ctx, sourceartifact.ImportOpenClawRequest{
			Scope: e.clawHubSourceScope, Compilation: installed.Compilation,
			ReferenceID: "clawhub.installation:" + installed.SourceIdentity, ReferenceKind: "installation",
		}); err != nil {
			return fmt.Errorf("retain installed skill source artifact: %w", err)
		}
		if _, _, err := e.skillSources.ImportOpenClaw(ctx, sourceartifact.ImportOpenClawRequest{
			Scope: e.clawHubSourceScope, Compilation: installed.Compilation,
			ReferenceID: "skill.definition:" + definition.ID + "@" + definition.Version + ":" + installed.SourceIdentity, ReferenceKind: "definition",
		}); err != nil {
			return fmt.Errorf("retain registered skill definition source artifact: %w", err)
		}
	}
	existing, err := e.skills.GetDefinitionVariant(ctx, definition.ID, definition.Version, installed.SourceIdentity)
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
	return nil
}

func (e *Engine) rebuildActionWorkerPools() error {
	e.actionPools = make([]*runtime.ActionWorkerPool, 0, len(e.actionPoolSpecs))
	for _, spec := range e.actionPoolSpecs {
		var pool *runtime.ActionWorkerPool
		var err error
		if spec.leaseIssuer != nil {
			pool, err = runtime.NewActionCredentialLeaseWorkerPool(e.store, e.skills, spec.leaseIssuer, spec.dispatcher, e.logger, spec.config)
		} else {
			pool, err = runtime.NewActionWorkerPool(e.store, e.skills, spec.credentials, spec.dispatcher, e.logger, spec.config)
		}
		if err != nil {
			return err
		}
		pool.SetWorkerLimiter(e.workerLimiter)
		e.actionPools = append(e.actionPools, pool)
	}
	return nil
}

func (e *Engine) rebuildActionWorkerSupervisors() error {
	e.actionSupervisors = make([]*runtime.ActionWorkerSupervisor, 0, len(e.actionSupervisorSpecs))
	for _, spec := range e.actionSupervisorSpecs {
		var supervisor *runtime.ActionWorkerSupervisor
		var err error
		if spec.leaseIssuer != nil {
			supervisor, err = runtime.NewActionCredentialLeaseWorkerSupervisor(e.store, e.skills, spec.leaseIssuer, spec.dispatcher, spec.source, e.logger, spec.config)
		} else {
			supervisor, err = runtime.NewActionWorkerSupervisor(e.store, e.skills, spec.credentials, spec.dispatcher, spec.source, e.logger, spec.config)
		}
		if err != nil {
			return err
		}
		supervisor.SetWorkerLimiter(e.workerLimiter)
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
		pool.SetWorkerLimiter(e.workerLimiter)
		pool.SetActionCoordinator(e.actions)
		observer, observerErr := runtime.NewOutreachActionProposalObserver(e)
		if observerErr != nil {
			return observerErr
		}
		pool.SetActionProposalObserver(observer)
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
		supervisor.SetWorkerLimiter(e.workerLimiter)
		supervisor.SetActionCoordinator(e.actions)
		observer, observerErr := runtime.NewOutreachActionProposalObserver(e)
		if observerErr != nil {
			return observerErr
		}
		supervisor.SetActionProposalObserver(observer)
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

// DecodeObjectiveEventRules validates and decodes the portable Objective event
// subscription contract for host connectors. Transport configuration and
// credentials remain the responsibility of the embedding host.
func DecodeObjectiveEventRules(value map[string]interface{}) (*ObjectiveEventRules, error) {
	return runtime.DecodeObjectiveEventRules(value)
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

func (e *Engine) CreateInitiative(ctx context.Context, req runtime.CreateInitiativeRequest) (*runtime.Initiative, *runtime.ActivityEvent, error) {
	if e.initiatives == nil {
		return nil, nil, errors.New("initiative capability is unavailable")
	}
	return e.initiatives.Create(ctx, req)
}
func (e *Engine) GetInitiative(ctx context.Context, scope runtime.Scope, id string) (*runtime.Initiative, error) {
	if e.initiatives == nil {
		return nil, errors.New("initiative capability is unavailable")
	}
	return e.initiatives.Get(ctx, scope, id)
}
func (e *Engine) ListInitiatives(ctx context.Context, filter runtime.InitiativeFilter) ([]*runtime.Initiative, error) {
	if e.initiatives == nil {
		return nil, errors.New("initiative capability is unavailable")
	}
	return e.initiatives.List(ctx, filter)
}
func (e *Engine) UpdateInitiative(ctx context.Context, scope runtime.Scope, initiativeID string, req runtime.UpdateInitiativeRequest) (*runtime.Initiative, *runtime.ActivityEvent, error) {
	if e.initiatives == nil {
		return nil, nil, errors.New("initiative capability is unavailable")
	}
	return e.initiatives.Patch(ctx, scope, initiativeID, req)
}

func (e *Engine) IngestSourceObservation(ctx context.Context, req runtime.IngestSourceObservationRequest) (*runtime.SourceObservationIngestResult, error) {
	if e.sourceMonitors == nil {
		return nil, errors.New("source monitor capability is unavailable")
	}
	return e.sourceMonitors.Ingest(ctx, req)
}

func (e *Engine) AdvanceSourceMonitorCheckpoint(ctx context.Context, req runtime.AdvanceSourceMonitorCheckpointRequest) (*runtime.SourceMonitorCheckpointResult, error) {
	if e.sourceMonitors == nil {
		return nil, errors.New("source monitor capability is unavailable")
	}
	return e.sourceMonitors.AdvanceCheckpoint(ctx, req)
}

func (e *Engine) GetSourceMonitorCheckpoint(ctx context.Context, scope runtime.Scope, initiativeID, monitorID string) (*runtime.SourceMonitorCheckpoint, error) {
	if e.sourceMonitors == nil {
		return nil, errors.New("source monitor capability is unavailable")
	}
	return e.sourceMonitors.GetCheckpoint(ctx, scope, initiativeID, monitorID)
}

func (e *Engine) ListSourceObservations(ctx context.Context, filter runtime.SourceObservationFilter) ([]*runtime.SourceObservation, error) {
	if e.sourceMonitors == nil {
		return nil, errors.New("source monitor capability is unavailable")
	}
	return e.sourceMonitors.List(ctx, filter)
}

func (e *Engine) GetSourceObservation(ctx context.Context, scope runtime.Scope, id string) (*runtime.SourceObservation, error) {
	store, ok := e.store.(runtime.SourceMonitorStore)
	if !ok {
		return nil, errors.New("source observation capability is unavailable")
	}
	return store.GetSourceObservation(ctx, scope, id)
}

// GetEventSourceCheckpoint returns durable host-connector progress without
// projecting opaque cursors into the model-visible activity or prompt layers.
func (e *Engine) GetEventSourceCheckpoint(ctx context.Context, scope runtime.Scope, source, subscriptionID string) (*runtime.EventSourceCheckpoint, error) {
	if e.eventSources == nil {
		return nil, errors.New("event source checkpoint capability is unavailable")
	}
	return e.eventSources.Get(ctx, scope, source, subscriptionID)
}

// AdvanceEventSourceCheckpoint atomically appends a bounded replay window and
// advances the connector cursor using optimistic concurrency.
func (e *Engine) AdvanceEventSourceCheckpoint(ctx context.Context, req runtime.AdvanceEventSourceCheckpointRequest) (*runtime.EventSourceCheckpoint, error) {
	if e.eventSources == nil {
		return nil, errors.New("event source checkpoint capability is unavailable")
	}
	return e.eventSources.Advance(ctx, req)
}

func (e *Engine) CreateOutreachThread(ctx context.Context, req runtime.CreateOutreachThreadRequest) (*runtime.OutreachThread, *runtime.ActivityEvent, error) {
	if e.outreach == nil {
		return nil, nil, errors.New("outreach capability is unavailable")
	}
	return e.outreach.Create(ctx, req)
}

func (e *Engine) GetOutreachThread(ctx context.Context, scope runtime.Scope, id string) (*runtime.OutreachThread, error) {
	if e.outreach == nil {
		return nil, errors.New("outreach capability is unavailable")
	}
	return e.outreach.Get(ctx, scope, id)
}

func (e *Engine) ListOutreachThreads(ctx context.Context, filter runtime.OutreachThreadFilter) ([]*runtime.OutreachThread, error) {
	if e.outreach == nil {
		return nil, errors.New("outreach capability is unavailable")
	}
	return e.outreach.List(ctx, filter)
}

func (e *Engine) LinkOutreachAction(ctx context.Context, scope runtime.Scope, id string, req runtime.LinkOutreachActionRequest) (*runtime.OutreachThread, *runtime.ActivityEvent, error) {
	if e.outreach == nil {
		return nil, nil, errors.New("outreach capability is unavailable")
	}
	return e.outreach.LinkAction(ctx, scope, id, req)
}

func (e *Engine) RecordOutreachDelivery(ctx context.Context, scope runtime.Scope, id string, req runtime.RecordOutreachDeliveryRequest) (*runtime.OutreachThread, *runtime.ActivityEvent, error) {
	if e.outreach == nil {
		return nil, nil, errors.New("outreach capability is unavailable")
	}
	return e.outreach.RecordDelivery(ctx, scope, id, req)
}

func (e *Engine) ResolveOutreachMessage(ctx context.Context, scope runtime.Scope, id string, req runtime.ResolveOutreachMessageRequest) (*runtime.OutreachThread, *runtime.ActivityEvent, error) {
	if e.outreach == nil {
		return nil, nil, errors.New("outreach capability is unavailable")
	}
	return e.outreach.ResolveMessage(ctx, scope, id, req)
}

func (e *Engine) ReconcileOutreachAction(ctx context.Context, scope runtime.Scope, id string, req runtime.ReconcileOutreachActionRequest) (*runtime.ReconcileOutreachActionResult, error) {
	if e.outreach == nil {
		return nil, errors.New("outreach capability is unavailable")
	}
	return e.outreach.ReconcileAction(ctx, scope, id, req)
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
	if err := e.ValidateAgentRunEntrypoint(ctx, req.Scope, req.AssignedAgentID, req.Entrypoint); err != nil {
		return nil, err
	}
	return runtime.NewRunCommandService(e.store).CreateAgentRun(ctx, req)
}

// ValidateAgentRunEntrypoint proves that an advertised runbook entrypoint is
// executable by the Agent's currently active immutable definition. Callers can
// use it for capability discovery; CreateAgentRunCommand enforces it again at
// the command boundary so stale UI or model-tool state fails before a Run is
// persisted.
func (e *Engine) ValidateAgentRunEntrypoint(ctx context.Context, scope runtime.Scope, agentID, entrypoint string) error {
	entrypoint = strings.TrimSpace(entrypoint)
	if entrypoint == "" {
		return nil
	}
	if e == nil || e.agents == nil {
		return fmt.Errorf("validate Agent Run entrypoint: Agent registry is not configured")
	}
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return fmt.Errorf("validate Agent Run entrypoint %q: assigned Agent is required", entrypoint)
	}
	deployment, err := e.GetAgentDeployment(ctx, skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, agentID)
	if err != nil || deployment == nil {
		return fmt.Errorf("validate Agent Run entrypoint %q for Agent %s: deployment is unavailable", entrypoint, agentID)
	}
	definition, err := e.GetAgentDefinition(ctx, deployment.DefinitionID, deployment.ActiveVersion)
	if err != nil || definition == nil || definition.Runbook == nil {
		return fmt.Errorf("validate Agent Run entrypoint %q for Agent %s: active runbook is unavailable", entrypoint, agentID)
	}
	if _, ok := definition.Runbook.Entrypoints[entrypoint]; !ok {
		return fmt.Errorf("validate Agent Run entrypoint %q for Agent %s: entrypoint is not defined by the active runbook", entrypoint, agentID)
	}
	return nil
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

func (e *Engine) SummarizeAgentRuns(ctx context.Context, scope runtime.Scope, owners []runtime.ObjectiveOwner) ([]runtime.AgentRunOwnerSummary, error) {
	return e.portfolio.SummarizeAgentRuns(ctx, scope, owners)
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

func (e *Engine) GetVisibleChannelMessage(ctx context.Context, scope runtime.Scope, conversationID, messageID string, viewer runtime.ConversationViewer) (*runtime.ChannelMessage, error) {
	if e.conversations == nil {
		return nil, fmt.Errorf("conversation store is not configured")
	}
	return e.conversations.GetVisibleChannelMessage(ctx, scope, conversationID, messageID, viewer)
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

func (e *Engine) ListConversationCursors(ctx context.Context, scope runtime.Scope, conversationID string) ([]*runtime.ConversationCursor, error) {
	if e == nil || e.conversations == nil {
		return nil, errors.New("conversation service is not configured")
	}
	return e.conversations.ListCursors(ctx, scope, conversationID)
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

// RouteEvent normalizes every event source onto canonical Objective and Run
// semantics. Embedding hosts own transport watches and authorization; the
// kernel owns matching, exact idempotency, durable work, and audit.
func (e *Engine) RouteEvent(ctx context.Context, event runtime.EventEnvelope) (*runtime.EventRouteResult, error) {
	if e == nil || e.store == nil {
		return nil, errors.New("objective event routing is unavailable")
	}
	return runtime.NewObjectiveEventRouter(e.store).Route(ctx, event)
}

func (e *Engine) RegisterSkill(ctx context.Context, definition *skill.Definition) error {
	return e.skills.Register(ctx, definition)
}

func (e *Engine) ImportOpenClawSkillSource(ctx context.Context, request sourceartifact.ImportOpenClawRequest) (*sourceartifact.Artifact, bool, error) {
	if e == nil || e.skillSources == nil {
		return nil, false, errors.New("skill source artifact store is unavailable")
	}
	return e.skillSources.ImportOpenClaw(ctx, request)
}

func (e *Engine) ExportOpenClawSkillSource(ctx context.Context, scope skill.ScopeReference, digest string) (skillopenclaw.Bundle, error) {
	if e == nil || e.skillSources == nil {
		return skillopenclaw.Bundle{}, errors.New("skill source artifact store is unavailable")
	}
	return e.skillSources.ExportOpenClaw(ctx, scope, digest)
}

func (e *Engine) ExportOpenClawSkillSourceForReference(ctx context.Context, scope skill.ScopeReference, digest, referenceID string) (skillopenclaw.Bundle, error) {
	if e == nil || e.skillSources == nil {
		return skillopenclaw.Bundle{}, errors.New("skill source artifact store is unavailable")
	}
	return e.skillSources.ExportOpenClawForReference(ctx, scope, digest, referenceID)
}

func (e *Engine) GetSkillSourceArtifact(ctx context.Context, scope skill.ScopeReference, digest string) (*sourceartifact.Artifact, error) {
	if e == nil || e.skillSources == nil {
		return nil, errors.New("skill source artifact store is unavailable")
	}
	return e.skillSources.Get(ctx, scope, digest)
}

func (e *Engine) SkillSourceContentProvider() (skill.ResourceContentProvider, error) {
	if e == nil || e.skillSources == nil {
		return nil, errors.New("skill source artifact store is unavailable")
	}
	return e.skillSources, nil
}

func (e *Engine) GarbageCollectSkillSourceArtifacts(ctx context.Context, now time.Time, unreferencedGrace time.Duration, limit int) (*sourceartifact.GarbageCollectionReport, error) {
	if e == nil || e.skillSources == nil {
		return nil, errors.New("skill source artifact store is unavailable")
	}
	return e.skillSources.GarbageCollect(ctx, now, unreferencedGrace, limit)
}

func (e *Engine) GetSkillDefinition(ctx context.Context, skillID, version string) (*skill.Definition, error) {
	return e.skills.GetDefinition(ctx, skillID, version)
}

func (e *Engine) GetSkillDefinitionVariant(ctx context.Context, skillID, version, sourceIdentity string) (*skill.Definition, error) {
	return e.skills.GetDefinitionVariant(ctx, skillID, version, sourceIdentity)
}

func (e *Engine) BindSkill(ctx context.Context, binding *skill.Binding) error {
	return e.skills.Bind(ctx, binding)
}

func (e *Engine) ListSkillBindings(ctx context.Context, scope skill.ScopeReference, deploymentID string) ([]*skill.Binding, error) {
	return e.skills.ListBindings(ctx, scope, deploymentID)
}

func (e *Engine) GetSkillBinding(ctx context.Context, scope skill.ScopeReference, deploymentID, bindingID string) (*skill.Binding, error) {
	return e.skills.GetBinding(ctx, scope, deploymentID, bindingID)
}

func (e *Engine) UpsertSkillBinding(ctx context.Context, request skill.UpsertBindingRequest) (*skill.Binding, error) {
	return e.skills.UpsertBinding(ctx, request)
}

func (e *Engine) DisableSkillBinding(ctx context.Context, request skill.DisableBindingRequest) (*skill.Binding, error) {
	return e.skills.DisableBinding(ctx, request)
}

// ListTeamSkillBindings exposes the first-class Team-owned binding portfolio.
// It verifies Team identity in the exact scope before reading the shared
// canonical Skill contract.
func (e *Engine) ListTeamSkillBindings(ctx context.Context, scope skill.ScopeReference, teamDeploymentID string) ([]*skill.Binding, error) {
	if _, err := e.teams.GetDeployment(ctx, scope, teamDeploymentID); err != nil {
		return nil, err
	}
	return e.skills.ListBindings(ctx, scope, teamDeploymentID)
}

func (e *Engine) UpsertTeamSkillBinding(ctx context.Context, teamDeploymentID string, request skill.UpsertBindingRequest) (*skill.Binding, error) {
	if request.Binding == nil || request.Binding.DeploymentID != teamDeploymentID {
		return nil, errors.New("Team Skill binding must identify the owning Team deployment")
	}
	if _, err := e.teams.GetDeployment(ctx, request.Binding.Scope, teamDeploymentID); err != nil {
		return nil, err
	}
	return e.skills.UpsertBinding(ctx, request)
}

func (e *Engine) DisableTeamSkillBinding(ctx context.Context, teamDeploymentID string, request skill.DisableBindingRequest) (*skill.Binding, error) {
	if request.DeploymentID != teamDeploymentID {
		return nil, errors.New("Team Skill binding must identify the owning Team deployment")
	}
	if _, err := e.teams.GetDeployment(ctx, request.Scope, teamDeploymentID); err != nil {
		return nil, err
	}
	return e.skills.DisableBinding(ctx, request)
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

func (e *Engine) ResolveExactSkillPrompt(ctx context.Context, scope skill.ScopeReference, deploymentID, skillID, version string, binding skill.BindingReference) (*skill.PromptModule, error) {
	return e.skills.ResolveExactPrompt(ctx, scope, deploymentID, skillID, version, binding)
}

func (e *Engine) ResolveSkillAction(ctx context.Context, scope skill.ScopeReference, deploymentID, skillID, version, action string) (*skill.BoundAction, error) {
	return e.skills.Resolve(ctx, scope, deploymentID, skillID, version, action)
}

func (e *Engine) ResolveExactSkillAction(ctx context.Context, scope skill.ScopeReference, deploymentID, skillID, version, action string, binding skill.BindingReference) (*skill.BoundAction, error) {
	return e.skills.Resolve(ctx, scope, deploymentID, skillID, version, action, binding)
}

func (e *Engine) ValidateSkillActionInput(ctx context.Context, bound *skill.BoundAction, input map[string]interface{}) error {
	return e.skills.ValidateInput(ctx, bound, input)
}

func (e *Engine) ActivateSkills(ctx context.Context, scope skill.ScopeReference, deploymentID string, host skill.HostCapabilityState) (*skill.ActivationSnapshot, error) {
	return e.skills.Activate(ctx, scope, deploymentID, host)
}

// PreviewSkillBindingActivation evaluates one exact proposed binding against
// the execution host without persisting or invoking host lifecycle adapters.
func (e *Engine) PreviewSkillBindingActivation(ctx context.Context, binding *skill.Binding, host skill.HostCapabilityState) (*skill.BindingActivationPreview, error) {
	return e.skills.PreviewBindingActivation(ctx, binding, host)
}

// PreviewSkillBindingActivation evaluates an already-resolved immutable Skill
// definition without requiring it to be installed in an Engine catalog.
func PreviewSkillBindingActivation(definition *skill.Definition, binding *skill.Binding, host skill.HostCapabilityState) (*skill.BindingActivationPreview, error) {
	return skill.PreviewBindingActivation(definition, binding, host)
}

// ValidateSkillResourceStageRequest exposes the portable host-boundary
// validation without requiring embedding runtimes to import an internal Skill
// package alongside the stable OpenSeal facade.
func ValidateSkillResourceStageRequest(request skill.ResourceStageRequest) error {
	return skill.ValidateResourceStageRequest(request)
}

func SkillRuntimePreparationID(request skill.RuntimePreparationRequest) (string, error) {
	return skill.RuntimePreparationID(request)
}

func ValidateSkillRuntimePreparationRequest(request skill.RuntimePreparationRequest) error {
	return skill.ValidateRuntimePreparationRequest(request)
}

func ValidateSkillPreparedRuntimeReference(runtime *skill.PreparedRuntime) error {
	return skill.ValidatePreparedRuntimeReference(runtime)
}

// EvaluateAuthorityProgression exposes the portable pure evaluator for hosts
// that already resolved an exact deployment revision and immutable base.
func EvaluateAuthorityProgression(request progression.EvaluationRequest) (*progression.Recommendation, error) {
	return progression.Evaluate(request)
}

func (e *Engine) RegisterAgentDefinition(ctx context.Context, definition *kernelagent.AgentDefinition) (*kernelagent.AgentDefinition, error) {
	return e.agents.RegisterDefinition(ctx, definition)
}

func (e *Engine) RecordAgentDefinitionCompilation(ctx context.Context, compilation *kernelagent.DefinitionCompilation) (*kernelagent.DefinitionCompilation, error) {
	return e.agents.RecordCompilation(ctx, compilation)
}

func (e *Engine) GetAgentDefinitionCompilation(ctx context.Context, scope skill.ScopeReference, id string) (*kernelagent.DefinitionCompilation, error) {
	return e.agents.GetCompilation(ctx, scope, id)
}

func (e *Engine) ListAgentDefinitionCompilations(ctx context.Context, scope skill.ScopeReference, deploymentID string) ([]*kernelagent.DefinitionCompilation, error) {
	return e.agents.ListCompilations(ctx, scope, deploymentID)
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

func (e *Engine) ListAgentDeployments(ctx context.Context, scope skill.ScopeReference) ([]*kernelagent.AgentDeployment, error) {
	return e.agents.ListDeployments(ctx, scope)
}

func (e *Engine) UpdateAgentDeployment(ctx context.Context, deployment *kernelagent.AgentDeployment, expectedRevision int64, actorType, actorID, reason string) (*kernelagent.AgentDeployment, *kernelagent.DefinitionActivation, error) {
	return e.agents.UpdateDeployment(ctx, deployment, expectedRevision, actorType, actorID, reason)
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

func (e *Engine) ListAgentDefinitionAmendments(ctx context.Context, scope skill.ScopeReference, deploymentID string) ([]*kernelagent.DefinitionAmendment, error) {
	return e.agents.ListAmendments(ctx, scope, deploymentID)
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

// RecommendAgentAuthority deterministically evaluates retained outcomes
// against the currently active immutable Agent definition. It creates no
// authority; callers must separately submit the recommendation as a governed
// amendment proposal.
func (e *Engine) RecommendAgentAuthority(ctx context.Context, scope skill.ScopeReference, deploymentID string, expectedRevision int64, policy progression.EvaluationPolicy, outcomes []progression.CriterionOutcome) (*progression.Recommendation, error) {
	if e == nil || e.progression == nil {
		return nil, errors.New("authority progression is not configured")
	}
	return e.progression.RecommendAgent(ctx, scope, deploymentID, expectedRevision, policy, outcomes)
}

func (e *Engine) ProposeAgentAuthorityProgression(ctx context.Context, request progression.ProposeAgentRequest) (*kernelagent.DefinitionAmendment, error) {
	if e == nil || e.progression == nil {
		return nil, errors.New("authority progression is not configured")
	}
	return e.progression.ProposeAgent(ctx, request)
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

// WorkforceAuthoringAvailable reports whether this Engine has a configured
// generation boundary. Hosts should use it for exact capability discovery.
func (e *Engine) WorkforceAuthoringAvailable() bool {
	return e != nil && e.authoring != nil
}

// CompileWorkforcePrompt produces a verified, reviewable candidate without
// registering definitions, creating deployments, or activating state.
func (e *Engine) CompileWorkforcePrompt(ctx context.Context, request authoring.GenerateRequest) (*authoring.CompileResult, error) {
	if e == nil || e.authoring == nil {
		return nil, errors.New("workforce authoring is not configured")
	}
	return e.authoring.Compile(ctx, request)
}

func (e *Engine) WorkforceChangeSetsAvailable() bool {
	return e != nil && e.authoringChanges != nil
}

func (e *Engine) WorkforceChangeSetApplyAvailable() bool {
	return e != nil && e.authoringChanges != nil && e.authoringChanges.ApplyAvailable()
}

func (e *Engine) CreateWorkforceChangeSet(ctx context.Context, request authoring.CreateChangeSetRequest) (*authoring.ChangeSet, bool, error) {
	if e == nil || e.authoringChanges == nil {
		return nil, false, errors.New("workforce change sets are not configured")
	}
	return e.authoringChanges.Create(ctx, request)
}

// PrepareWorkforceChangeSet persists a credential-free generation request
// without waiting for probabilistic compilation.
func (e *Engine) PrepareWorkforceChangeSet(ctx context.Context, request authoring.CreateChangeSetRequest) (*authoring.ChangeSet, bool, error) {
	if e == nil || e.authoringChanges == nil {
		return nil, false, errors.New("workforce change sets are not configured")
	}
	if e.authoringRuns != nil {
		changeSet, _, replay, err := e.authoringRuns.Prepare(ctx, request)
		return changeSet, replay, err
	}
	return e.authoringChanges.Prepare(ctx, request)
}

// GeneratePreparedWorkforceChangeSet completes one persisted generation intent.
// Durable hosts invoke this from a leased canonical Run worker.
func (e *Engine) GeneratePreparedWorkforceChangeSet(ctx context.Context, scope skill.ScopeReference, id string, expectedRevision int64) (*authoring.ChangeSet, error) {
	if e == nil || e.authoringChanges == nil {
		return nil, errors.New("workforce change sets are not configured")
	}
	return e.authoringChanges.GeneratePrepared(ctx, scope, id, expectedRevision)
}

func (e *Engine) RetryWorkforceChangeSetGeneration(ctx context.Context, request authoring.RetryChangeSetGenerationRequest) (*authoring.ChangeSet, bool, error) {
	if e == nil || e.authoringRuns == nil {
		return nil, false, errors.New("durable workforce authoring runs are not configured")
	}
	changeSet, _, replayed, err := e.authoringRuns.Retry(ctx, request)
	return changeSet, replayed, err
}

func (e *Engine) GetWorkforceChangeSet(ctx context.Context, scope skill.ScopeReference, id string) (*authoring.ChangeSet, error) {
	if e == nil || e.authoringChanges == nil {
		return nil, errors.New("workforce change sets are not configured")
	}
	return e.authoringChanges.Get(ctx, scope, id)
}

// ListPendingWorkforceChangeSetEvaluations exposes the durable recovery index
// for trusted host policy workers. It is not a user-facing catalog operation.
func (e *Engine) ListPendingWorkforceChangeSetEvaluations(ctx context.Context, scope skill.ScopeReference, limit int) ([]*authoring.ChangeSet, error) {
	if e == nil || e.authoringChanges == nil {
		return nil, errors.New("workforce change sets are unavailable")
	}
	return e.authoringChanges.ListPendingEvaluations(ctx, scope, limit)
}

func (e *Engine) SubmitWorkforceChangeSetEvaluation(ctx context.Context, request authoring.SubmitChangeSetEvaluationRequest) (*authoring.ChangeSet, bool, error) {
	if e == nil || e.authoringChanges == nil {
		return nil, false, errors.New("workforce change sets are not configured")
	}
	return e.authoringChanges.SubmitEvaluation(ctx, request)
}

func (e *Engine) ResolveWorkforceChangeSetApproval(ctx context.Context, request authoring.ResolveChangeSetApprovalRequest) (*authoring.ChangeSet, bool, error) {
	if e == nil || e.authoringChanges == nil {
		return nil, false, errors.New("workforce change sets are not configured")
	}
	return e.authoringChanges.ResolveApproval(ctx, request)
}

func (e *Engine) UpdateWorkforceChangeSetPlacement(ctx context.Context, request authoring.UpdateChangeSetPlacementRequest) (*authoring.ChangeSet, bool, error) {
	if e == nil || e.authoringChanges == nil {
		return nil, false, errors.New("workforce change sets are not configured")
	}
	return e.authoringChanges.UpdatePlacement(ctx, request)
}

func (e *Engine) ApplyWorkforceChangeSet(ctx context.Context, request authoring.ApplyChangeSetRequest) (*authoring.ChangeSet, bool, error) {
	if e == nil || e.authoringChanges == nil {
		return nil, false, errors.New("workforce change sets are not configured")
	}
	return e.authoringChanges.Apply(ctx, request)
}

func (e *Engine) CreateTeamDeployment(ctx context.Context, deployment *kernelteam.Deployment, actorType, actorID, reason string) (*kernelteam.Deployment, *workforce.DefinitionActivation, error) {
	return e.teams.CreateDeployment(ctx, deployment, actorType, actorID, reason)
}

func (e *Engine) GetTeamDeployment(ctx context.Context, scope skill.ScopeReference, deploymentID string) (*kernelteam.Deployment, error) {
	return e.teams.GetDeployment(ctx, scope, deploymentID)
}

func (e *Engine) ListTeamDeployments(ctx context.Context, scope skill.ScopeReference) ([]*kernelteam.Deployment, error) {
	return e.teams.ListDeployments(ctx, scope)
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

func (e *Engine) ListTeamDefinitionAmendments(ctx context.Context, scope skill.ScopeReference, deploymentID string) ([]*kernelteam.DefinitionAmendment, error) {
	return e.teams.ListAmendments(ctx, scope, deploymentID)
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

// RecommendTeamAuthority is the Team peer of RecommendAgentAuthority and uses
// the same evidence, policy, CAS, and deterministic recommendation contract.
func (e *Engine) RecommendTeamAuthority(ctx context.Context, scope skill.ScopeReference, deploymentID string, expectedRevision int64, policy progression.EvaluationPolicy, outcomes []progression.CriterionOutcome) (*progression.Recommendation, error) {
	if e == nil || e.progression == nil {
		return nil, errors.New("authority progression is not configured")
	}
	return e.progression.RecommendTeam(ctx, scope, deploymentID, expectedRevision, policy, outcomes)
}

func (e *Engine) ProposeTeamAuthorityProgression(ctx context.Context, request progression.ProposeTeamRequest) (*kernelteam.DefinitionAmendment, error) {
	if e == nil || e.progression == nil {
		return nil, errors.New("authority progression is not configured")
	}
	return e.progression.ProposeTeam(ctx, request)
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

func (e *Engine) ListClawHubSkillVersions(ctx context.Context, reference clawhub.SkillReference, limit int, cursor string) (*clawhub.VersionPage, error) {
	if e == nil || e.clawHubRegistry == nil {
		return nil, fmt.Errorf("ClawHub registry is not configured")
	}
	return e.clawHubRegistry.ListVersions(ctx, reference, limit, cursor)
}

func (e *Engine) GetClawHubSkillVersion(ctx context.Context, reference clawhub.SkillReference, version string) (*clawhub.VersionDetail, error) {
	if e == nil || e.clawHubRegistry == nil {
		return nil, fmt.Errorf("ClawHub registry is not configured")
	}
	return e.clawHubRegistry.GetVersion(ctx, reference, version)
}

func (e *Engine) GetClawHubSkillFile(ctx context.Context, reference clawhub.SkillReference, version, tag, path string) ([]byte, error) {
	if e == nil || e.clawHubRegistry == nil {
		return nil, fmt.Errorf("ClawHub registry is not configured")
	}
	return e.clawHubRegistry.GetFile(ctx, reference, path, version, tag)
}

func (e *Engine) VerifyClawHubSkill(ctx context.Context, reference clawhub.SkillReference, version, tag string) (*clawhub.Verification, error) {
	if e == nil || e.clawHubRegistry == nil {
		return nil, fmt.Errorf("ClawHub registry is not configured")
	}
	return e.clawHubRegistry.VerifySkill(ctx, reference, version, tag)
}

// PreviewClawHubSkill fetches and verifies one immutable registry artifact and
// compiles its secret-free capability projection without installing,
// activating, or executing it. Pass the returned receipt to installation to
// reject registry or compiler drift between review and install.
func (e *Engine) PreviewClawHubSkill(ctx context.Context, request clawhub.PreviewRequest) (*clawhub.CompilationPreview, error) {
	if e == nil || e.clawHubPreview == nil {
		return nil, fmt.Errorf("ClawHub compilation preview is not configured")
	}
	return e.clawHubPreview.PreviewValidated(ctx, request, e.validateClawHubCompilation)
}

func (e *Engine) InstallClawHubSkill(ctx context.Context, request clawhub.InstallRequest) (*clawhub.InstalledSkill, error) {
	if e.clawHub == nil {
		return nil, fmt.Errorf("ClawHub registry is not configured")
	}
	installed, err := e.clawHub.InstallValidated(ctx, request, e.validateClawHubCompilation)
	if err != nil {
		return nil, err
	}
	if installed.Changed {
		if err := e.activateInstalledSkill(ctx, installed); err != nil {
			return nil, err
		}
	}
	return installed, nil
}

// InstallClawHubSkillLifecycle installs and activates a verified canonical
// Skill and returns the audit-safe receipt separately from host-only compiled
// material. Hosts persist the receipt and may use InstalledSkill to project
// the same Definition into their authorized catalog without re-parsing it.
func (e *Engine) InstallClawHubSkillLifecycle(ctx context.Context, request clawhub.InstallRequest) (*clawhub.InstalledSkill, *clawhub.LifecycleResult, error) {
	installed, err := e.InstallClawHubSkill(ctx, request)
	if err != nil {
		return nil, nil, err
	}
	result := &clawhub.LifecycleResult{
		APIVersion:     clawhub.LifecycleAPIVersion,
		Operation:      clawhub.LifecycleInstall,
		SourceIdentity: installed.SourceIdentity,
		Reference:      installed.Reference,
		Version:        installed.Version,
		Outcome:        clawhub.LifecycleOutcomeInstalled,
		Changed:        installed.Changed,
	}
	if !installed.Changed {
		result.Outcome = clawhub.LifecycleOutcomeUnchanged
	}
	return installed, result, nil
}

func (e *Engine) UpdateClawHubSkill(ctx context.Context, slug string) (*clawhub.InstalledSkill, error) {
	if e.clawHub == nil {
		return nil, fmt.Errorf("ClawHub registry is not configured")
	}
	installed, err := e.clawHub.UpdateValidated(ctx, slug, e.validateClawHubCompilation)
	if err != nil {
		return nil, err
	}
	if installed.Changed {
		if err := e.activateInstalledSkill(ctx, installed); err != nil {
			return nil, err
		}
	}
	return installed, nil
}

// UpdateClawHubSkillLifecycle updates one unpinned installation and reports
// both versions without exposing compilation or workspace internals.
func (e *Engine) UpdateClawHubSkillLifecycle(ctx context.Context, reference string) (*clawhub.InstalledSkill, *clawhub.LifecycleResult, error) {
	if e == nil || e.clawHub == nil {
		return nil, nil, fmt.Errorf("ClawHub registry is not configured")
	}
	lock, err := e.clawHub.List()
	if err != nil {
		return nil, nil, err
	}
	identity, entry, err := resolveClawHubLifecycleEntry(lock, reference)
	if err != nil {
		return nil, nil, err
	}
	installed, err := e.UpdateClawHubSkill(ctx, reference)
	if err != nil {
		return nil, nil, err
	}
	result := &clawhub.LifecycleResult{APIVersion: clawhub.LifecycleAPIVersion, Operation: clawhub.LifecycleUpdate, SourceIdentity: identity, Reference: installed.Reference, Version: installed.Version, Outcome: clawhub.LifecycleOutcomeUpdated, Changed: installed.Changed}
	if entry.Version != nil {
		result.PreviousVersion = *entry.Version
	}
	if !installed.Changed {
		result.Outcome = clawhub.LifecycleOutcomeUnchanged
	}
	return installed, result, nil
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

// PinClawHubSkillLifecycle applies the atomic lockfile mutation and returns the
// canonical secret-free receipt hosts persist in their audit/control plane.
func (e *Engine) PinClawHubSkillLifecycle(reference, reason string) (*clawhub.LifecycleResult, error) {
	if e == nil || e.clawHub == nil {
		return nil, fmt.Errorf("ClawHub registry is not configured")
	}
	lock, err := e.clawHub.List()
	if err != nil {
		return nil, err
	}
	identity, entry, err := resolveClawHubLifecycleEntry(lock, reference)
	if err != nil {
		return nil, err
	}
	if err := e.clawHub.Pin(reference, reason); err != nil {
		return nil, err
	}
	result := clawHubLifecycleStateResult(clawhub.LifecyclePin, clawhub.LifecycleOutcomePinned, identity, entry)
	result.Changed = !entry.Pinned || entry.PinReason != reason
	if !result.Changed {
		result.Outcome = clawhub.LifecycleOutcomeUnchanged
	}
	result.Reason = reason
	return result, nil
}

func (e *Engine) UnpinClawHubSkill(slug string) error {
	if e.clawHub == nil {
		return fmt.Errorf("ClawHub registry is not configured")
	}
	return e.clawHub.Unpin(slug)
}

// UnpinClawHubSkillLifecycle returns a canonical receipt even for an
// idempotent replay, allowing hosts to expose truthful mutation outcomes.
func (e *Engine) UnpinClawHubSkillLifecycle(reference string) (*clawhub.LifecycleResult, error) {
	if e == nil || e.clawHub == nil {
		return nil, fmt.Errorf("ClawHub registry is not configured")
	}
	lock, err := e.clawHub.List()
	if err != nil {
		return nil, err
	}
	identity, entry, err := resolveClawHubLifecycleEntry(lock, reference)
	if err != nil {
		return nil, err
	}
	if err := e.clawHub.Unpin(reference); err != nil {
		return nil, err
	}
	result := clawHubLifecycleStateResult(clawhub.LifecycleUnpin, clawhub.LifecycleOutcomeUnpinned, identity, entry)
	result.Changed = entry.Pinned
	if !result.Changed {
		result.Outcome = clawhub.LifecycleOutcomeUnchanged
	}
	return result, nil
}

func clawHubLifecycleStateResult(operation clawhub.LifecycleOperation, outcome clawhub.LifecycleOutcome, identity string, entry clawhub.LockEntry) *clawhub.LifecycleResult {
	result := &clawhub.LifecycleResult{APIVersion: clawhub.LifecycleAPIVersion, Operation: operation, SourceIdentity: identity, Reference: clawhub.SkillReference{Owner: entry.OwnerHandle, Slug: entry.Slug}, Outcome: outcome}
	if entry.Version != nil {
		result.Version = *entry.Version
	}
	return result
}

func (e *Engine) ClawHubLifecycleCapabilities() clawhub.LifecycleCapability {
	capability := clawhub.CanonicalLifecycleCapability()
	if e == nil || e.clawHubRegistry == nil {
		capability.Operations = nil
		return capability
	}
	if e.clawHub == nil {
		capability.Operations = []clawhub.LifecycleOperation{
			clawhub.LifecycleInspectCatalog, clawhub.LifecycleInspectVersions, clawhub.LifecycleInspectFiles,
			clawhub.LifecycleInspectSecurity, clawhub.LifecycleVerify,
		}
	}
	return capability
}

func (e *Engine) ListInstalledClawHubSkillStates() ([]clawhub.InstalledState, error) {
	if e == nil || e.clawHub == nil {
		return nil, fmt.Errorf("ClawHub registry is not configured")
	}
	return e.clawHub.ListInstalledStates()
}

// UpdateAllClawHubSkills deliberately reuses UpdateClawHubSkill so every
// candidate passes the Engine's compiler validation and activation path. A
// batch result is complete and deterministic even when individual skills are
// pinned, modified, unavailable, or invalid.
func (e *Engine) UpdateAllClawHubSkills(ctx context.Context) (*clawhub.LifecycleBatchResult, error) {
	if e == nil || e.clawHub == nil {
		return nil, fmt.Errorf("ClawHub registry is not configured")
	}
	lock, err := e.clawHub.List()
	if err != nil {
		return nil, err
	}
	result := &clawhub.LifecycleBatchResult{
		APIVersion: clawhub.LifecycleAPIVersion,
		Operation:  clawhub.LifecycleUpdateAll,
		Results:    make([]clawhub.LifecycleResult, 0, len(lock.Skills)),
	}
	for identity, entry := range lock.Skills {
		item := clawhub.LifecycleResult{
			APIVersion: clawhub.LifecycleAPIVersion, Operation: clawhub.LifecycleUpdate,
			SourceIdentity: identity,
			Reference:      clawhub.SkillReference{Owner: entry.OwnerHandle, Slug: entry.Slug},
		}
		if entry.Version != nil {
			item.PreviousVersion = *entry.Version
		}
		if entry.Pinned {
			item.Version, item.Outcome, item.Reason = item.PreviousVersion, clawhub.LifecycleOutcomeSkipped, "pinned"
			result.Results = append(result.Results, item)
			continue
		}
		updateReference := entry.Slug
		if entry.OwnerHandle != "" {
			updateReference = entry.OwnerHandle + "/" + entry.Slug
		}
		installed, updateErr := e.UpdateClawHubSkill(ctx, updateReference)
		if updateErr != nil {
			item.Version, item.Outcome, item.ErrorCode = item.PreviousVersion, clawhub.LifecycleOutcomeError, ClassifyClawHubLifecycleError(updateErr)
			result.Results = append(result.Results, item)
			continue
		}
		item.Version, item.Changed = installed.Version, installed.Changed
		if installed.Changed {
			item.Outcome = clawhub.LifecycleOutcomeUpdated
		} else {
			item.Outcome = clawhub.LifecycleOutcomeUnchanged
		}
		result.Results = append(result.Results, item)
	}
	result.Sort()
	return result, nil
}

// ClassifyClawHubLifecycleError maps internal/registry failures onto the
// stable secret-free lifecycle contract used by batch and host audit results.
func ClassifyClawHubLifecycleError(err error) clawhub.LifecycleErrorCode {
	switch {
	case errors.Is(err, clawhub.ErrSkillPinned):
		return clawhub.LifecycleErrorPinned
	case errors.Is(err, clawhub.ErrSkillModified):
		return clawhub.LifecycleErrorModified
	case errors.Is(err, clawhub.ErrVerificationFailed):
		return clawhub.LifecycleErrorVerificationFailed
	case errors.Is(err, clawhub.ErrNotFound):
		return clawhub.LifecycleErrorNotFound
	case errors.Is(err, clawhub.ErrAmbiguousSkill):
		return clawhub.LifecycleErrorAmbiguous
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return clawhub.LifecycleErrorCanceled
	default:
		return clawhub.LifecycleErrorUnavailable
	}
}

func (e *Engine) UninstallClawHubSkill(reference string, force bool) (*clawhub.LifecycleResult, error) {
	if e == nil || e.clawHub == nil {
		return nil, fmt.Errorf("ClawHub registry is not configured")
	}
	lock, err := e.clawHub.List()
	if err != nil {
		return nil, err
	}
	identity, entry, err := resolveClawHubLifecycleEntry(lock, reference)
	if err != nil {
		return nil, err
	}
	retainedDigest := ""
	if e.skillSources != nil && e.clawHubSourceScope.Kind != "" {
		installed, err := e.clawHub.LoadInstalled()
		if err != nil {
			return nil, fmt.Errorf("load retained skill source before uninstall: %w", err)
		}
		for _, item := range installed {
			if item != nil && item.SourceIdentity == identity && item.Compilation != nil {
				retainedDigest = item.Compilation.SourceDigest
				break
			}
		}
		if retainedDigest == "" {
			return nil, errors.New("installed skill source artifact could not be resolved")
		}
	}
	if err := e.clawHub.Uninstall(reference, force); err != nil {
		return nil, err
	}
	if retainedDigest != "" {
		if err := e.skillSources.Release(context.Background(), e.clawHubSourceScope, retainedDigest, "clawhub.installation:"+identity); err != nil {
			return nil, fmt.Errorf("release uninstalled skill source artifact: %w", err)
		}
	}
	result := &clawhub.LifecycleResult{
		APIVersion: clawhub.LifecycleAPIVersion, Operation: clawhub.LifecycleUninstall,
		SourceIdentity: identity, Reference: clawhub.SkillReference{Owner: entry.OwnerHandle, Slug: entry.Slug},
		Outcome: clawhub.LifecycleOutcomeRemoved, Changed: true,
	}
	if entry.Version != nil {
		result.PreviousVersion = *entry.Version
	}
	return result, nil
}

func resolveClawHubLifecycleEntry(lock clawhub.Lockfile, reference string) (string, clawhub.LockEntry, error) {
	reference = strings.TrimSpace(reference)
	if entry, ok := lock.Skills[reference]; ok {
		return reference, entry, nil
	}
	parsed, parseErr := clawhub.ParseSkillReference(reference)
	var identity string
	var resolved clawhub.LockEntry
	for candidate, entry := range lock.Skills {
		matches := strings.EqualFold(reference, entry.Slug)
		if parseErr == nil {
			matches = strings.EqualFold(parsed.Slug, entry.Slug) && (parsed.Owner == "" || strings.EqualFold(parsed.Owner, entry.OwnerHandle))
		}
		if !matches {
			continue
		}
		if identity != "" {
			return "", clawhub.LockEntry{}, clawhub.ErrAmbiguousSkill
		}
		identity, resolved = candidate, entry
	}
	if identity == "" {
		return "", clawhub.LockEntry{}, clawhub.ErrNotFound
	}
	return identity, resolved, nil
}
