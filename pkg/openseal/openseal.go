// Package openseal provides the stable public API for embedding OpenSeal
// as a library. Downstream consumers should import this package
// rather than reaching into individual pkg/ subpackages.
package openseal

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	kernelbundle "github.com/axiom-studio/openseal/pkg/bundle"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/chatfs"
	"github.com/axiom-studio/openseal/pkg/executor"
	"github.com/axiom-studio/openseal/pkg/httpaction"
	"github.com/axiom-studio/openseal/pkg/kernelapi"
	"github.com/axiom-studio/openseal/pkg/models"
	kerneloauth "github.com/axiom-studio/openseal/pkg/oauth"
	"github.com/axiom-studio/openseal/pkg/progression"
	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/activity"
	"github.com/axiom-studio/openseal/pkg/runtime/agentbuilder"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/artifactcatalog"
	"github.com/axiom-studio/openseal/pkg/runtime/callback"
	"github.com/axiom-studio/openseal/pkg/runtime/collaboration"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/conversationrun"
	"github.com/axiom-studio/openseal/pkg/runtime/externalconversation"
	"github.com/axiom-studio/openseal/pkg/runtime/modelstep"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/routine"
	"github.com/axiom-studio/openseal/pkg/runtime/runloop"
	"github.com/axiom-studio/openseal/pkg/runtime/runworker"
	"github.com/axiom-studio/openseal/pkg/runtime/skillbinding"
	"github.com/axiom-studio/openseal/pkg/runtime/store/postgres"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/runtime/workqueue"
	"github.com/axiom-studio/openseal/pkg/skill"
	skillopenclaw "github.com/axiom-studio/openseal/pkg/skill/openclaw"
	skillsource "github.com/axiom-studio/openseal/pkg/skill/source"
	"github.com/axiom-studio/openseal/pkg/skill/sourceartifact"
	"github.com/axiom-studio/openseal/pkg/skillcatalog"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
	"github.com/axiom-studio/openseal/pkg/validation"
	"github.com/axiom-studio/openseal/pkg/workforce"
	"github.com/axiom-studio/openseal/pkg/workspace"
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
	RunbookInterface         = runbook.Interface
	RunbookTrigger           = runbook.Trigger
	RunbookTriggerKind       = runbook.TriggerKind
	RunbookSchedule          = runbook.Schedule
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
	RunbookStepProducer      = routine.RunbookStepProducer
)

type (
	RunbookDiagnosticSeverity      = runbook.DiagnosticSeverity
	RunbookVerificationReport      = runbook.VerificationReport
	RunbookResolvedAction          = runbook.ResolvedAction
	RunbookVerificationEnvironment = runbook.VerificationEnvironment
)

type (
	AgentDefinition                         = kernelagent.AgentDefinition
	AgentStandingActionGrant                = kernelagent.StandingActionGrant
	AgentApprovalDestination                = kernelagent.ApprovalDestination
	AgentChannelRoute                       = kernelagent.ChannelRoute
	AgentManifest                           = kernelagent.Manifest
	AgentManifestMetadata                   = kernelagent.ManifestMetadata
	AgentManifestSpec                       = kernelagent.ManifestSpec
	AgentManifestInstallationRequest        = kernelagent.ManifestInstallationRequest
	AgentManifestInstallationResult         = kernelagent.ManifestInstallationResult
	AgentManifestInstallationRegistry       = kernelagent.ManifestInstallationRegistry
	AgentManifestExportRequest              = kernelagent.ManifestExportRequest
	AgentBundle                             = kernelagent.Bundle
	AgentBundleMetadata                     = kernelagent.BundleMetadata
	AgentBundleDeploymentPolicy             = kernelagent.BundleDeploymentPolicy
	AgentBundleRuntimeRequirement           = kernelagent.BundleRuntimeRequirement
	AgentBundleSkillRequirement             = kernelagent.BundleSkillRequirement
	AgentBundleSkillPolicy                  = kernelagent.BundleSkillPolicy
	AgentBundleCredentialNeed               = kernelagent.BundleCredentialNeed
	AgentBundleEndpointNeed                 = kernelagent.BundleEndpointNeed
	AgentBundleCallbackSubscription         = kernelagent.BundleCallbackSubscription
	AgentBundleCallbackNeed                 = kernelagent.BundleCallbackNeed
	AgentBundleExportRequest                = kernelagent.BundleExportRequest
	AgentBundlePlacement                    = kernelagent.BundlePlacement
	AgentBundleSkillPlacement               = kernelagent.BundleSkillPlacement
	AgentBundleEndpointPlacement            = kernelagent.BundleEndpointPlacement
	AgentBundleCallbackPlacement            = kernelagent.BundleCallbackPlacement
	AgentBundleRequirementKind              = kernelagent.BundleRequirementKind
	AgentBundleRequirementResolution        = kernelagent.BundleRequirementResolution
	AgentBundleInstallationPreview          = kernelagent.BundleInstallationPreview
	AgentBundleInstallationRequest          = kernelagent.BundleInstallationRequest
	AgentBundleEndpointInstallation         = kernelagent.BundleEndpointInstallation
	AgentBundleCallbackInstallation         = kernelagent.BundleCallbackInstallation
	AgentBundleInstallationPlan             = kernelagent.BundleInstallationPlan
	AgentDefinitionCompilation              = kernelagent.DefinitionCompilation
	AgentCompilationSource                  = kernelagent.CompilationSource
	AgentCompilationDiagnostic              = kernelagent.CompilationDiagnostic
	AgentCompilationStatus                  = kernelagent.CompilationStatus
	AgentSkillRequirement                   = kernelagent.SkillRequirement
	AgentAuthorityPolicy                    = kernelagent.AuthorityPolicy
	AgentApprovalTimeoutPolicy              = kernelagent.ApprovalTimeoutPolicy
	AgentMemoryPolicy                       = kernelagent.MemoryPolicy
	AgentEscalationPolicy                   = kernelagent.EscalationPolicy
	AgentObjectiveTemplate                  = kernelagent.ObjectiveTemplate
	AgentEvaluationCriterion                = kernelagent.EvaluationCriterion
	AgentAmendmentPolicy                    = kernelagent.AmendmentPolicy
	AgentDefinitionProvenance               = kernelagent.DefinitionProvenance
	AgentDeployment                         = kernelagent.AgentDeployment
	AgentDeploymentFilter                   = kernelagent.AgentDeploymentFilter
	AgentDeploymentRestrictions             = kernelagent.DeploymentRestrictions
	AgentDeploymentCapacity                 = kernelagent.DeploymentCapacity
	AgentDeploymentHealth                   = kernelagent.DeploymentHealth
	WorkspaceAuthority                      = workspace.Authority
	WorkspaceOperation                      = workspace.Operation
	AgentDefinitionActivation               = kernelagent.DefinitionActivation
	AgentRolloutStatus                      = kernelagent.RolloutStatus
	AgentDefinitionAmendment                = kernelagent.DefinitionAmendment
	AgentAmendmentStatus                    = kernelagent.AmendmentStatus
	AgentDefinitionFieldChange              = kernelagent.DefinitionFieldChange
	AgentAmendmentEvaluation                = kernelagent.AmendmentEvaluation
	AgentAmendmentDecision                  = kernelagent.AmendmentDecision
	ProposeAgentAmendmentRequest            = kernelagent.ProposeAmendmentRequest
	SubmitAgentAmendmentEvaluationRequest   = kernelagent.SubmitAmendmentEvaluationRequest
	ResolveAgentAmendmentRequest            = kernelagent.ResolveAmendmentRequest
	AgentRegistryStore                      = kernelagent.Store
	WorkforceBundle                         = kernelbundle.Bundle
	WorkforceBundleMetadata                 = kernelbundle.Metadata
	WorkforceBundleCompatibility            = kernelbundle.Compatibility
	WorkforceBundleCapabilityRequirement    = kernelbundle.CapabilityRequirement
	WorkforceBundleProvenance               = kernelbundle.Provenance
	WorkforceBundleOwnerKind                = kernelbundle.OwnerKind
	WorkforceBundleOwnerReference           = kernelbundle.OwnerReference
	WorkforceBundleAgent                    = kernelbundle.Agent
	WorkforceBundleRosterAssignment         = kernelbundle.RosterAssignment
	WorkforceBundleTeamDeployment           = kernelbundle.TeamDeployment
	WorkforceBundleTeam                     = kernelbundle.Team
	WorkforceBundleObjective                = kernelbundle.Objective
	WorkforceBundleRunbookActivation        = kernelbundle.RunbookActivation
	WorkforceBundleSignature                = kernelbundle.Signature
	WorkforceBundleTrustPolicy              = kernelbundle.TrustPolicy
	WorkforceBundleVerification             = kernelbundle.Verification
	WorkforceBundleChangeType               = kernelbundle.ChangeType
	WorkforceBundleChange                   = kernelbundle.Change
	WorkforceBundleDiff                     = kernelbundle.Diff
	WorkforceBundleUpgradePlan              = kernelbundle.UpgradePlan
	WorkforceBundleInspection               = kernelbundle.Inspection
	WorkforceBundlePlacement                = kernelbundle.Placement
	WorkforceBundleTeamPlacement            = kernelbundle.TeamPlacement
	WorkforceBundleRequirement              = kernelbundle.Requirement
	WorkforceBundleInstallationPreview      = kernelbundle.InstallationPreview
	WorkforceBundleInstallationRequest      = kernelbundle.InstallationRequest
	WorkforceBundleInstallationPlan         = kernelbundle.InstallationPlan
	WorkforceBundleAgentInstallation        = kernelbundle.AgentInstallation
	WorkforceBundleTeamInstallation         = kernelbundle.TeamInstallation
	WorkforceBundleInstallationReceipt      = kernelbundle.InstallationReceipt
	WorkforceBundleInstallationStore        = kernelbundle.InstallationStore
	WorkforceBundleExportRequest            = kernelbundle.ExportRequest
	WorkforceBundleAgentExport              = kernelbundle.AgentExport
	WorkforceBundleTeamExport               = kernelbundle.TeamExport
	WorkforceBundleObjectiveExport          = kernelbundle.ObjectiveExport
	WorkforceBundleRunbookExport            = kernelbundle.RunbookExport
	KernelAgentDeploymentList               = kernelapi.AgentDeploymentList
	KernelAgentDeploymentCatalogEntry       = kernelapi.AgentDeploymentCatalogEntry
	KernelUpdateAgentDeploymentRequest      = kernelapi.UpdateAgentDeploymentRequest
	KernelAgentDeploymentUpdateResult       = kernelapi.AgentDeploymentUpdateResult
	KernelActivateAgentDefinitionRequest    = kernelapi.ActivateAgentDefinitionRequest
	KernelRollbackAgentDefinitionRequest    = kernelapi.RollbackAgentDefinitionRequest
	KernelAgentDefinitionActivationResult   = kernelapi.AgentDefinitionActivationResult
	KernelAgentDefinitionAmendmentList      = kernelapi.AgentDefinitionAmendmentList
	KernelActivateAgentAmendmentRequest     = kernelapi.ActivateAgentDefinitionAmendmentRequest
	KernelAgentAmendmentActivationResult    = kernelapi.AgentDefinitionAmendmentActivationResult
	KernelAgentDefinitionCompilationHistory = kernelapi.AgentDefinitionCompilationHistory
	TeamDefinition                          = kernelteam.Definition
	TeamManifest                            = kernelteam.Manifest
	TeamManifestMetadata                    = kernelteam.ManifestMetadata
	TeamManifestSpec                        = kernelteam.ManifestSpec
	TeamManifestMember                      = kernelteam.ManifestMember
	TeamManifestDelegationPolicy            = kernelteam.ManifestDelegationPolicy
	TeamManifestSharedContextPolicy         = kernelteam.ManifestSharedContextPolicy
	TeamChannelParticipation                = kernelteam.ChannelParticipation
	TeamRoutingPolicy                       = kernelteam.RoutingPolicy
	TeamDelegationPolicy                    = kernelteam.DelegationPolicy
	TeamApprovalPolicy                      = kernelteam.ApprovalPolicy
	TeamDeployment                          = kernelteam.Deployment
	TeamDeploymentStatus                    = kernelteam.DeploymentStatus
	TeamRosterAssignment                    = kernelteam.RosterAssignment
	TeamDeploymentRestrictions              = kernelteam.DeploymentRestrictions
	TeamDefinitionAmendment                 = kernelteam.DefinitionAmendment
	TeamAmendmentStatus                     = kernelteam.AmendmentStatus
	TeamAmendmentEvaluation                 = kernelteam.AmendmentEvaluation
	TeamAmendmentDecision                   = kernelteam.AmendmentDecision
	ProposeTeamAmendmentRequest             = kernelteam.ProposeAmendmentRequest
	SubmitTeamAmendmentEvaluationRequest    = kernelteam.SubmitAmendmentEvaluationRequest
	ResolveTeamAmendmentRequest             = kernelteam.ResolveAmendmentRequest
	RecoverTeamParticipationRequest         = kernelteam.RecoverParticipationRequest
	TeamParticipationRecoveryResult         = kernelteam.ParticipationRecoveryResult
	TeamRegistryStore                       = kernelteam.Store
	AuthorityProgressionOwnerKind           = progression.OwnerKind
	AuthorityProgressionDirection           = progression.Direction
	AuthorityProgressionCriterionOutcome    = progression.CriterionOutcome
	AuthorityProgressionEvaluationPolicy    = progression.EvaluationPolicy
	AuthorityProgressionEvaluationRequest   = progression.EvaluationRequest
	AuthorityProgressionRecommendation      = progression.Recommendation
	AgentAuthorityProgressionCeiling        = progression.AgentPolicyCeiling
	TeamAuthorityProgressionCeiling         = progression.TeamPolicyCeiling
	ProposeAgentAuthorityProgressionRequest = progression.ProposeAgentRequest
	ProposeTeamAuthorityProgressionRequest  = progression.ProposeTeamRequest
	WorkforceSharedContextPolicy            = workforce.SharedContextPolicy
	WorkforceObjectiveTemplate              = workforce.ObjectiveTemplate
	WorkforceEvaluationCriterion            = workforce.EvaluationCriterion
	WorkforceAmendmentPolicy                = workforce.AmendmentPolicy
	WorkforceDefinitionProvenance           = workforce.DefinitionProvenance
	WorkforceDefinitionActivation           = workforce.DefinitionActivation
	DeploymentChangeKind                    = workforce.DeploymentChangeKind
	WorkforceSkillCapability                = skillcatalog.SkillCapability
	WorkforceSkillActionContract            = skillcatalog.SkillActionContract
	WorkforceSkillCredential                = skillcatalog.SkillCredential
	WorkforceConversationAdapterCapability  = skillcatalog.ConversationAdapterCapability
	WorkforceCallbackAdapterCapability      = skillcatalog.CallbackAdapterCapability
	WorkforceAuthorityConstraint            = skillcatalog.AuthorityConstraint
	WorkforceCatalogDiagnostic              = skillcatalog.CatalogDiagnostic
	WorkforceSkillReadiness                 = skillcatalog.SkillReadiness
	WorkforceSkillCompatibility             = skillcatalog.SkillCompatibility
	WorkforceCapabilityCatalog              = skillcatalog.CapabilityCatalog
	WorkforceExecutionTarget                = kernelapi.WorkforceExecutionTarget
	WorkforceExecutionTargetCandidate       = kernelapi.WorkforceExecutionTargetCandidate
	WorkforceExecutionTargetList            = kernelapi.WorkforceExecutionTargetList
	WorkforceSensitiveInputKind             = validation.SensitiveInputKind
	WorkforceSensitiveInputFinding          = validation.SensitiveInputFinding
	SensitiveInputError                     = validation.SensitiveInputError
	SkillReferenceUpgradePlan               = models.SkillReferenceUpgradePlan
	SkillReferenceUpgradeFinding            = models.SkillReferenceUpgradeFinding
	SkillReferenceIdentity                  = models.SkillReferenceIdentity
	SkillReferenceObjectiveReference        = models.SkillReferenceObjectiveReference
	SkillReferenceObjectiveImpact           = models.SkillReferenceObjectiveImpact
	PlanSkillReferenceUpgradeRequest        = skillbinding.PlanSkillReferenceUpgradeRequest
	ApplySkillReferenceUpgradeRequest       = skillbinding.ApplySkillReferenceUpgradeRequest
	SkillReferenceUpgradeApproval           = models.SkillReferenceUpgradeApproval
	SkillReferenceUpgradeReceipt            = models.SkillReferenceUpgradeReceipt
	SkillRuntimeUsageFilter                 = models.SkillRuntimeUsageFilter
	SkillRuntimeReference                   = models.SkillRuntimeReference
	SkillRuntimeUsageStore                  = models.SkillRuntimeUsageStore

	PortfolioStore                       = models.PortfolioStore
	KernelStore                          = models.KernelStore
	PostgresStore                        = postgres.Store
	PostgresStoreOption                  = postgres.Option
	PostgresPoolConfig                   = postgres.PoolConfig
	PostgresPoolStats                    = postgres.PoolStats
	WorkerLimiter                        = workqueue.WorkerLimiter
	WorkerLimiterStats                   = workqueue.WorkerLimiterStats
	Scope                                = models.Scope
	ObjectiveOwner                       = models.ObjectiveOwner
	Objective                            = models.Objective
	ObjectiveExecutionPolicy             = models.ObjectiveExecutionPolicy
	ObjectiveStatus                      = models.ObjectiveStatus
	ScheduledAgentTask                   = models.ScheduledAgentTask
	RoutineGrant                         = models.RoutineGrant
	RoutineBrief                         = routine.RoutineBrief
	RoutineOwnerAddressResolver          = models.RoutineOwnerAddressResolver
	RevokeRoutineGrantsRequest           = routine.RevokeRoutineGrantsRequest
	RoutineScript                        = models.RoutineScript
	RoutineScriptStatus                  = routine.RoutineScriptStatus
	ChatFile                             = routine.ChatFile
	ChatFileResolver                     = routine.ChatFileResolver
	WorkspaceCommandPin                  = workspace.CommandPin
	WorkspaceCommandRecord               = workspace.CommandRecord
	WorkspaceExpectedDigest              = workspace.ExpectedDigest
	RunbookActivation                    = models.RunbookActivation
	RunbookDetail                        = routine.RunbookDetail
	RunbookExecutionAudit                = routine.RunbookExecutionAudit
	RunExecutionAudit                    = routine.RunExecutionAudit
	RunbookExecutionAuditRun             = routine.RunbookExecutionAuditRun
	RunbookNodeExecutionAudit            = routine.RunbookNodeExecutionAudit
	RunbookDataLineage                   = routine.RunbookDataLineage
	RunbookActivationStatus              = models.RunbookActivationStatus
	RunbookActivationFilter              = models.RunbookActivationFilter
	CreateRunbookActivationRequest       = routine.CreateRunbookActivationRequest
	UpdateRunbookActivationRequest       = routine.UpdateRunbookActivationRequest
	StartRunbookActivationRequest        = routine.StartRunbookActivationRequest
	RunbookScheduleResult                = routine.RunbookScheduleResult
	ObjectiveFilter                      = models.ObjectiveFilter
	AgentRun                             = models.AgentRun
	AgentRunIntervention                 = models.AgentRunIntervention
	ConversationAnswerReceipt            = models.ConversationAnswerReceipt
	HumanInterventionRequest             = models.HumanInterventionRequest
	HumanInterventionStatus              = models.HumanInterventionStatus
	BudgetPolicy                         = models.BudgetPolicy
	BudgetUsage                          = models.BudgetUsage
	BudgetReservation                    = models.BudgetReservation
	BudgetState                          = models.BudgetState
	AgentRunStatus                       = models.AgentRunStatus
	AgentRunFilter                       = models.AgentRunFilter
	AgentRunOrder                        = models.AgentRunOrder
	AgentRunOwnerSummary                 = models.AgentRunOwnerSummary
	RunSource                            = models.RunSource
	RunKind                              = models.RunKind
	WakeCondition                        = models.WakeCondition
	CreateObjectiveRequest               = portfolio.CreateObjectiveRequest
	CreateObjectiveResult                = portfolio.CreateObjectiveResult
	UpdateObjectiveRequest               = portfolio.UpdateObjectiveRequest
	CreateAgentRunRequest                = models.CreateAgentRunRequest
	AgentRunCommandKind                  = agentrun.AgentRunCommandKind
	AgentRunCommandRequest               = agentrun.AgentRunCommandRequest
	AgentRunCommandResult                = agentrun.AgentRunCommandResult
	RunActivityStore                     = models.RunActivityStore
	ActivityEvent                        = models.ActivityEvent
	ActivityActor                        = models.ActivityActor
	ActivityFilter                       = models.ActivityFilter
	ActivityFeedRequest                  = activity.ActivityFeedRequest
	ActivityProjection                   = models.ActivityProjection
	ActivityFeedPage                     = activity.ActivityFeedPage
	ActivitySeverity                     = models.ActivitySeverity
	ActivityVisibility                   = models.ActivityVisibility
	RunTransitionRequest                 = activity.RunTransitionRequest
	RunDependencyStore                   = models.RunDependencyStore
	DependencyKernelStore                = models.DependencyKernelStore
	RunDependencyKind                    = models.RunDependencyKind
	RunDependencyState                   = models.RunDependencyState
	FanInMode                            = models.FanInMode
	DependencyFailureMode                = models.DependencyFailureMode
	RunDependencyGroupStatus             = models.RunDependencyGroupStatus
	RunDependencyPolicy                  = models.RunDependencyPolicy
	RunDependencyGroup                   = models.RunDependencyGroup
	RunDependency                        = models.RunDependency
	RunDependencySpec                    = models.RunDependencySpec
	RunDependencyEvaluation              = models.RunDependencyEvaluation
	CreateRunDependencyGroupRequest      = models.CreateRunDependencyGroupRequest
	ResolveRunDependencyRequest          = portfolio.ResolveRunDependencyRequest
	ReconcileRunDependencyGroupRequest   = portfolio.ReconcileRunDependencyGroupRequest
	RunDependencyGroupWork               = models.RunDependencyGroupWork
	RunDependencyResult                  = models.RunDependencyResult
	ConversationStore                    = models.ConversationStore
	Conversation                         = models.Conversation
	ConversationStatus                   = models.ConversationStatus
	ConversationParticipantType          = models.ConversationParticipantType
	ConversationParticipant              = models.ConversationParticipant
	ConversationMessageIntent            = models.ConversationMessageIntent
	ConversationAudienceKind             = models.ConversationAudienceKind
	ConversationAudience                 = models.ConversationAudience
	ConversationReferenceKind            = models.ConversationReferenceKind
	ConversationReference                = models.ConversationReference
	ConversationArchiveSource            = conversation.ConversationArchiveSource
	ConversationArchiveMessage           = conversation.ConversationArchiveMessage
	ImportConversationArchiveRequest     = conversation.ImportConversationArchiveRequest
	ConversationArchiveImportResult      = conversation.ConversationArchiveImportResult
	ChannelMessage                       = models.ChannelMessage
	ChannelMessageQuote                  = models.ChannelMessageQuote
	AgentBuilderParameters               = models.AgentBuilderParameters
	AgentBuilderDraftProfile             = models.AgentBuilderDraftProfile
	MessageReaction                      = models.MessageReaction
	MessageReactionRecord                = models.MessageReactionRecord
	MessageReactionRequest               = conversation.MessageReactionRequest
	MessageReactionResult                = conversation.MessageReactionResult
	MessageReactionProjector             = conversation.MessageReactionProjector
	ConversationReactionEvent            = models.ConversationReactionEvent
	ConversationReactionStore            = models.ConversationReactionStore
	TurnReaction                         = conversation.TurnReaction
	ConversationCursor                   = models.ConversationCursor
	ConversationPresenceState            = models.ConversationPresenceState
	ConversationPresence                 = models.ConversationPresence
	ConversationAddressing               = models.ConversationAddressing
	ConversationResponder                = models.ConversationResponder
	ConversationRouter                   = conversation.ConversationRouter
	ConversationRouterFunc               = conversation.ConversationRouterFunc
	ConversationRouteRequest             = conversation.ConversationRouteRequest
	ConversationRouteDecision            = conversation.ConversationRouteDecision
	RouteMember                          = conversation.RouteMember
	ConversationRoutingRequest           = conversation.ConversationRoutingRequest
	CommitConversationRoutingRequest     = conversation.CommitConversationRoutingRequest
	ParticipationRoundStatus             = models.ParticipationRoundStatus
	ParticipationRound                   = models.ParticipationRound
	ConversationParticipantBinding       = conversation.ConversationParticipantBinding
	ConversationParticipantQuery         = conversation.ConversationParticipantQuery
	ConversationParticipantSource        = conversation.ConversationParticipantSource
	ConversationParticipantSourceFunc    = conversation.ConversationParticipantSourceFunc
	ConversationCoordinatorConfig        = conversation.ConversationCoordinatorConfig
	ConversationRunSchedulerConfig       = conversationrun.ConversationRunSchedulerConfig
	ConversationRunTurnRunnerConfig      = conversationrun.ConversationRunTurnRunnerConfig
	ConversationPlace                    = conversation.ConversationPlace
	ConversationRunReconcilerConfig      = conversationrun.ConversationRunReconcilerConfig
	ConversationRunReconcileResult       = conversationrun.ConversationRunReconcileResult
	ConversationChangeRequest            = conversation.ConversationChangeRequest
	ConversationChangeSet                = conversation.ConversationChangeSet
	CreateConversationRequest            = conversation.CreateConversationRequest
	UpdateConversationRequest            = conversation.UpdateConversationRequest
	ConversationApprovalMode             = models.ConversationApprovalMode
	SetConversationApprovalModeRequest   = conversation.SetConversationApprovalModeRequest
	SetConversationApprovalModeResult    = conversation.SetConversationApprovalModeResult
	ConversationFilter                   = models.ConversationFilter
	Space                                = models.Space
	SpaceLook                            = models.SpaceLook
	SpaceMember                          = models.SpaceMember
	SpaceMemberType                      = models.SpaceMemberType
	SpaceAccess                          = models.SpaceAccess
	SpacePerson                          = models.SpacePerson
	AddSpacePersonRequest                = portfolio.AddSpacePersonRequest
	SetSpaceAccessStateRequest           = portfolio.SetSpaceAccessStateRequest
	SpaceStore                           = models.SpaceStore
	SpacePatch                           = models.SpacePatch
	CreateSpaceRequest                   = portfolio.CreateSpaceRequest
	UpdateSpaceRequest                   = portfolio.UpdateSpaceRequest
	PutSpaceMemberRequest                = portfolio.PutSpaceMemberRequest
	DeleteSpaceMemberRequest             = portfolio.DeleteSpaceMemberRequest
	SpaceAgentRemovalTeam                = portfolio.SpaceAgentRemovalTeam
	SpaceAgentRemovalNeedsConfirmation   = portfolio.SpaceAgentRemovalNeedsConfirmation
	SpaceTeamOutcome                     = portfolio.SpaceTeamOutcome
	PostChannelMessageRequest            = conversation.PostChannelMessageRequest
	ChannelMessageFilter                 = models.ChannelMessageFilter
	ConversationViewer                   = models.ConversationViewer
	ParticipationRoundFilter             = models.ParticipationRoundFilter
	AdvanceConversationCursorRequest     = conversation.AdvanceConversationCursorRequest
	SetConversationPresenceRequest       = conversation.SetConversationPresenceRequest
	ReleaseConversationPresenceRequest   = models.ReleaseConversationPresenceRequest
	ChannelMessageCommitResult           = models.ChannelMessageCommitResult
	ParticipationRoundResult             = models.ParticipationRoundResult
	EmbedInstallationStatus              = models.EmbedInstallationStatus
	EmbedIdentityMode                    = models.EmbedIdentityMode
	EmbedIdentityPolicy                  = models.EmbedIdentityPolicy
	EmbedActionGrant                     = models.EmbedActionGrant
	EmbedPermissionPolicy                = models.EmbedPermissionPolicy
	EmbedInstallation                    = models.EmbedInstallation
	EmbedSessionStatus                   = models.EmbedSessionStatus
	EmbedSession                         = models.EmbedSession
	EmbedInstallationFilter              = models.EmbedInstallationFilter
	EmbedStore                           = models.EmbedStore
	CreateEmbedInstallationRequest       = conversation.CreateEmbedInstallationRequest
	UpdateEmbedInstallationRequest       = conversation.UpdateEmbedInstallationRequest
	OpenEmbedSessionRequest              = conversation.OpenEmbedSessionRequest
	OpenEmbedSessionResult               = conversation.OpenEmbedSessionResult
	CollaborationStore                   = models.CollaborationStore
	CollaborationKernelStore             = models.CollaborationKernelStore
	CollaborationParty                   = models.CollaborationParty
	ArtifactRequirement                  = models.ArtifactRequirement
	ArtifactReference                    = models.ArtifactReference
	Artifact                             = models.Artifact
	ArtifactClassification               = models.ArtifactClassification
	ArtifactRetention                    = models.ArtifactRetention
	ArtifactProvenance                   = models.ArtifactProvenance
	ArtifactEvidenceLink                 = models.EvidenceLink
	ArtifactEvidenceRelation             = models.EvidenceRelation
	ArtifactEvidenceTargetKind           = models.EvidenceTargetKind
	ArtifactFilter                       = models.ArtifactFilter
	ArtifactStore                        = models.ArtifactStore
	ArtifactCatalog                      = artifactcatalog.ArtifactCatalog
	RegisterArtifactRequest              = artifactcatalog.RegisterArtifactRequest
	ArtifactRegistrationResult           = artifactcatalog.ArtifactRegistrationResult
	ArtifactContentStore                 = models.ArtifactContentStore
	ArtifactContentInspector             = artifactcatalog.ArtifactContentInspector
	ArtifactContentDeleter               = artifactcatalog.ArtifactContentDeleter
	ArtifactContentAvailability          = models.ArtifactContentAvailability
	ArtifactContentResolver              = artifactcatalog.ArtifactContentResolver
	ArtifactContentWrite                 = models.ArtifactContentWrite
	ArtifactStoredContent                = models.ArtifactStoredContent
	ArtifactContentResolutionRequest     = artifactcatalog.ArtifactContentResolutionRequest
	ArtifactContentResolution            = artifactcatalog.ArtifactContentResolution
	ArtifactRetentionService             = artifactcatalog.ArtifactRetentionService
	ArtifactRetentionSweepResult         = artifactcatalog.ArtifactRetentionSweepResult
	ArtifactRetentionDeletion            = artifactcatalog.ArtifactRetentionDeletion
	AgentRequest                         = models.AgentRequest
	AgentRequestKind                     = models.AgentRequestKind
	AgentRequestStatus                   = models.AgentRequestStatus
	AgentRequestDecision                 = collaboration.AgentRequestDecision
	AgentRequestBidDecision              = models.AgentRequestBidDecision
	AgentRequestCandidate                = models.AgentRequestCandidate
	AgentRequestBid                      = models.AgentRequestBid
	AgentRequestAssignmentDecision       = models.AgentRequestAssignmentDecision
	AgentRequestCompletionReviewDecision = models.AgentRequestCompletionReviewDecision
	AgentRequestAcceptancePolicy         = models.AgentRequestAcceptancePolicy
	AgentRequestFilter                   = models.AgentRequestFilter
	CreateAgentRequestRequest            = collaboration.CreateAgentRequestRequest
	AgentRequestGroupSpec                = collaboration.AgentRequestGroupSpec
	CreateAgentRequestGroupRequest       = collaboration.CreateAgentRequestGroupRequest
	AgentRequestGroupResult              = collaboration.AgentRequestGroupResult
	RespondAgentRequestRequest           = collaboration.RespondAgentRequestRequest
	SubmitAgentRequestBidRequest         = collaboration.SubmitAgentRequestBidRequest
	CompleteAgentRequestRequest          = collaboration.CompleteAgentRequestRequest
	ReviewAgentRequestRequest            = collaboration.ReviewAgentRequestCompletionRequest
	AgentRequestResult                   = collaboration.AgentRequestResult
	AgentTurnStore                       = models.AgentTurnStore
	AgentTurn                            = models.AgentTurn
	AgentTurnStatus                      = models.AgentTurnStatus
	AgentTurnFilter                      = models.AgentTurnFilter
	AgentTurnRecord                      = kernelapi.AgentTurnRecord
	TurnUsage                            = models.TurnUsage
	BeginAgentTurnRequest                = activity.BeginAgentTurnRequest
	FinishAgentTurnRequest               = activity.FinishAgentTurnRequest
	HostedSkillPrompt                    = models.HostedSkillPrompt
	HostedSkillSelection                 = models.HostedSkillSelection
	HostedSkillDisposition               = models.HostedSkillDisposition
	HostedRunbookOperation               = agentrun.HostedRunbookOperation
	HostedConversationTaskContext        = models.HostedConversationTaskContext
	ConversationTaskSnapshot             = models.ConversationTaskSnapshot
	ConversationTask                     = models.ConversationTask
	ConversationTaskStore                = models.ConversationTaskStore
	ConversationActiveRunsReadStore      = models.ConversationActiveRunsReadStore
	ConversationForegroundRunsReadStore  = models.ConversationForegroundRunsReadStore
	ConversationTaskKernelStore          = models.ConversationTaskKernelStore
	ConversationTaskCreateRecord         = models.ConversationTaskCreateRecord
	GetConversationTaskRequest           = conversationrun.GetConversationTaskRequest
	CancelConversationTaskRequest        = conversationrun.CancelConversationTaskRequest
	ConversationTaskFilter               = models.ConversationTaskFilter
	ConversationTaskService              = conversationrun.ConversationTaskService
	StartConversationTaskRequest         = conversationrun.StartConversationTaskRequest
	ConversationTaskResult               = models.ConversationTaskResult
	HostedRunBudget                      = modelstep.HostedRunBudget
	TurnHostFailure                      = modelstep.TurnHostFailure
	HostedContextPressure                = modelstep.HostedContextPressure
	CapabilityInvocationStepProducer     = modelstep.CapabilityInvocationStepProducer
	AgentRunScheduleStore                = models.AgentRunScheduleStore
	AgentRunAdmissionStore               = models.AgentRunAdmissionStore
	AgentRunClaimRequest                 = agentrun.AgentRunClaimRequest
	AgentRunAdmissionOutcome             = models.AgentRunAdmissionOutcome
	AgentRunAdmissionReason              = models.AgentRunAdmissionReason
	AgentRunAdmissionBlock               = models.AgentRunAdmissionBlock
	AgentRunAdmissionDecision            = models.AgentRunAdmissionDecision
	AgentRunWorkerConfig                 = runworker.AgentRunWorkerConfig
	DynamicAgentRunWorkerConfig          = runworker.DynamicAgentRunWorkerConfig
	TurnRunnerBinding                    = agentrun.TurnRunnerBinding
	PreparedSkillRuntime                 = agentrun.PreparedSkillRuntime
	TurnRunnerResolver                   = conversation.TurnRunnerResolver
	TurnRunnerResolverFunc               = conversation.TurnRunnerResolverFunc
	AgentTurnCatalog                     = conversationrun.AgentTurnCatalog
	LoopBindingConfig                    = runworker.LoopBindingConfig
	SkillHostCapabilityResolver          = runworker.SkillHostCapabilityResolver
	SkillHostCapabilityResolverFunc      = runworker.SkillHostCapabilityResolverFunc
	WorkerScopeSource                    = workqueue.WorkerScopeSource
	WorkerScopeSourceFunc                = workqueue.WorkerScopeSourceFunc
	WakeSignal                           = agentrun.WakeSignal
	WokenRun                             = agentrun.WokenRun
	WakeResult                           = agentrun.WakeResult
	EventEnvelope                        = models.EventEnvelope
	SkillCatalog                         = skill.Catalog
	SkillDefinition                      = skill.Definition
	SkillAction                          = skill.Action
	SkillBinding                         = skill.Binding
	SkillBindingReference                = skill.BindingReference
	SkillRunActor                        = skill.RunActor
	SkillUmbrella                        = skill.Umbrella
	SkillUmbrellaKind                    = skill.UmbrellaKind
	SkillHostBindingSet                  = skill.HostBindingSet
	SkillHostBindingProvider             = skill.HostBindingProvider
	SkillBindingActor                    = skill.BindingActor
	SkillBindingLifecycleAction          = skill.BindingLifecycleAction
	SkillBindingLifecycleEntry           = skill.BindingLifecycleEntry
	UpsertSkillBindingRequest            = skill.UpsertBindingRequest
	DisableSkillBindingRequest           = skill.DisableBindingRequest
	DeleteSkillBindingRequest            = skill.DeleteBindingRequest
	SkillScope                           = skill.ScopeReference
	SkillIdentity                        = capability.SkillIdentity
	SkillRiskLevel                       = skill.RiskLevel
	SkillSideEffect                      = skill.SideEffect
	SkillIdempotencyMode                 = skill.IdempotencyMode
	SkillCredentialRequirement           = skill.CredentialRequirement
	SkillCredentialReference             = skill.CredentialReference
	OAuth2Subject                        = capability.OAuth2Subject
	OAuth2Requirement                    = capability.OAuth2Requirement
	OAuth2GrantSummary                   = capability.OAuth2GrantSummary
	OAuthConnectionStatus                = kerneloauth.ConnectionStatus
	OAuthAuthorizationSessionStatus      = kerneloauth.SessionStatus
	OAuthConnectionOwnerKind             = kerneloauth.OwnerKind
	OAuthConnectionOwner                 = kerneloauth.Owner
	OAuthExternalIdentity                = kerneloauth.ExternalIdentity
	OAuthConnection                      = kerneloauth.Connection
	OAuthAuthorizationSession            = kerneloauth.AuthorizationSession
	BeginOAuthAuthorizationRequest       = kerneloauth.BeginAuthorizationRequest
	BeginOAuthAuthorizationResult        = kerneloauth.BeginAuthorizationResult
	CompleteOAuthAuthorizationRequest    = kerneloauth.CompleteAuthorizationRequest
	CompleteOAuthAuthorizationResult     = kerneloauth.CompleteAuthorizationResult
	OAuthProviderAuthorizationRequest    = kerneloauth.ProviderAuthorizationRequest
	OAuthProviderCodeExchangeRequest     = kerneloauth.ProviderCodeExchangeRequest
	OAuthProviderRefreshRequest          = kerneloauth.ProviderRefreshRequest
	OAuthProviderRevocationRequest       = kerneloauth.ProviderRevocationRequest
	OAuthTokenGrant                      = kerneloauth.TokenGrant
	OAuthProvider                        = kerneloauth.Provider
	OAuthRefreshProvider                 = kerneloauth.RefreshProvider
	OAuthRevocationProvider              = kerneloauth.RevocationProvider
	OAuthCredentialStoreRequest          = kerneloauth.CredentialStoreRequest
	OAuthCredentialStore                 = kerneloauth.CredentialStore
	OAuthStore                           = kerneloauth.Store
	OAuthService                         = kerneloauth.Service
	OAuthMemoryStore                     = kerneloauth.MemoryStore
	RefreshOAuthConnectionRequest        = kerneloauth.RefreshConnectionRequest
	RevokeOAuthConnectionRequest         = kerneloauth.RevokeConnectionRequest
	ConversationEndpointMode             = capability.ConversationEndpointMode
	ConversationAdapterFeature           = capability.ConversationAdapterFeature
	ConversationDeliveryOperation        = capability.ConversationDeliveryOperation
	ConversationDeliveryOrdering         = capability.ConversationDeliveryOrdering
	ConversationDeliveryCapabilities     = capability.ConversationDeliveryCapabilities
	ConversationAdapterTransport         = capability.ConversationAdapterTransport
	ConversationDestinationDiscovery     = capability.ConversationDestinationDiscovery
	ConversationAdapter                  = capability.ConversationAdapter
	ConversationSubjectEvidence          = capability.ConversationSubjectEvidence
	BoundConversationAdapter             = capability.BoundConversationAdapter
	CallbackAdapterTransport             = capability.CallbackAdapterTransport
	CallbackAdapter                      = capability.CallbackAdapter
	BoundCallbackAdapter                 = capability.BoundCallbackAdapter
	SkillTransportReference              = skill.TransportReference
	SkillTransportArgument               = skill.TransportArgument
	SkillArgumentRule                    = skill.ArgumentRule
	SkillBindingArgumentSource           = skill.BindingArgumentSource
	SkillBindingArgumentValue            = skill.BindingArgumentValue
	SkillActionRetryPolicy               = skill.ActionRetryPolicy
	SkillRequirements                    = skill.Requirements
	SkillInstaller                       = skill.Installer
	ModelSkillAction                     = skill.ModelAction
	ModelSkillPrompt                     = skill.ModelPrompt
	BoundSkillAction                     = skill.BoundAction
	SkillPromptModule                    = skill.PromptModule
	SkillHostCapabilityState             = skill.HostCapabilityState
	SkillAdapterState                    = skill.AdapterState
	SkillAdapterCapability               = skill.AdapterCapability
	SkillResourceStageRequest            = skill.ResourceStageRequest
	SkillResourceStage                   = skill.ResourceStage
	SkillResourceStager                  = skill.ResourceStager
	SkillResourceContentProvider         = skill.ResourceContentProvider
	SkillRuntimePreparationState         = skill.RuntimePreparationState
	SkillRuntimePreparationRequest       = skill.RuntimePreparationRequest
	SkillPreparedRuntime                 = skill.PreparedRuntime
	SkillRuntimePreparationResult        = skill.RuntimePreparationResult
	SkillRuntimePreparer                 = skill.RuntimePreparer
	SkillAvailabilityReason              = skill.AvailabilityReason
	SkillBindingActivationPreview        = skill.BindingActivationPreview
	ActivatedSkill                       = skill.ActivatedSkill
	ActivatedConversationAdapter         = skill.ActivatedConversationAdapter
	UnavailableSkill                     = skill.UnavailableSkill
	SkillActivationSnapshot              = skill.ActivationSnapshot
	SkillDiscoveryReadiness              = skill.DiscoveryReadiness
	SkillDiscoveryRequest                = skill.DiscoveryRequest
	SkillDiscoveryAction                 = skill.DiscoveryAction
	SkillDiscoveryCredential             = skill.DiscoveryCredential
	SkillDiscoveryConversationAdapter    = skill.DiscoveryConversationAdapter
	SkillDiscoveryCompatibility          = skill.DiscoveryCompatibility
	SkillDiscoveryCandidate              = skill.DiscoveryCandidate
	SkillDiscoveryPage                   = skill.DiscoveryPage
	SkillDiscoveryProvider               = skill.DiscoveryProvider
	SkillDiscoveryProviderFunc           = skill.DiscoveryProviderFunc
	HTTPActionParameter                  = httpaction.Parameter
	HTTPActionInvocation                 = httpaction.Invocation
	HTTPActionRequestPolicy              = httpaction.RequestPolicy
	HTTPActionRequestPolicyFunc          = httpaction.RequestPolicyFunc
	HTTPActionExecutor                   = httpaction.Executor
	SkillCatalogStore                    = skill.CatalogStore
	ActionStore                          = models.ActionStore
	ActionCall                           = models.ActionCall
	ExternalOperationIdentity            = activity.ExternalOperationIdentity
	ExternalOperationConflictError       = models.ExternalOperationConflictError
	ActionCallStatus                     = models.ActionCallStatus
	ActionFilter                         = models.ActionFilter
	ActionDisposition                    = action.ActionDisposition
	ActionPolicyInput                    = action.ActionPolicyInput
	ActionPolicyDecision                 = action.ActionPolicyDecision
	ActionPolicyEvaluator                = action.ActionPolicyEvaluator
	ActionPolicyEvaluatorFunc            = action.ActionPolicyEvaluatorFunc
	ActionProposalValidator              = action.ActionProposalValidator
	ActionProposalValidationInput        = agentrun.ActionProposalValidationInput
	ProposeActionRequest                 = action.ProposeActionRequest
	ActionProposalResult                 = models.ActionProposalResult
	ApprovalCheckpoint                   = models.ApprovalCheckpoint
	ApprovalStatus                       = models.ApprovalStatus
	ApprovalDecision                     = models.ApprovalDecision
	ApprovalPrincipal                    = models.ApprovalPrincipal
	ApprovalFilter                       = models.ApprovalFilter
	ApprovalAuthorizer                   = action.ApprovalAuthorizer
	ApprovalAuthorizerFunc               = action.ApprovalAuthorizerFunc
	ResolveApprovalRequest               = action.ResolveApprovalRequest
	ApprovalResolutionResult             = models.ApprovalResolutionResult
	RunForkBranch                        = agentrun.RunForkBranch
	CreateRunForkRequest                 = agentrun.CreateRunForkRequest
	RunForkResult                        = agentrun.RunForkResult
	RunForkCoordinator                   = agentrun.RunForkCoordinator
	ActionWorkerConfig                   = action.ActionWorkerConfig
	DynamicActionWorkerConfig            = action.DynamicActionWorkerConfig
	CredentialResolver                   = action.CredentialResolver
	CredentialResolverFunc               = action.CredentialResolverFunc
	CredentialResolutionRequest          = action.CredentialResolutionRequest
	ActionDispatchInput                  = agentrun.ActionDispatchInput
	ActionDispatcher                     = agentrun.ActionDispatcher
	ActionDispatcherFunc                 = action.ActionDispatcherFunc
	ToolInvoker                          = modelstep.ToolInvoker
	ToolInvokerFunc                      = modelstep.ToolInvokerFunc
	ToolInvocation                       = models.ToolInvocation
	ToolActionDispatcher                 = modelstep.ToolActionDispatcher
	AgentBehaviorActionValidator         = agentbuilder.AgentBehaviorActionValidator
	AgentBehaviorActionDispatcher        = agentbuilder.AgentBehaviorActionDispatcher
	RunActionValidator                   = agentrun.RunActionValidator
	RunActionDispatcher                  = agentrun.RunActionDispatcher
	RunbookActionValidator               = routine.RunbookActionValidator
	RunbookActionDispatcher              = routine.RunbookActionDispatcher
	TeamMemberActionValidator            = collaboration.TeamMemberActionValidator
	TeamMemberActionDispatcher           = collaboration.TeamMemberActionDispatcher
	TeamSkillActionValidator             = skillbinding.TeamSkillActionValidator
	TeamSkillAuthorityCatalog            = skillbinding.TeamSkillAuthorityCatalog
	SkillBindingActionValidator          = skillbinding.SkillBindingActionValidator
	SkillBindingActionDispatcher         = skillbinding.SkillBindingActionDispatcher
	OpenClawSkillSource                  = skillopenclaw.Source
	OpenClawSkillFile                    = skillopenclaw.File
	OpenClawSkillBundle                  = skillopenclaw.Bundle
	OpenClawSkillDiagnostic              = skillopenclaw.Diagnostic
	OpenClawSkillCompilation             = skillopenclaw.Compilation
	SkillSourceArtifact                  = sourceartifact.Artifact
	SkillSourceArtifactFile              = sourceartifact.File
	SkillSourceArtifactOrigin            = sourceartifact.Origin
	SkillSourceArtifactReference         = sourceartifact.Reference
	SkillSourceArtifactKey               = sourceartifact.Key
	SkillSourceArtifactStore             = sourceartifact.Store
	SkillSourceArtifactImportRequest     = sourceartifact.ImportOpenClawRequest
	SkillSourceArtifactGCReport          = sourceartifact.GarbageCollectionReport
	SkillSourceRootKind                  = skillsource.RootKind
	SkillSourceRoot                      = skillsource.Root
	SkillSourceCandidate                 = skillsource.Candidate
	SkillSourceShadowed                  = skillsource.ShadowedCandidate
	SkillSourceDiagnostic                = skillsource.Diagnostic
	SkillSourceSnapshot                  = skillsource.Snapshot
	SkillSourceCatalog                   = skillsource.Catalog
	SkillSourceChange                    = skillsource.Change
	SkillSourceWatcher                   = skillsource.Watcher
)

// External conversation aliases are isolated so their intentionally explicit
// names do not reformat the facade's larger compatibility catalog.
type (
	CallbackRegistrationStatus                      = models.CallbackRegistrationStatus
	CallbackAdapterReference                        = models.CallbackAdapterReference
	CallbackSubscription                            = models.CallbackSubscription
	CallbackRegistration                            = models.CallbackRegistration
	CallbackRegistrationFilter                      = models.CallbackRegistrationFilter
	CreateCallbackRegistrationRequest               = callback.CreateCallbackRegistrationRequest
	UpdateCallbackRegistrationRequest               = callback.UpdateCallbackRegistrationRequest
	CallbackRegistrationStore                       = models.CallbackRegistrationStore
	CallbackAdapterResolver                         = callback.CallbackAdapterResolver
	CallbackRegistry                                = callback.CallbackRegistry
	CallbackPublicRequest                           = callback.CallbackPublicRequest
	NormalizedCallbackEvent                         = callback.NormalizedCallbackEvent
	CallbackHostRequest                             = callback.CallbackHostRequest
	CallbackHostResult                              = callback.CallbackHostResult
	CallbackAdapterHost                             = callback.CallbackAdapterHost
	CallbackEventReceiptStatus                      = models.CallbackEventReceiptStatus
	CallbackEventReceipt                            = models.CallbackEventReceipt
	CallbackEventStore                              = models.CallbackEventStore
	CallbackEventConsumer                           = callback.CallbackEventConsumer
	CallbackEventConsumerFunc                       = callback.CallbackEventConsumerFunc
	CallbackIngressResult                           = callback.CallbackIngressResult
	CallbackIngressService                          = callback.CallbackIngressService
	ApprovalCallbackConsumer                        = externalconversation.ApprovalCallbackConsumer
	ExternalConversationEndpointStatus              = models.ExternalConversationEndpointStatus
	ExternalConversationHandlerKind                 = models.ExternalConversationHandlerKind
	ExternalConversationHandler                     = models.ExternalConversationHandler
	ExternalConversationMessageSelection            = models.ExternalConversationMessageSelection
	ExternalConversationReplyMode                   = models.ExternalConversationReplyMode
	ExternalConversationPolicy                      = models.ExternalConversationPolicy
	ExternalConversationAdapterReference            = models.ExternalConversationAdapterReference
	ExternalConversationEndpoint                    = models.ExternalConversationEndpoint
	ExternalConversationEndpointFilter              = models.ExternalConversationEndpointFilter
	CreateExternalConversationEndpointRequest       = externalconversation.CreateExternalConversationEndpointRequest
	UpdateExternalConversationEndpointRequest       = externalconversation.UpdateExternalConversationEndpointRequest
	ExternalConversationEndpointStore               = models.ExternalConversationEndpointStore
	ExternalConversationAdapterResolver             = externalconversation.ExternalConversationAdapterResolver
	ExternalConversationEndpointService             = externalconversation.ExternalConversationEndpointService
	NormalizedExternalConversationEvent             = models.NormalizedExternalConversationEvent
	ExternalConversationInboxStatus                 = models.ExternalConversationInboxStatus
	ExternalConversationInboxItem                   = models.ExternalConversationInboxItem
	ExternalConversationMapping                     = models.ExternalConversationMapping
	ExternalParticipantMapping                      = models.ExternalParticipantMapping
	ExternalMessageDirection                        = models.ExternalMessageDirection
	ExternalMessageMapping                          = models.ExternalMessageMapping
	ExternalConversationDeliveryStatus              = models.ExternalConversationDeliveryStatus
	ExternalConversationDelivery                    = models.ExternalConversationDelivery
	ExternalConversationInboxFilter                 = models.ExternalConversationInboxFilter
	ExternalConversationDeliveryFilter              = models.ExternalConversationDeliveryFilter
	ExternalConversationTransportStore              = models.ExternalConversationTransportStore
	ExternalConversationStore                       = models.ExternalConversationStore
	ReceiveExternalConversationEventRequest         = externalconversation.ReceiveExternalConversationEventRequest
	ReceiveExternalConversationEventResult          = externalconversation.ReceiveExternalConversationEventResult
	EnqueueExternalConversationDeliveryRequest      = externalconversation.EnqueueExternalConversationDeliveryRequest
	EnqueueExternalConversationDeliveryResult       = externalconversation.EnqueueExternalConversationDeliveryResult
	ExternalConversationTransportService            = externalconversation.ExternalConversationTransportService
	ExternalConversationIngressRequest              = externalconversation.ExternalConversationIngressRequest
	ExternalConversationPublicIngressRequest        = externalconversation.ExternalConversationPublicIngressRequest
	ExternalConversationIngressHostResult           = externalconversation.ExternalConversationIngressHostResult
	ExternalConversationIngressHostRequest          = externalconversation.ExternalConversationIngressHostRequest
	ExternalConversationIngressAdapterHost          = externalconversation.ExternalConversationIngressAdapterHost
	ExternalConversationIngressResult               = externalconversation.ExternalConversationIngressResult
	ExternalConversationIngressGateway              = models.ExternalConversationIngressGateway
	ExternalConversationGatewayStatus               = models.ExternalConversationGatewayStatus
	ExternalConversationGatewayRegistration         = models.ExternalConversationGatewayRegistration
	ExternalConversationGatewayFilter               = models.ExternalConversationGatewayFilter
	CreateExternalConversationGatewayRequest        = externalconversation.CreateExternalConversationGatewayRequest
	UpdateExternalConversationGatewayRequest        = externalconversation.UpdateExternalConversationGatewayRequest
	ExternalConversationGatewayStore                = models.ExternalConversationGatewayStore
	ExternalConversationGatewayService              = externalconversation.ExternalConversationGatewayService
	ExternalConversationVerifiedRoute               = models.ExternalConversationVerifiedRoute
	ExternalConversationGatewayEvent                = externalconversation.ExternalConversationGatewayEvent
	ExternalConversationGatewayHostRequest          = externalconversation.ExternalConversationGatewayHostRequest
	ExternalConversationGatewayHostResult           = externalconversation.ExternalConversationGatewayHostResult
	ExternalConversationGatewayAdapterHost          = externalconversation.ExternalConversationGatewayAdapterHost
	ExternalConversationGatewayIngressResult        = externalconversation.ExternalConversationGatewayIngressResult
	ExternalConversationDispatchRequest             = externalconversation.ExternalConversationDispatchRequest
	ExternalConversationDispatchResult              = externalconversation.ExternalConversationDispatchResult
	ExternalConversationDispatcher                  = externalconversation.ExternalConversationDispatcher
	CanonicalExternalConversationDispatcher         = externalconversation.CanonicalExternalConversationDispatcher
	ExternalConversationInboxWorkerConfig           = externalconversation.ExternalConversationInboxWorkerConfig
	CallbackEventWorkerConfig                       = callback.CallbackEventWorkerConfig
	ExternalConversationInboxWorker                 = externalconversation.ExternalConversationInboxWorker
	ExternalConversationDeliveryHostRequest         = externalconversation.ExternalConversationDeliveryHostRequest
	ExternalConversationAcknowledgementStatus       = externalconversation.ExternalConversationAcknowledgementStatus
	ExternalConversationDeliveryAcknowledgement     = externalconversation.ExternalConversationDeliveryAcknowledgement
	ExternalConversationDeliveryOutcome             = externalconversation.ExternalConversationDeliveryOutcome
	ExternalConversationDeliveryHostResult          = externalconversation.ExternalConversationDeliveryHostResult
	ExternalConversationAdapterHost                 = externalconversation.ExternalConversationAdapterHost
	ExternalConversationDestination                 = externalconversation.ExternalConversationDestination
	ExternalConversationDestinationPage             = externalconversation.ExternalConversationDestinationPage
	ExternalConversationDestinationDiscoveryRequest = externalconversation.ExternalConversationDestinationDiscoveryRequest
	ExternalConversationDestinationDiscoveryHost    = externalconversation.ExternalConversationDestinationDiscoveryHost
	ExternalConversationDeliveryWorkerConfig        = externalconversation.ExternalConversationDeliveryWorkerConfig
	ExternalConversationDeliveryWorker              = externalconversation.ExternalConversationDeliveryWorker
	ExternalConversationReplyStore                  = models.ExternalConversationReplyStore
	ExternalConversationReplyWorker                 = externalconversation.ExternalConversationReplyWorker
	RunProgressSnapshot                             = agentrun.RunProgressSnapshot
	RunProgressAcknowledgementRequest               = agentrun.RunProgressAcknowledgementRequest
	RunProgressAcknowledgementRenderer              = agentrun.RunProgressAcknowledgementRenderer
	RunProgressAcknowledgementRendererFunc          = externalconversation.RunProgressAcknowledgementRendererFunc
	RunProgressAcknowledgement                      = externalconversation.RunProgressAcknowledgement
	RunProgressAcknowledgementStore                 = models.RunProgressAcknowledgementStore
	RunProgressAcknowledgementWorkerConfig          = externalconversation.RunProgressAcknowledgementWorkerConfig
	RunProgressAcknowledgementWorker                = externalconversation.RunProgressAcknowledgementWorker
	TurnRunnerProgressAcknowledgementRenderer       = externalconversation.TurnRunnerProgressAcknowledgementRenderer
	ExternalConversationSupervisor                  = externalconversation.ExternalConversationSupervisor
)

// ExternalConversationGatewayVerificationAdapterHost opts in to handling
// provider setup challenges while a saved gateway remains paused.
type ExternalConversationGatewayVerificationAdapterHost = externalconversation.ExternalConversationGatewayVerificationAdapterHost

// Credential lease aliases are kept in their own group so extending the
// security protocol does not reformat the facade's much larger alias catalog.
type (
	ActionCredentialLeaseIssuer            = action.ActionCredentialLeaseIssuer
	ActionCredentialLeaseIssuerFunc        = action.ActionCredentialLeaseIssuerFunc
	ActionCredentialLeaseIssueRequest      = action.ActionCredentialLeaseIssueRequest
	ActionLeaseIdentity                    = models.ActionLeaseIdentity
	ActionCredentialFieldReference         = models.ActionCredentialFieldReference
	ActionCredentialLease                  = models.ActionCredentialLease
	ActionCredentialLeaseSignature         = models.ActionCredentialLeaseSignature
	SignedActionCredentialLease            = models.SignedActionCredentialLease
	CreateActionCredentialLeaseRequest     = models.CreateActionCredentialLeaseRequest
	ActionCredentialLeaseSigner            = action.ActionCredentialLeaseSigner
	ActionCredentialLeaseSignatureVerifier = action.ActionCredentialLeaseSignatureVerifier
	ActionCredentialLeaseValidationRequest = action.ActionCredentialLeaseValidationRequest
	ActionCredentialLeaseRedemptionRequest = models.ActionCredentialLeaseRedemptionRequest
	ActionCredentialLeaseRedeemer          = models.ActionCredentialLeaseRedeemer
	ActionCredentialLeaseValidator         = action.ActionCredentialLeaseValidator
)

type (
	ConversationTaskMode                   = models.ConversationTaskMode
	ConversationTaskProofStore             = models.ConversationTaskProofStore
	ConversationTaskPromotionStore         = models.ConversationTaskPromotionStore
	ConversationTaskPromotionRequest       = models.ConversationTaskPromotionRequest
	ConversationTaskDueStore               = models.ConversationTaskDueStore
	ConversationTaskDueFilter              = models.ConversationTaskDueFilter
	ConversationTaskContinuationStore      = models.ConversationTaskContinuationStore
	ConversationTaskContinuationReconciler = conversationrun.ConversationTaskContinuationReconciler
)

var (
	NewConversationTaskContinuationReconciler = conversationrun.NewConversationTaskContinuationReconciler
	ConversationTaskMatchesWorkRun            = models.ConversationTaskMatchesWorkRun
	VerifyConversationTaskWorkRun             = agentrun.VerifyConversationTaskWorkRun
	ResolveTeamHandoffRequest                 = conversation.ResolveTeamHandoffRequest
	ConversationTaskAcknowledgmentMessageID   = models.ConversationTaskAcknowledgmentMessageID
	FindConversationTaskResultMessage         = conversationrun.FindConversationTaskResultMessage
)

const (
	ConversationTaskContextKey          = models.ConversationTaskContextKey
	ConversationTaskModeIndependent     = models.ConversationTaskModeIndependent
	ConversationTaskModeContinuation    = models.ConversationTaskModeContinuation
	ConversationTaskForegroundTimeout   = models.ConversationTaskForegroundTimeout
	AgentManifestAPIVersion             = kernelagent.ManifestAPIVersion
	AgentManifestKind                   = kernelagent.ManifestKind
	AgentBundleAPIVersion               = kernelagent.BundleAPIVersion
	AgentBundleKind                     = kernelagent.BundleKind
	AgentBundleRequirementRuntime       = kernelagent.BundleRequirementRuntime
	AgentBundleRequirementSkill         = kernelagent.BundleRequirementSkill
	AgentBundleRequirementCredential    = kernelagent.BundleRequirementCredential
	AgentBundleRequirementEndpoint      = kernelagent.BundleRequirementEndpoint
	AgentBundleRequirementCallback      = kernelagent.BundleRequirementCallback
	AgentBundleRequirementDeployment    = kernelagent.BundleRequirementDeployment
	WorkforceBundleAPIVersion           = kernelbundle.APIVersion
	WorkforceBundleKind                 = kernelbundle.Kind
	WorkforceBundleOwnerAgent           = kernelbundle.OwnerAgent
	WorkforceBundleOwnerTeam            = kernelbundle.OwnerTeam
	WorkforceBundleChangeAdded          = kernelbundle.ChangeAdded
	WorkforceBundleChangeRemoved        = kernelbundle.ChangeRemoved
	WorkforceBundleChangeModified       = kernelbundle.ChangeModified
	TeamManifestAPIVersion              = kernelteam.ManifestAPIVersion
	TeamManifestKind                    = kernelteam.ManifestKind
	SkillSourceArtifactFormatOpenClawV1 = sourceartifact.FormatOpenClawSkillV1
	ActionCredentialLeaseVersion        = models.ActionCredentialLeaseVersion
	MaximumActionCredentialLeaseTTL     = models.MaximumActionCredentialLeaseTTL
	MaximumConversationPlaces           = conversation.MaximumConversationPlaces
)

func CompileAgentManifest(manifest *AgentManifest, definitionID string, provenance AgentDefinitionProvenance) (*AgentDefinition, error) {
	return kernelagent.CompileManifest(manifest, definitionID, provenance)
}

func DecodeAgentManifestYAML(data []byte) (*AgentManifest, error) {
	return kernelagent.DecodeManifestYAML(data)
}

func ExportAgentManifest(request AgentManifestExportRequest) (*AgentManifest, error) {
	return kernelagent.ExportManifest(request)
}

func EncodeAgentManifestYAML(manifest *AgentManifest) ([]byte, error) {
	return kernelagent.EncodeManifestYAML(manifest)
}

func ExportAgentBundle(request AgentBundleExportRequest) (*AgentBundle, error) {
	return kernelagent.ExportBundle(request)
}

func EncodeAgentBundleYAML(bundle *AgentBundle) ([]byte, error) {
	return kernelagent.EncodeBundleYAML(bundle)
}

func DecodeAgentBundleYAML(data []byte) (*AgentBundle, error) {
	return kernelagent.DecodeBundleYAML(data)
}

func PreviewAgentBundleInstallation(bundle *AgentBundle, placement AgentBundlePlacement) (*AgentBundleInstallationPreview, error) {
	return kernelagent.PreviewBundleInstallation(bundle, placement)
}

func CompileAgentBundleInstallation(request AgentBundleInstallationRequest) (*AgentBundleInstallationPlan, error) {
	return kernelagent.CompileBundleInstallation(request)
}

func NewWorkforceBundle(metadata WorkforceBundleMetadata) *WorkforceBundle {
	return kernelbundle.New(metadata)
}

func ExportWorkforceBundle(request WorkforceBundleExportRequest) (*WorkforceBundle, error) {
	return kernelbundle.Export(request)
}

func EncodeWorkforceBundleYAML(bundle *WorkforceBundle) ([]byte, error) {
	return kernelbundle.EncodeYAML(bundle)
}

func DecodeWorkforceBundleYAML(data []byte) (*WorkforceBundle, error) {
	return kernelbundle.DecodeYAML(data)
}

func SignWorkforceBundle(bundle *WorkforceBundle, keyID string, privateKey ed25519.PrivateKey) error {
	return kernelbundle.Sign(bundle, keyID, privateKey)
}

func VerifyWorkforceBundle(bundle *WorkforceBundle, policy WorkforceBundleTrustPolicy) (*WorkforceBundleVerification, error) {
	return kernelbundle.Verify(bundle, policy)
}

func InspectWorkforceBundle(bundle *WorkforceBundle) (*WorkforceBundleInspection, error) {
	return kernelbundle.Inspect(bundle)
}

func CompareWorkforceBundles(from, to *WorkforceBundle) (*WorkforceBundleDiff, error) {
	return kernelbundle.Compare(from, to)
}

func PlanWorkforceBundleUpgrade(current, target *WorkforceBundle) (*WorkforceBundleUpgradePlan, error) {
	return kernelbundle.PlanUpgrade(current, target)
}

func PreviewWorkforceBundleInstallation(bundle *WorkforceBundle, placement WorkforceBundlePlacement) (*WorkforceBundleInstallationPreview, error) {
	return kernelbundle.PreviewInstallation(bundle, placement)
}

func CompileWorkforceBundleInstallation(request WorkforceBundleInstallationRequest) (*WorkforceBundleInstallationPlan, error) {
	return kernelbundle.CompileInstallation(request)
}

func InstallWorkforceBundle(ctx context.Context, store WorkforceBundleInstallationStore, request WorkforceBundleInstallationRequest) (*WorkforceBundleInstallationReceipt, error) {
	return kernelbundle.Install(ctx, store, request)
}

func InstallAgentManifest(ctx context.Context, registry AgentManifestInstallationRegistry, request AgentManifestInstallationRequest) (*AgentManifestInstallationResult, error) {
	return kernelagent.InstallManifest(ctx, registry, request)
}

func CompileTeamManifest(manifest *TeamManifest, definitionID string, provenance WorkforceDefinitionProvenance) (*TeamDefinition, error) {
	return kernelteam.CompileManifest(manifest, definitionID, provenance)
}

// TeamManifestRoster returns the manifest's members as roster assignments;
// agents maps each member id to its placed Agent deployment.
func TeamManifestRoster(manifest *TeamManifest, agents map[string]string) ([]TeamRosterAssignment, error) {
	return kernelteam.ManifestRoster(manifest, agents)
}

// TeamHasSpeakingMember reports whether any roster member may answer in the
// Team channel.
func TeamHasSpeakingMember(deployment *TeamDeployment) bool {
	return kernelteam.HasSpeakingMember(deployment)
}

func DecodeTeamManifestYAML(data []byte) (*TeamManifest, error) {
	return kernelteam.DecodeManifestYAML(data)
}

func NewSkillIdentity(id, version, sourceIdentity string) SkillIdentity {
	return capability.NewSkillIdentity(id, version, sourceIdentity)
}

type (
	KernelCapability                    = kernelapi.Capability
	KernelCapabilityDocument            = kernelapi.CapabilityDocument
	KernelCapabilityContext             = kernelapi.CapabilityContext
	KernelCapabilityBlockingRequirement = kernelapi.CapabilityBlockingRequirement
	ConversationGatewayAdapterChoice    = kernelapi.ConversationGatewayAdapterChoice
	CallbackAdapterChoice               = kernelapi.CallbackAdapterChoice
	CredentialBindingChoice             = capability.CredentialBindingChoice
	CredentialExternalIdentity          = capability.CredentialExternalIdentity
	KernelSkillActionList               = kernelapi.SkillActionList
	KernelSkillBindingList              = kernelapi.SkillBindingList
	KernelSkillBindingMutationResult    = kernelapi.SkillBindingMutationResult
	ReconcileRunbookSchedulesRequest    = kernelapi.ReconcileRunbookSchedulesRequest
	RunbookScheduleReconciliation       = kernelapi.RunbookScheduleReconciliation
	KernelTeamDeploymentList            = kernelapi.TeamDeploymentList
	KernelTeamDeploymentCatalogEntry    = kernelapi.TeamDeploymentCatalogEntry
	ChannelCapabilityFeatures           = kernelapi.ChannelCapabilityFeatures
	AgentDefinitionCapabilityFeatures   = kernelapi.AgentDefinitionCapabilityFeatures
	TeamDefinitionCapabilityFeatures    = kernelapi.TeamDefinitionCapabilityFeatures
	ActionApprovalCapabilityFeatures    = kernelapi.ActionApprovalCapabilityFeatures
)

const (
	DeploymentChangeConfigurationUpdated    = workforce.DeploymentChangeConfigurationUpdated
	TurnHostContextExceeded                 = modelstep.TurnHostContextExceeded
	KernelAPIVersion                        = kernelapi.APIVersion
	ChannelsCapabilityID                    = kernelapi.ChannelsCapabilityID
	ChannelsCapabilityVersion               = kernelapi.ChannelsCapabilityVersion
	ActivityCapabilityID                    = kernelapi.ActivityCapabilityID
	ActivityCapabilityVersion               = kernelapi.ActivityCapabilityVersion
	ActionCallsCapabilityID                 = kernelapi.ActionCallsCapabilityID
	ActionCallsCapabilityVersion            = kernelapi.ActionCallsCapabilityVersion
	AgentTurnsCapabilityID                  = kernelapi.AgentTurnsCapabilityID
	AgentTurnsCapabilityVersion             = kernelapi.AgentTurnsCapabilityVersion
	SkillActionsCapabilityID                = kernelapi.SkillActionsCapabilityID
	SkillActionsCapabilityVersion           = kernelapi.SkillActionsCapabilityVersion
	SkillBindingsCapabilityID               = kernelapi.SkillBindingsCapabilityID
	SkillBindingsCapabilityVersion          = kernelapi.SkillBindingsCapabilityVersion
	AgentEmbedsCapabilityID                 = kernelapi.AgentEmbedsCapabilityID
	AgentEmbedsCapabilityVersion            = kernelapi.AgentEmbedsCapabilityVersion
	RunbooksCapabilityID                    = kernelapi.RunbooksCapabilityID
	RunbooksCapabilityVersion               = kernelapi.RunbooksCapabilityVersion
	KernelOperationCreate                   = kernelapi.OperationCreate
	KernelOperationExecute                  = kernelapi.OperationExecute
	KernelOperationList                     = kernelapi.OperationList
	KernelOperationGet                      = kernelapi.OperationGet
	KernelOperationUpdate                   = kernelapi.OperationUpdate
	KernelOperationRegister                 = kernelapi.OperationRegister
	KernelOperationPause                    = kernelapi.OperationPause
	KernelOperationResume                   = kernelapi.OperationResume
	KernelOperationRetire                   = kernelapi.OperationRetire
	KernelOperationListCompilations         = kernelapi.OperationListCompilations
	KernelOperationDeploy                   = kernelapi.OperationDeploy
	KernelOperationActivate                 = kernelapi.OperationActivate
	KernelOperationRollback                 = kernelapi.OperationRollback
	KernelOperationListActivations          = kernelapi.OperationListActivations
	KernelOperationProposeAmendment         = kernelapi.OperationProposeAmendment
	KernelOperationListAmendments           = kernelapi.OperationListAmendments
	KernelOperationGetAmendment             = kernelapi.OperationGetAmendment
	KernelOperationEvaluateAmendment        = kernelapi.OperationEvaluateAmendment
	KernelOperationResolveAmendment         = kernelapi.OperationResolveAmendment
	KernelOperationActivateAmendment        = kernelapi.OperationActivateAmendment
	KernelOperationRecoverParticipation     = kernelapi.OperationRecoverParticipation
	KernelOperationReconcile                = kernelapi.OperationReconcile
	KernelOperationUpsert                   = kernelapi.OperationUpsert
	KernelOperationDisable                  = kernelapi.OperationDisable
	KernelOperationRevoke                   = kernelapi.OperationRevoke
	KernelOperationListVersions             = kernelapi.OperationListVersions
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
	KernelOperationSearch                   = kernelapi.OperationSearch
	HostedSkillApplied                      = models.HostedSkillApplied
	HostedSkillNotApplied                   = models.HostedSkillNotApplied
	RunbookAPIVersion                       = runbook.APIVersion
	RunbookTriggerEvent                     = runbook.TriggerEvent
	RunbookTriggerSchedule                  = runbook.TriggerSchedule
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

const (
	ConversationGatewaysCapabilityID      = kernelapi.ConversationGatewaysCapabilityID
	ConversationGatewaysCapabilityVersion = kernelapi.ConversationGatewaysCapabilityVersion
	SpacesCapabilityID                    = kernelapi.SpacesCapabilityID
	CallbacksCapabilityID                 = kernelapi.CallbacksCapabilityID
	CallbacksCapabilityVersion            = kernelapi.CallbacksCapabilityVersion
)

// ChannelCapability returns the canonical versioned descriptor for the
// channel services an embedding host has actually wired.
func ChannelCapability(features ChannelCapabilityFeatures) KernelCapability {
	return kernelapi.ChannelsCapability(features)
}

func AgentRunsCapability() KernelCapability {
	return kernelapi.AgentRunsCapability()
}

func WorkspaceSearchCapability() KernelCapability {
	return kernelapi.WorkspaceSearchCapability()
}

func ObjectivesCapability() KernelCapability {
	return kernelapi.ObjectivesCapability()
}

func RunbooksCapability() KernelCapability {
	return kernelapi.RunbooksCapability()
}

func ConversationGatewaysCapability(management bool, choices ...[]ConversationGatewayAdapterChoice) KernelCapability {
	return kernelapi.ConversationGatewaysCapability(management, choices...)
}

func CallbacksCapability(management bool, choices ...[]CallbackAdapterChoice) KernelCapability {
	return kernelapi.CallbacksCapability(management, choices...)
}

func SkillActionsCapability() KernelCapability {
	return kernelapi.SkillActionsCapability()
}

func SkillBindingsCapability(management bool) KernelCapability {
	return kernelapi.SkillBindingsCapability(management)
}

func AgentEmbedsCapability(management bool) KernelCapability {
	return kernelapi.AgentEmbedsCapability(management)
}

func SpacesCapability() KernelCapability {
	return kernelapi.SpacesCapability()
}

func ActivityCapability() KernelCapability {
	return kernelapi.ActivityCapability()
}

func ActionCallsCapability() KernelCapability {
	return kernelapi.ActionCallsCapability()
}

func AgentTurnsCapability() KernelCapability {
	return kernelapi.AgentTurnsCapability()
}

func ProjectAgentTurn(turn *AgentTurn) *AgentTurnRecord {
	return kernelapi.ProjectAgentTurn(turn)
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

func WorkforceExecutionTargetsCapability(management bool) KernelCapability {
	return kernelapi.WorkforceExecutionTargetsCapability(management)
}

func WorkforceBundlesCapability(installation bool) KernelCapability {
	return kernelapi.WorkforceBundlesCapability(installation)
}

// NewKernelCapabilityDocument composes the exact capability envelope consumed
// by OpenSeal's TUI and embedding-host user interfaces.
func NewKernelCapabilityDocument(capabilities ...KernelCapability) KernelCapabilityDocument {
	return kernelapi.NewCapabilityDocument(capabilities...)
}

func CanonicalConversationTriggerInputSchema() map[string]interface{} {
	return skillcatalog.CanonicalConversationTriggerInputSchema()
}

func ValidateWorkforceCapabilityCatalog(catalog WorkforceCapabilityCatalog) error {
	return skillcatalog.ValidateCapabilityCatalog(catalog)
}

func WorkforceHostedSkillModelInputTokenCeiling(definition SkillDefinition) (int64, error) {
	return capability.HostedSkillModelInputTokenCeiling(definition)
}

var (
	NormalizeOAuth2Requirement   = capability.NormalizeOAuth2Requirement
	NormalizeOAuth2GrantSummary  = capability.NormalizeOAuth2GrantSummary
	OAuth2GrantSatisfies         = capability.OAuth2GrantSatisfies
	NormalizeConversationAdapter = capability.NormalizeConversationAdapter
	NormalizeCallbackAdapter     = capability.NormalizeCallbackAdapter
)

var (
	ConversationTaskGoalSummary         = conversationrun.ConversationTaskGoalSummary
	PersistedTaskOrigin                 = agentrun.PersistedTaskOrigin
	NewConversationTaskService          = conversationrun.NewConversationTaskService
	NewTurnHostFailure                  = modelstep.NewTurnHostFailure
	TerminalFailureReply                = conversation.TerminalFailureReply
	ErrInvalidApprovalReviewContext     = action.ErrInvalidApprovalReviewContext
	NewCapabilityInvocationStepProducer = modelstep.NewCapabilityInvocationStepProducer
	ResolveLoopBinding                  = runworker.ResolveLoopBinding
	NewArtifactRetentionService         = artifactcatalog.NewArtifactRetentionService
	ErrInvalidConversationTask          = models.ErrInvalidConversationTask
	ErrConversationTaskNotFound         = agentrun.ErrConversationTaskNotFound
	ErrConversationTaskConflict         = models.ErrConversationTaskConflict
	ErrTurnHostUnavailable              = modelstep.ErrTurnHostUnavailable
	ErrTurnHostConfiguration            = modelstep.ErrTurnHostConfiguration
	ValidateRunbook                     = runbook.Validate
	VerifyRunbook                       = runbook.Verify
	ValidateRunbookInterfaceInput       = runbook.ValidateInterfaceInput
	NewRunbookStepProducer              = routine.NewRunbookStepProducer
	NewRunForkCoordinator               = agentrun.NewRunForkCoordinator
	ObjectiveManagementSkill            = action.ObjectiveManagementSkill
	NewObjectiveActionValidator         = action.NewObjectiveActionValidator
	NewObjectiveActionDispatcher        = action.NewObjectiveActionDispatcher
	AgentManagementSkill                = models.AgentManagementSkill
	RunManagementSkill                  = agentrun.RunManagementSkill
	NewAgentBehaviorActionValidator     = agentbuilder.NewAgentBehaviorActionValidator
	NewAgentBehaviorActionDispatcher    = agentbuilder.NewAgentBehaviorActionDispatcher
	NewRunActionValidator               = agentrun.NewRunActionValidator
	NewRunActionDispatcher              = agentrun.NewRunActionDispatcher
	RunbookManagementSkill              = routine.RunbookManagementSkill
	PinRunCommand                       = workspace.PinRunCommand
	RecordPinnedCommand                 = workspace.RecordPinnedCommand
	ExpectedDigestOf                    = workspace.ExpectedDigestOf
	ResolveRoutineScriptStatus          = routine.ResolveRoutineScriptStatus
	NewRunbookActionValidator           = routine.NewRunbookActionValidator
	NewRunbookActionDispatcher          = routine.NewRunbookActionDispatcher
	TeamManagementSkill                 = collaboration.TeamManagementSkill
	NewTeamMemberActionValidator        = collaboration.NewTeamMemberActionValidator
	NewTeamMemberActionDispatcher       = collaboration.NewTeamMemberActionDispatcher
	NewTeamSkillActionValidator         = skillbinding.NewTeamSkillActionValidator
	AuthorizeTeamSkillActivation        = skillbinding.AuthorizeTeamSkillActivation
	SkillManagementSkill                = skillbinding.SkillManagementSkill
	NewSkillBindingActionValidator      = skillbinding.NewSkillBindingActionValidator
	NewSkillBindingActionDispatcher     = skillbinding.NewSkillBindingActionDispatcher
	NewHTTPActionExecutor               = httpaction.NewExecutor
	DecodeHTTPActionInvocation          = httpaction.DecodeInvocation
)

var NewExternalConversationEndpointService = externalconversation.NewExternalConversationEndpointService
var NewCallbackRegistry = callback.NewCallbackRegistry
var NewCallbackIngressService = callback.NewCallbackIngressService
var NewApprovalCallbackConsumer = externalconversation.NewApprovalCallbackConsumer
var ConversationDestinationDiscoveryArguments = externalconversation.ConversationDestinationDiscoveryArguments
var ProjectConversationDestinationPage = externalconversation.ProjectConversationDestinationPage
var NewExternalConversationGatewayService = externalconversation.NewExternalConversationGatewayService
var NewExternalConversationTransportService = externalconversation.NewExternalConversationTransportService
var NewCanonicalExternalConversationDispatcher = externalconversation.NewCanonicalExternalConversationDispatcher
var NewExternalConversationInboxWorker = externalconversation.NewExternalConversationInboxWorker
var NewExternalConversationDeliveryWorker = externalconversation.NewExternalConversationDeliveryWorker
var NewExternalConversationReplyWorker = externalconversation.NewExternalConversationReplyWorker
var NewRunProgressAcknowledgementWorker = externalconversation.NewRunProgressAcknowledgementWorker
var NewTurnRunnerProgressAcknowledgementRenderer = externalconversation.NewTurnRunnerProgressAcknowledgementRenderer
var NewExternalConversationSupervisor = externalconversation.NewExternalConversationSupervisor
var NewOAuthService = kerneloauth.NewService
var NewOAuthMemoryStore = kerneloauth.NewMemoryStore
var OAuthSessionIDFromState = kerneloauth.SessionIDFromState

var (
	ErrOAuthInvalid            = kerneloauth.ErrInvalid
	ErrOAuthSessionNotFound    = kerneloauth.ErrSessionNotFound
	ErrOAuthSessionConsumed    = kerneloauth.ErrSessionConsumed
	ErrOAuthSessionExpired     = kerneloauth.ErrSessionExpired
	ErrOAuthStateMismatch      = kerneloauth.ErrStateMismatch
	ErrOAuthScopeMismatch      = kerneloauth.ErrScopeMismatch
	ErrOAuthRevisionConflict   = kerneloauth.ErrRevisionConflict
	ErrOAuthConnectionNotFound = kerneloauth.ErrConnectionNotFound
	ErrOAuthRefreshInProgress  = kerneloauth.ErrRefreshInProgress
	ErrOAuthUnsupported        = kerneloauth.ErrUnsupported
	ErrOAuthGrantRevoked       = kerneloauth.ErrGrantRevoked
)

// OAuthRefreshRevokedErrorCode marks a degraded connection whose grant the
// provider revoked: it needs a new sign-in, not a retry.
const OAuthRefreshRevokedErrorCode = kerneloauth.RefreshRevokedErrorCode

// PersistentKernelStore is the complete durable control-plane contract for an
// embedded OpenSeal engine. Enterprise adapters implement this interface so
// runtime state, agent definitions/deployments, and skill bindings share one
// authoritative persistence boundary.
type PersistentKernelStore interface {
	models.KernelStore
	models.CollaborationStore
	models.RunDependencyStore
	models.ConversationStore
	models.ArtifactStore
	models.SkillReferenceUpgradeStore
	kernelagent.Store
	kernelteam.Store
	skill.CatalogStore
	sourceartifact.Store
}

var _ PersistentKernelStore = (*postgres.Store)(nil)

var (
	ErrRunNotFound                          = models.ErrRunNotFound
	ErrRevisionConflict                     = models.ErrRevisionConflict
	ErrInvalidRunTransition                 = models.ErrInvalidRunTransition
	ErrRunIdempotency                       = models.ErrRunIdempotency
	ErrInvalidAgentRun                      = models.ErrInvalidAgentRun
	ErrInvalidRunCommand                    = portfolio.ErrInvalidRunCommand
	ErrInvalidScope                         = models.ErrInvalidScope
	ErrInvalidOwner                         = models.ErrInvalidOwner
	ErrObjectiveNotFound                    = models.ErrObjectiveNotFound
	ErrObjectiveIdempotency                 = models.ErrObjectiveIdempotency
	ErrInvalidObjectiveTransition           = portfolio.ErrInvalidObjectiveTransition
	ErrRunbookActivationNotFound            = models.ErrRunbookActivationNotFound
	ErrRoutineGrantsNotOwner                = routine.ErrRoutineGrantsNotOwner
	ErrRoutineHasNoScript                   = routine.ErrRoutineHasNoScript
	ErrChatFileNotFound                     = routine.ErrChatFileNotFound
	ErrRunbookDefinitionNotFound            = routine.ErrRunbookDefinitionNotFound
	ErrRunbookActivationInactive            = routine.ErrRunbookActivationInactive
	ErrRunbookActivationRevision            = models.ErrRunbookActivationRevision
	ErrRunbookActivationIdempotency         = models.ErrRunbookActivationIdempotency
	ErrBudgetExhausted                      = models.ErrBudgetExhausted
	ErrTurnNotFound                         = models.ErrTurnNotFound
	ErrActionNotFound                       = models.ErrActionNotFound
	ErrApprovalNotFound                     = models.ErrApprovalNotFound
	ErrApprovalResolved                     = models.ErrApprovalResolved
	ErrActionIdempotencyConflict            = models.ErrIdempotencyConflict
	ErrActionCredentialLeaseInvalid         = models.ErrActionCredentialLeaseInvalid
	ErrActionCredentialLeaseExpired         = models.ErrActionCredentialLeaseExpired
	ErrActionCredentialLeaseMismatch        = models.ErrActionCredentialLeaseMismatch
	ErrActionCredentialLeaseReplay          = models.ErrActionCredentialLeaseReplay
	ErrAgentRequestNotFound                 = models.ErrAgentRequestNotFound
	ErrInvalidAgentRequestState             = models.ErrInvalidAgentRequestState
	ErrAgentRequestUnauthorized             = collaboration.ErrAgentRequestUnauthorized
	ErrAgentRequestAssignment               = models.ErrAgentRequestAssignment
	ErrAgentRequestIdempotency              = models.ErrAgentRequestIdempotency
	ErrUnsafeSharedContext                  = models.ErrUnsafeSharedContext
	ErrInvalidArtifact                      = models.ErrInvalidArtifact
	ErrArtifactNotFound                     = artifactcatalog.ErrArtifactNotFound
	ErrArtifactImmutable                    = models.ErrArtifactImmutable
	ErrArtifactVersionConflict              = models.ErrArtifactVersionConflict
	ErrInvalidArtifactRecord                = models.ErrInvalidArtifactRecord
	ErrRunDependencyNotFound                = models.ErrRunDependencyNotFound
	ErrDependencyGroupNotFound              = models.ErrDependencyGroupNotFound
	ErrInvalidRunDependency                 = models.ErrInvalidRunDependency
	ErrDependencyConflict                   = models.ErrDependencyConflict
	ErrConversationNotFound                 = models.ErrConversationNotFound
	ErrSpaceNotFound                        = models.ErrSpaceNotFound
	ErrInvalidSpace                         = models.ErrInvalidSpace
	ErrDefaultSpaceProtected                = models.ErrDefaultSpaceProtected
	ErrNoDefaultSpace                       = models.ErrNoDefaultSpace
	ErrTeamAgentOutsideSpace                = models.ErrTeamAgentOutsideSpace
	ErrChannelMessageNotFound               = models.ErrChannelMessageNotFound
	ErrParticipationRoundNotFound           = models.ErrParticipationRoundNotFound
	ErrInvalidConversation                  = models.ErrInvalidConversation
	ErrMessageConflict                      = models.ErrMessageConflict
	ErrConversationCursorConflict           = models.ErrConversationCursorConflict
	ErrConversationPresenceConflict         = models.ErrConversationPresenceConflict
	ErrConversationCoordinationUnavailable  = conversation.ErrConversationCoordinationUnavailable
	ErrNoConversationParticipants           = conversation.ErrNoConversationParticipants
	ErrAgentDefinitionNotFound              = kernelagent.ErrDefinitionNotFound
	ErrAgentDeploymentNotFound              = kernelagent.ErrDeploymentNotFound
	ErrAgentAmendmentNotFound               = kernelagent.ErrAmendmentNotFound
	ErrAgentDeploymentRevisionConflict      = kernelagent.ErrRevisionConflict
	ErrAgentManifestInstallationConflict    = kernelagent.ErrManifestInstallationConflict
	ErrTeamDefinitionNotFound               = kernelteam.ErrDefinitionNotFound
	ErrTeamDeploymentNotFound               = kernelteam.ErrDeploymentNotFound
	ErrDeploymentIDTaken                    = workforce.ErrDeploymentIDTaken
	ErrDeploymentIDNotReserved              = workforce.ErrDeploymentIDNotReserved
	ErrTeamDeploymentRevisionConflict       = kernelteam.ErrRevisionConflict
	ErrTeamAmendmentNotFound                = kernelteam.ErrAmendmentNotFound
	ErrTeamIdempotencyConflict              = kernelteam.ErrIdempotencyConflict
	ErrTeamParticipationRecoveryNotRequired = kernelteam.ErrParticipationRecoveryNotRequired
	ErrTeamRosterMinimum                    = kernelteam.ErrRosterMinimum
)

func NewToolActionDispatcher(invoker modelstep.ToolInvoker) (*modelstep.ToolActionDispatcher, error) {
	return modelstep.NewToolActionDispatcher(invoker)
}

func NewActionCredentialLease(request CreateActionCredentialLeaseRequest) (*ActionCredentialLease, error) {
	return models.NewActionCredentialLease(request)
}

func SignActionCredentialLease(ctx context.Context, lease ActionCredentialLease, signer ActionCredentialLeaseSigner) (*SignedActionCredentialLease, error) {
	return action.SignActionCredentialLease(ctx, lease, signer)
}

func NewActionCredentialLeaseValidator(verifier ActionCredentialLeaseSignatureVerifier, redeemer ActionCredentialLeaseRedeemer) (*ActionCredentialLeaseValidator, error) {
	return action.NewActionCredentialLeaseValidator(verifier, redeemer)
}

func MatchActionCredentialLease(lease ActionCredentialLease, call *ActionCall, run *AgentRun, transport string, credentialFields map[string][]string) error {
	return models.MatchActionCredentialLease(lease, call, run, transport, credentialFields)
}

func MatchActionCredentialLeaseReferences(envelope *SignedActionCredentialLease, call *ActionCall, run *AgentRun, transport string) error {
	return action.MatchActionCredentialLeaseReferences(envelope, call, run, transport)
}

func ValidateCredentialFreeContext(value interface{}) error {
	return models.ValidateCredentialFreeContext(value)
}

func EvaluateBudget(policy BudgetPolicy, usage BudgetUsage) (BudgetState, []string, error) {
	return models.EvaluateBudget(policy, usage)
}

func DefaultConversationCoordinatorConfig() conversation.ConversationCoordinatorConfig {
	return conversation.DefaultConversationCoordinatorConfig()
}

// NewPostgresStore opens the production-grade shared persistence adapter and
// applies OpenSeal's versioned schema migrations.
func NewPostgresStore(ctx context.Context, dsn string, options ...postgres.Option) (*postgres.Store, error) {
	return postgres.New(ctx, dsn, options...)
}

type PostgresMigrationEvent = postgres.MigrationEvent
type PostgresMigrationPhase = postgres.MigrationPhase
type PostgresMigrationStats = postgres.MigrationStats

const (
	PostgresMigrationWaiting  = postgres.MigrationWaiting
	PostgresMigrationAcquired = postgres.MigrationAcquired
	PostgresMigrationCurrent  = postgres.MigrationCurrent
	PostgresMigrationComplete = postgres.MigrationComplete
	PostgresMigrationTimeout  = postgres.MigrationTimeout
)

var ErrPostgresMigrationLockTimeout = postgres.ErrMigrationLockTimeout

// NewSQLiteStore opens the standalone durable kernel store.
func NewSQLiteStore(path string) (*sqlite.Store, error) {
	return sqlite.New(path)
}

func WithPostgresSchema(schema string) postgres.Option {
	return postgres.WithSchema(schema)
}

func DefaultPostgresPoolConfig() postgres.PoolConfig {
	return postgres.DefaultPoolConfig()
}

func WithPostgresPool(pool postgres.PoolConfig) postgres.Option {
	return postgres.WithPool(pool)
}

func WithPostgresMigrationLock(timeout, pollInterval time.Duration) postgres.Option {
	return postgres.WithMigrationLock(timeout, pollInterval)
}

func WithPostgresMigrationObserver(observer postgres.MigrationObserver) postgres.Option {
	return postgres.WithMigrationObserver(observer)
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
	WorkforceCatalogDiagnosticExecutionTargetMissing = skillcatalog.CatalogDiagnosticExecutionTargetMissing
	WorkforceSkillReady                              = skillcatalog.SkillReadinessReady
	WorkforceSkillNeedsBinding                       = skillcatalog.SkillReadinessNeedsBinding
	WorkforceSkillNeedsInstallation                  = skillcatalog.SkillReadinessNeedsInstallation
	WorkforceSkillUnavailable                        = skillcatalog.SkillReadinessUnavailable

	OwnerTypeAgent = models.OwnerTypeAgent
	OwnerTypeTeam  = models.OwnerTypeTeam

	ObjectiveStatusDraft            = models.ObjectiveStatusDraft
	ObjectiveStatusActive           = models.ObjectiveStatusActive
	ObjectiveStatusPaused           = models.ObjectiveStatusPaused
	ObjectiveStatusSatisfied        = models.ObjectiveStatusSatisfied
	ObjectiveStatusFailed           = models.ObjectiveStatusFailed
	ObjectiveStatusRetired          = models.ObjectiveStatusRetired
	ObjectiveManagementSkillID      = models.ObjectiveManagementSkillID
	ObjectiveManagementSkillVersion = action.ObjectiveManagementSkillVersion
	ObjectiveActionCreate           = action.ObjectiveActionCreate
	ObjectiveActionUpdate           = action.ObjectiveActionUpdate
	ObjectiveActionPause            = action.ObjectiveActionPause
	AgentManagementSkillID          = models.AgentManagementSkillID
	AgentManagementSkillVersion     = models.AgentManagementSkillVersion
	RunManagementSkillID            = models.RunManagementSkillID
	RunManagementSkillVersion       = agentrun.RunManagementSkillVersion
	RunActionPause                  = models.RunActionPause
	RunActionResume                 = models.RunActionResume
	RunActionCancel                 = models.RunActionCancel
	AgentActionAmendBehavior        = models.AgentActionAmendBehavior
	AgentActionListChannels         = models.AgentActionListChannels
	AgentActionConfigureChannel     = models.AgentActionConfigureChannel
	RunbookManagementSkillID        = models.RunbookManagementSkillID
	RunbookManagementSkillVersion   = routine.RunbookManagementSkillVersion
	RunbookActionCreateTask         = models.RunbookActionCreateTask
	RunbookActionList               = models.RunbookActionList
	RunbookActionSetStatus          = models.RunbookActionSetStatus
	RunbookActionSetScript          = models.RunbookActionSetScript
	ExpectedDigestArgument          = workspace.ExpectedDigestArgument
	RefusedCommandResult            = workspace.RefusedResult
	RoutineScriptContextKey         = routine.RoutineScriptContextKey
	MaximumRoutineScriptSeconds     = models.MaximumRoutineScriptSeconds
	DefaultRoutineScriptSeconds     = routine.DefaultRoutineScriptSeconds
	RunbookActionStart              = routine.RunbookActionStart
	RunbookActionReplaceSchedule    = routine.RunbookActionReplaceSchedule
	TeamManagementSkillID           = models.TeamManagementSkillID
	TeamManagementSkillVersion      = models.TeamManagementSkillVersion
	TeamActionUpdateMember          = models.TeamActionUpdateMember
	TeamMemberAmendmentField        = models.TeamMemberAmendmentField
	TeamActionUpdateProfile         = models.TeamActionUpdateProfile
	TeamActionCreateTeam            = models.TeamActionCreateTeam
	SkillManagementSkillID          = models.SkillManagementSkillID
	SkillManagementSkillVersion     = skillbinding.SkillManagementSkillVersion
	SkillActionDiscover             = skillbinding.SkillActionDiscoverBinding
	SkillActionUpsertBinding        = skillbinding.SkillActionUpsertBinding
	SkillActionDisableBinding       = skillbinding.SkillActionDisableBinding
	RunbookActivationActive         = models.RunbookActivationActive
	RunbookActivationPaused         = models.RunbookActivationPaused
	RunbookActivationRetired        = models.RunbookActivationRetired

	RunSourceManual           = models.RunSourceManual
	RunSourceChat             = models.RunSourceChat
	RunSourceSchedule         = models.RunSourceSchedule
	RunSourceEvent            = models.RunSourceEvent
	RunSourceWebhook          = models.RunSourceWebhook
	RunSourceRequest          = models.RunSourceRequest
	RunSourceRequestDecision  = models.RunSourceRequestDecision
	RunSourceCompletionReview = models.RunSourceCompletionReview
	RunSourceHandoff          = models.RunSourceHandoff
	RunSourceEscalation       = models.RunSourceEscalation
	RunSourceObjective        = models.RunSourceObjective
	RunSourceFork             = models.RunSourceFork
	DelegationModeContextKey  = agentrun.DelegationModeContextKey

	RunKindAgentWork    = models.RunKindAgentWork
	RunKindConversation = models.RunKindConversation

	TeamChannelActive              = kernelteam.ChannelActive
	TeamChannelObserveOnly         = kernelteam.ChannelObserveOnly
	TeamChannelDisabled            = kernelteam.ChannelDisabled
	TeamDeploymentDraft            = kernelteam.DeploymentDraft
	TeamDeploymentActive           = kernelteam.DeploymentActive
	TeamDeploymentPaused           = kernelteam.DeploymentPaused
	TeamDeploymentArchived         = kernelteam.DeploymentArchived
	TeamAmendmentEvaluating        = kernelteam.AmendmentEvaluating
	TeamAmendmentAwaitingApproval  = kernelteam.AmendmentAwaitingApproval
	TeamAmendmentReady             = kernelteam.AmendmentReady
	TeamAmendmentApproved          = kernelteam.AmendmentApproved
	TeamAmendmentRejected          = kernelteam.AmendmentRejected
	TeamAmendmentEvaluationFailed  = kernelteam.AmendmentEvaluationFailed
	TeamAmendmentActivated         = kernelteam.AmendmentActivated
	AuthorityProgressionOwnerAgent = progression.OwnerAgent
	AuthorityProgressionOwnerTeam  = progression.OwnerTeam
	AuthorityProgressionPromote    = progression.DirectionPromote
	AuthorityProgressionNoChange   = progression.DirectionNoChange
	AuthorityProgressionRegress    = progression.DirectionRegress

	AgentRunStatusQueued                          = models.AgentRunStatusQueued
	AgentRunOrderScheduler                        = models.AgentRunOrderScheduler
	AgentRunOrderCreatedDesc                      = models.AgentRunOrderCreatedDesc
	AgentRunStatusPlanning                        = models.AgentRunStatusPlanning
	AgentRunStatusRunning                         = models.AgentRunStatusRunning
	AgentRunStatusPaused                          = models.AgentRunStatusPaused
	AgentRunStatusSleeping                        = models.AgentRunStatusSleeping
	AgentRunStatusWaitingForDependency            = models.AgentRunStatusWaitingForDependency
	AgentRunStatusWaitingForAgent                 = models.AgentRunStatusWaitingForAgent
	AgentRunStatusWaitingForApproval              = models.AgentRunStatusWaitingForApproval
	AgentRunStatusWaitingForEvent                 = models.AgentRunStatusWaitingForEvent
	AgentRunStatusCompleted                       = models.AgentRunStatusCompleted
	AgentRunStatusFailed                          = models.AgentRunStatusFailed
	AgentRunStatusCanceled                        = models.AgentRunStatusCanceled
	AgentRunAdmissionClaimed                      = models.AgentRunAdmissionClaimed
	AgentRunAdmissionReady                        = models.AgentRunAdmissionReady
	AgentRunAdmissionIdle                         = models.AgentRunAdmissionIdle
	AgentRunAdmissionNotDue                       = models.AgentRunAdmissionNotDue
	AgentRunAdmissionLeaseHeld                    = models.AgentRunAdmissionLeaseHeld
	AgentRunAdmissionBackpressured                = models.AgentRunAdmissionBackpressured
	AgentRunAdmissionBudgetStopped                = models.AgentRunAdmissionBudgetStopped
	AgentRunAdmissionReasonNotDue                 = models.AgentRunAdmissionReasonNotDue
	AgentRunAdmissionReasonLeaseHeld              = models.AgentRunAdmissionReasonLeaseHeld
	AgentRunAdmissionReasonAgentCapacity          = models.AgentRunAdmissionReasonAgentCapacity
	AgentRunAdmissionReasonOwnerCapacity          = models.AgentRunAdmissionReasonOwnerCapacity
	AgentRunAdmissionReasonObjectiveCapacity      = models.AgentRunAdmissionReasonObjectiveCapacity
	AgentRunAdmissionReasonResourceCapacity       = models.AgentRunAdmissionReasonResourceCapacity
	AgentRunAdmissionReasonConcurrencyCapacity    = models.AgentRunAdmissionReasonConcurrencyCapacity
	AgentRunAdmissionReasonAttemptBudgetExhausted = models.AgentRunAdmissionReasonAttemptBudgetExhausted

	BudgetStateActive    = models.BudgetStateActive
	BudgetStateWarning   = models.BudgetStateWarning
	BudgetStateExhausted = models.BudgetStateExhausted

	AgentRunCommandPause                    = agentrun.AgentRunCommandPause
	AgentRunCommandResume                   = agentrun.AgentRunCommandResume
	AgentRunCommandRetry                    = agentrun.AgentRunCommandRetry
	AgentRunCommandCancel                   = agentrun.AgentRunCommandCancel
	AgentRunCommandIntervene                = agentrun.AgentRunCommandIntervene
	AgentRunCommandResolveHumanIntervention = agentrun.AgentRunCommandResolveHumanIntervention

	HumanInterventionStatusPending  = models.HumanInterventionStatusPending
	HumanInterventionStatusResolved = models.HumanInterventionStatusResolved
	HumanInterventionStatusCanceled = models.HumanInterventionStatusCanceled

	ActivitySeverityDebug   = models.ActivitySeverityDebug
	ActivitySeverityInfo    = models.ActivitySeverityInfo
	ActivitySeverityWarning = models.ActivitySeverityWarning
	ActivitySeverityError   = models.ActivitySeverityError

	ActivityVisibilityPrivate = models.ActivityVisibilityPrivate
	ActivityVisibilityTeam    = models.ActivityVisibilityTeam
	ActivityVisibilityScope   = models.ActivityVisibilityScope

	AgentRequestKindRequest    = models.AgentRequestKindRequest
	AgentRequestKindHandoff    = models.AgentRequestKindHandoff
	AgentRequestKindEscalation = models.AgentRequestKindEscalation

	AgentRequestStatusPending                = models.AgentRequestStatusPending
	AgentRequestStatusClarificationRequested = models.AgentRequestStatusClarificationRequested
	AgentRequestStatusAccepted               = models.AgentRequestStatusAccepted
	AgentRequestStatusCompletionReview       = models.AgentRequestStatusCompletionReview
	AgentRequestStatusCompleted              = models.AgentRequestStatusCompleted
	AgentRequestStatusFailed                 = models.AgentRequestStatusFailed
	AgentRequestStatusRejected               = models.AgentRequestStatusRejected
	AgentRequestStatusCanceled               = models.AgentRequestStatusCanceled

	AgentRequestDecisionAccept               = collaboration.AgentRequestDecisionAccept
	AgentRequestDecisionReject               = collaboration.AgentRequestDecisionReject
	AgentRequestDecisionRequestClarification = collaboration.AgentRequestDecisionRequestClarification
	AgentRequestDecisionProvideClarification = collaboration.AgentRequestDecisionProvideClarification

	AgentRequestAcceptanceRecipientReview = models.AgentRequestAcceptanceRecipientReview
	AgentRequestAcceptancePreauthorized   = models.AgentRequestAcceptancePreauthorized

	AgentRequestBidAccept  = models.AgentRequestBidAccept
	AgentRequestBidDecline = models.AgentRequestBidDecline

	ConversationStatusActive   = models.ConversationStatusActive
	ConversationApprovalManual = models.ConversationApprovalManual
	ConversationApprovalAuto   = models.ConversationApprovalAuto
	ConversationApprovalSkip   = models.ConversationApprovalSkip
	ConversationStatusArchived = models.ConversationStatusArchived

	SpaceMemberAgent = models.SpaceMemberAgent
	SpaceMemberTeam  = models.SpaceMemberTeam

	SpaceTeamLeaves   = portfolio.SpaceTeamLeaves
	SpaceTeamArchived = portfolio.SpaceTeamArchived

	SpaceAccessOwner    = models.SpaceAccessOwner
	SpaceAccessInvited  = models.SpaceAccessInvited
	SpaceAccessEveryone = models.SpaceAccessEveryone

	ConversationParticipantUser       = models.ConversationParticipantUser
	ConversationParticipantAgent      = models.ConversationParticipantAgent
	ConversationParticipantTeam       = models.ConversationParticipantTeam
	ConversationParticipantService    = models.ConversationParticipantService
	VoiceCallCoordinatorParticipantID = models.VoiceCallCoordinatorParticipantID

	MessageIntentQuestion        = models.MessageIntentQuestion
	MessageIntentAnswer          = models.MessageIntentAnswer
	MessageIntentUpdate          = models.MessageIntentUpdate
	MessageIntentProposal        = models.MessageIntentProposal
	MessageIntentDecision        = models.MessageIntentDecision
	MessageIntentObjection       = models.MessageIntentObjection
	MessageIntentHandoff         = models.MessageIntentHandoff
	MessageIntentEscalation      = models.MessageIntentEscalation
	MessageIntentApprovalRequest = models.MessageIntentApprovalRequest
	MessageIntentAcknowledgment  = models.MessageIntentAcknowledgment
	MessageIntentSystem          = models.MessageIntentSystem

	ConversationAudienceChannel      = models.ConversationAudienceChannel
	ConversationAudienceParticipants = models.ConversationAudienceParticipants
	ConversationAudienceRoles        = models.ConversationAudienceRoles
	ConversationAudienceEveryone     = models.ConversationAudienceEveryone

	ConversationReferenceObjective      = models.ConversationReferenceObjective
	ConversationReferenceAgentControl   = models.ConversationReferenceAgentControl
	ConversationReferenceAgentApprovals = models.ConversationReferenceAgentApprovals
	ConversationReferenceEmbedSession   = models.ConversationReferenceEmbedSession

	EmbedInstallationActive             = models.EmbedInstallationActive
	EmbedInstallationPaused             = models.EmbedInstallationPaused
	EmbedInstallationRevoked            = models.EmbedInstallationRevoked
	EmbedIdentityAnonymous              = models.EmbedIdentityAnonymous
	EmbedIdentitySigned                 = models.EmbedIdentitySigned
	EmbedSessionActive                  = models.EmbedSessionActive
	EmbedSessionExpired                 = models.EmbedSessionExpired
	EmbedSessionRevoked                 = models.EmbedSessionRevoked
	ConversationReferenceTask           = models.ConversationReferenceTask
	ConversationReferenceRun            = models.ConversationReferenceRun
	ConversationReferenceRequest        = models.ConversationReferenceRequest
	ConversationReferenceApproval       = models.ConversationReferenceApproval
	ConversationReferenceArtifact       = models.ConversationReferenceArtifact
	ConversationReferenceActivity       = models.ConversationReferenceActivity
	ConversationReferenceExternalSource = models.ConversationReferenceExternalSource

	ConversationPresenceTyping   = models.ConversationPresenceTyping
	ConversationPresenceThinking = models.ConversationPresenceThinking
	ConversationPresenceWorking  = models.ConversationPresenceWorking

	ConversationAddressedMention  = models.ConversationAddressedMention
	ConversationAddressedEveryone = models.ConversationAddressedEveryone
	ConversationAddressedReply    = models.ConversationAddressedReply
	ConversationAddressedRole     = models.ConversationAddressedRole
	ConversationAddressedRouted   = models.ConversationAddressedRouted

	ParticipationRoundCommitted = models.ParticipationRoundCommitted

	RunDependencyKindRun          = models.RunDependencyKindRun
	RunDependencyKindAgentRequest = models.RunDependencyKindAgentRequest
	RunDependencyKindHandoff      = models.RunDependencyKindHandoff
	RunDependencyKindAction       = models.RunDependencyKindAction
	RunDependencyKindApproval     = models.RunDependencyKindApproval

	RunDependencyStatePending   = models.RunDependencyStatePending
	RunDependencyStateRunning   = models.RunDependencyStateRunning
	RunDependencyStateSatisfied = models.RunDependencyStateSatisfied
	RunDependencyStateFailed    = models.RunDependencyStateFailed
	RunDependencyStateCanceled  = models.RunDependencyStateCanceled

	FanInModeAll    = models.FanInModeAll
	FanInModeAny    = models.FanInModeAny
	FanInModeQuorum = models.FanInModeQuorum

	DependencyFailureFailFast = models.DependencyFailureFailFast
	DependencyFailureWait     = models.DependencyFailureWait

	RunDependencyGroupOpen      = models.RunDependencyGroupOpen
	RunDependencyGroupWaiting   = models.RunDependencyGroupWaiting
	RunDependencyGroupSatisfied = models.RunDependencyGroupSatisfied
	RunDependencyGroupFailed    = models.RunDependencyGroupFailed
	RunDependencyGroupCanceled  = models.RunDependencyGroupCanceled

	AgentTurnStatusRunning   = models.AgentTurnStatusRunning
	AgentTurnStatusCompleted = models.AgentTurnStatusCompleted
	AgentTurnStatusFailed    = models.AgentTurnStatusFailed
	AgentTurnStatusCanceled  = models.AgentTurnStatusCanceled

	SkillRiskRead        = skill.RiskLevelRead
	SkillRiskWrite       = skill.RiskLevelWrite
	SkillRiskExternal    = skill.RiskLevelExternal
	SkillRiskProduction  = skill.RiskLevelProduction
	SkillRiskDestructive = skill.RiskLevelDestructive

	WorkspaceOperationListDirectory   = workspace.OperationListDirectory
	WorkspaceOperationReadFile        = workspace.OperationReadFile
	WorkspaceOperationSearchFiles     = workspace.OperationSearchFiles
	WorkspaceOperationWriteFile       = workspace.OperationWriteFile
	WorkspaceOperationRunCommand      = workspace.OperationRunCommand
	SkillBindingArgumentLiteral       = skill.BindingArgumentLiteral
	SkillBindingArgumentSessionID     = skill.BindingArgumentSessionID
	SkillBindingArgumentVerifiedClaim = skill.BindingArgumentVerifiedClaim
	SkillBindingLifecycleDisabled     = skill.BindingLifecycleDisabled
	SkillBindingLifecycleDeleted      = skill.BindingLifecycleDeleted

	OAuth2SubjectInstallation = capability.OAuth2SubjectInstallation
	OAuth2SubjectUser         = capability.OAuth2SubjectUser
	OAuthConnectionAPIVersion = kerneloauth.APIVersion

	OAuthConnectionPending  = kerneloauth.ConnectionPending
	OAuthConnectionActive   = kerneloauth.ConnectionActive
	OAuthConnectionDegraded = kerneloauth.ConnectionDegraded
	OAuthConnectionRevoked  = kerneloauth.ConnectionRevoked

	OAuthAuthorizationPending    = kerneloauth.SessionPending
	OAuthAuthorizationExchanging = kerneloauth.SessionExchanging
	OAuthAuthorizationCompleted  = kerneloauth.SessionCompleted
	OAuthAuthorizationDenied     = kerneloauth.SessionDenied
	OAuthAuthorizationFailed     = kerneloauth.SessionFailed

	OAuthConnectionOwnerUser = kerneloauth.OwnerUser

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
	SkillAdapterConversation    = skill.AdapterConversation

	ConversationEndpointChannel = capability.ConversationEndpointChannel
	ConversationEndpointDirect  = capability.ConversationEndpointDirect

	ConversationAdapterProtocolV1 = capability.ConversationAdapterProtocolV1
	CallbackAdapterProtocolV1     = capability.CallbackAdapterProtocolV1
	CallbackEventApprovalDecided  = capability.CallbackEventApprovalDecided

	ConversationFeatureThreads     = capability.ConversationFeatureThreads
	ConversationFeatureMentions    = capability.ConversationFeatureMentions
	ConversationFeatureAttachments = capability.ConversationFeatureAttachments
	ConversationFeatureReactions   = capability.ConversationFeatureReactions
	ConversationFeatureEdits       = capability.ConversationFeatureEdits
	ConversationFeatureDeletes     = capability.ConversationFeatureDeletes
	ConversationFeatureTyping      = capability.ConversationFeatureTyping

	ConversationDeliveryMessageSend     = capability.ConversationDeliveryMessageSend
	ConversationDeliveryMessageUpdate   = capability.ConversationDeliveryMessageUpdate
	ConversationDeliveryMessageDelete   = capability.ConversationDeliveryMessageDelete
	ConversationDeliveryReactionAdd     = capability.ConversationDeliveryReactionAdd
	ConversationDeliveryReactionRemove  = capability.ConversationDeliveryReactionRemove
	ConversationDeliveryTypingIndicator = capability.ConversationDeliveryTypingIndicator

	ConversationDeliveryOrderEndpoint     = capability.ConversationDeliveryOrderEndpoint
	ConversationDeliveryOrderConversation = capability.ConversationDeliveryOrderConversation
	ConversationDeliveryOrderThread       = capability.ConversationDeliveryOrderThread

	ConversationEventMessageReceived   = capability.ConversationEventMessageReceived
	ConversationEventMessageUpdated    = capability.ConversationEventMessageUpdated
	ConversationEventMessageDeleted    = capability.ConversationEventMessageDeleted
	ConversationEventReactionAdded     = capability.ConversationEventReactionAdded
	ConversationEventReactionRemoved   = capability.ConversationEventReactionRemoved
	ConversationEventParticipantJoined = capability.ConversationEventParticipantJoined
	ConversationEventParticipantLeft   = capability.ConversationEventParticipantLeft
)

const (
	MaximumExternalConversationIngressBytes = externalconversation.MaximumExternalConversationIngressBytes
	MaximumCallbackIngressBytes             = callback.MaximumCallbackIngressBytes

	ExternalConversationEndpointActive  = models.ExternalConversationEndpointActive
	ExternalConversationEndpointPaused  = models.ExternalConversationEndpointPaused
	ExternalConversationEndpointRetired = models.ExternalConversationEndpointRetired
	ExternalConversationGatewayActive   = models.ExternalConversationGatewayActive
	ExternalConversationGatewayPaused   = models.ExternalConversationGatewayPaused
	ExternalConversationGatewayRetired  = models.ExternalConversationGatewayRetired
	CallbackRegistrationActive          = models.CallbackRegistrationActive
	CallbackRegistrationPaused          = models.CallbackRegistrationPaused
	CallbackRegistrationRetired         = models.CallbackRegistrationRetired
	CallbackEventPending                = models.CallbackEventPending
	CallbackEventApplied                = models.CallbackEventApplied

	ExternalConversationHandlerAgent = models.ExternalConversationHandlerAgent
	ExternalConversationHandlerTeam  = models.ExternalConversationHandlerTeam

	ExternalConversationSelectAllMessages     = models.ExternalConversationSelectAllMessages
	ExternalConversationSelectMentions        = models.ExternalConversationSelectMentions
	ExternalConversationSelectDirectOrMention = models.ExternalConversationSelectDirectOrMention

	ExternalConversationReplyProviderDefault = models.ExternalConversationReplyProviderDefault
	ExternalConversationReplyThread          = models.ExternalConversationReplyThread
	ExternalConversationReplyChannel         = models.ExternalConversationReplyChannel

	ExternalConversationInboxPending    = models.ExternalConversationInboxPending
	ExternalConversationInboxLeased     = models.ExternalConversationInboxLeased
	ExternalConversationInboxRetry      = models.ExternalConversationInboxRetry
	ExternalConversationInboxApplied    = models.ExternalConversationInboxApplied
	ExternalConversationInboxIgnored    = models.ExternalConversationInboxIgnored
	ExternalConversationInboxDeadLetter = models.ExternalConversationInboxDeadLetter

	ExternalMessageInbound  = models.ExternalMessageInbound
	ExternalMessageOutbound = models.ExternalMessageOutbound

	ExternalConversationDeliveryPending   = models.ExternalConversationDeliveryPending
	ExternalConversationDeliveryLeased    = models.ExternalConversationDeliveryLeased
	ExternalConversationDeliveryRetry     = models.ExternalConversationDeliveryRetry
	ExternalConversationDeliveryDelivered = models.ExternalConversationDeliveryDelivered
	ExternalConversationDeliveryFailed    = models.ExternalConversationDeliveryFailed
	ExternalConversationDeliveryCanceled  = models.ExternalConversationDeliveryCanceled

	ExternalConversationAcknowledgementFound    = externalconversation.ExternalConversationAcknowledgementFound
	ExternalConversationAcknowledgementNotFound = externalconversation.ExternalConversationAcknowledgementNotFound
	ExternalConversationAcknowledgementUnknown  = externalconversation.ExternalConversationAcknowledgementUnknown

	ExternalConversationDeliveryOutcomeDelivered = externalconversation.ExternalConversationDeliveryOutcomeDelivered
	ExternalConversationDeliveryOutcomeRetry     = externalconversation.ExternalConversationDeliveryOutcomeRetry
	ExternalConversationDeliveryOutcomeFailed    = externalconversation.ExternalConversationDeliveryOutcomeFailed
)

var (
	ErrExternalOperationClaimed             = models.ErrExternalOperationClaimed
	ErrExternalConversationEndpointNotFound = models.ErrExternalConversationEndpointNotFound
	ErrExternalConversationGatewayNotFound  = models.ErrExternalConversationGatewayNotFound
	ErrCallbackRegistrationNotFound         = models.ErrCallbackRegistrationNotFound
	ErrInvalidCallbackRegistration          = models.ErrInvalidCallbackRegistration
	ErrCallbackRegistrationConflict         = models.ErrCallbackRegistrationConflict
	ErrExternalConversationConflict         = models.ErrExternalConversationConflict
	ErrInvalidExternalConversation          = models.ErrInvalidExternalConversation
)

const (
	SkillAdapterHTTPAction  = skill.AdapterHTTPAction
	HTTPActionTransportName = httpaction.TransportName

	SkillRuntimePreparing   = skill.RuntimePreparationPreparing
	SkillRuntimeReady       = skill.RuntimePreparationReady
	SkillRuntimeUnavailable = skill.RuntimePreparationUnavailable

	ActionDispositionAllow           = action.ActionDispositionAllow
	ActionDispositionDeny            = action.ActionDispositionDeny
	ActionDispositionRequireApproval = action.ActionDispositionRequireApproval

	ActionCallStatusReady           = models.ActionCallStatusReady
	ActionCallStatusWaitingApproval = models.ActionCallStatusWaitingApproval
	ActionCallStatusDenied          = models.ActionCallStatusDenied
	ActionCallStatusRunning         = models.ActionCallStatusRunning
	ActionCallStatusSucceeded       = models.ActionCallStatusSucceeded
	ActionCallStatusFailed          = models.ActionCallStatusFailed

	ApprovalStatusPending          = models.ApprovalStatusPending
	ApprovalStatusApproved         = models.ApprovalStatusApproved
	ApprovalStatusRejected         = models.ApprovalStatusRejected
	ApprovalStatusChangesRequested = models.ApprovalStatusChangesRequested
	ApprovalStatusExpired          = models.ApprovalStatusExpired
	ApprovalStatusCanceled         = models.ApprovalStatusCanceled
	ApprovalStatusSuperseded       = models.ApprovalStatusSuperseded

	ApprovalDecisionApprove        = models.ApprovalDecisionApprove
	ApprovalDecisionReject         = models.ApprovalDecisionReject
	ApprovalDecisionRequestChanges = models.ApprovalDecisionRequestChanges

	ApprovalTimeoutExpire  = models.ApprovalTimeoutExpire
	ApprovalTimeoutApprove = models.ApprovalTimeoutApprove

	SkillSourceWorkspace    = skillsource.RootWorkspace
	SkillSourceProjectAgent = skillsource.RootProjectAgent
	SkillSourcePersonal     = skillsource.RootPersonal
	SkillSourceManaged      = skillsource.RootManaged
	SkillSourceBundled      = skillsource.RootBundled
	SkillSourcePlugin       = skillsource.RootPlugin
	SkillSourceExtra        = skillsource.RootExtra

	AgentRolloutPending                 = kernelagent.RolloutPending
	AgentRolloutActive                  = kernelagent.RolloutActive
	AgentRolloutDegraded                = kernelagent.RolloutDegraded
	AgentRolloutPaused                  = kernelagent.RolloutPaused
	AgentRolloutRetired                 = kernelagent.RolloutRetired
	AgentModelProviderCredentialBinding = kernelagent.ModelProviderCredentialBinding
	AgentCompilationClean               = kernelagent.CompilationClean
	AgentCompilationFailed              = kernelagent.CompilationFailed

	AgentAmendmentEvaluating       = kernelagent.AmendmentEvaluating
	AgentAmendmentAwaitingApproval = kernelagent.AmendmentAwaitingApproval
	AgentAmendmentReady            = kernelagent.AmendmentReady
	AgentAmendmentApproved         = kernelagent.AmendmentApproved
	AgentAmendmentRejected         = kernelagent.AmendmentRejected
	AgentAmendmentEvaluationFailed = kernelagent.AmendmentEvaluationFailed
	AgentAmendmentActivated        = kernelagent.AmendmentActivated

	ArtifactClassPublic        = models.ArtifactClassificationPublic
	ArtifactClassInternal      = models.ArtifactClassificationInternal
	ArtifactClassConfidential  = models.ArtifactClassificationConfidential
	ArtifactClassRestricted    = models.ArtifactClassificationRestricted
	ArtifactContentAvailable   = models.ArtifactContentAvailable
	ArtifactContentUnavailable = models.ArtifactContentUnavailable

	ArtifactEvidenceDerivedFrom = models.EvidenceRelationDerivedFrom
	ArtifactEvidenceSupports    = models.EvidenceRelationSupports
	ArtifactEvidenceContradicts = models.EvidenceRelationContradicts
	ArtifactEvidenceCites       = models.EvidenceRelationCites
	ArtifactEvidenceInputTo     = models.EvidenceRelationInputTo
	ArtifactEvidenceOutputOf    = models.EvidenceRelationOutputOf

	ArtifactEvidenceTargetArtifact       = models.EvidenceTargetArtifact
	ArtifactEvidenceTargetExternalSource = models.EvidenceTargetExternalSource
	ArtifactEvidenceTargetRun            = models.EvidenceTargetRun
	ArtifactEvidenceTargetActivity       = models.EvidenceTargetActivity
	ArtifactEvidenceTargetRequest        = models.EvidenceTargetRequest
	ArtifactEvidenceTargetAction         = models.EvidenceTargetAction
	ArtifactEvidenceTargetTurn           = models.EvidenceTargetTurn
	ArtifactEvidenceTargetClaim          = models.EvidenceTargetClaim
)

const ()

var (
	ErrSkillDefinitionImmutable         = skill.ErrDefinitionImmutable
	ErrSkillDefinitionAmbiguous         = skill.ErrDefinitionAmbiguous
	ErrSkillBindingAmbiguous            = skill.ErrBindingAmbiguous
	ErrSkillBindingInvalid              = skill.ErrBindingInvalid
	ErrSkillBindingRevisionConflict     = skill.ErrBindingRevisionConflict
	ErrSkillBindingNotFound             = skill.ErrBindingNotFound
	ErrSkillBindingAlreadyDisabled      = skill.ErrBindingAlreadyDisabled
	ErrSkillSourceArtifactNotFound      = sourceartifact.ErrNotFound
	ErrSkillSourceArtifactImmutable     = sourceartifact.ErrImmutable
	ErrSkillSourceReferenceConflict     = sourceartifact.ErrReferenceConflict
	ErrSkillSourceOriginAmbiguous       = sourceartifact.ErrAmbiguousOrigin
	ErrInvalidAuthorityEvaluation       = progression.ErrInvalidEvaluation
	ErrAuthorityPolicyCeiling           = progression.ErrPolicyCeiling
	ErrAuthorityRecommendationStale     = progression.ErrRecommendationStale
	ErrAuthorityProgressionNoChange     = progression.ErrNoChange
	ErrSkillReferenceUpgradeUnavailable = skillbinding.ErrSkillReferenceUpgradeUnavailable
	ErrSkillReferenceUpgradeConflict    = models.ErrSkillReferenceUpgradeConflict
	ErrSkillReferenceUpgradeApproval    = skillbinding.ErrSkillReferenceUpgradeApproval
	ErrSkillReferenceUpgradeInvalid     = models.ErrSkillReferenceUpgradeInvalid
	ErrSkillReferenceUpgradeBusy        = models.ErrSkillReferenceUpgradeBusy
	ErrSkillReferenceUpgradeCurrent     = skillbinding.ErrSkillReferenceUpgradeCurrent
)

// Engine is the primary entry point for OpenSeal. It composes the durable
// Agent, Team, objective, collaboration, action, approval, and Skill services.
type Engine struct {
	store models.KernelStore
	// ownedStore is the in-memory store New opened, either as e.store or as
	// the fallback for services a partial store lacks. Close frees it.
	ownedStore                    *sqlite.Store
	portfolio                     *portfolio.PortfolioService
	activity                      *activity.RunActivityService
	dependencies                  *portfolio.DependencyCoordinator
	conversations                 *conversation.ConversationService
	spaces                        *portfolio.SpaceService
	embeds                        *conversation.EmbedSessionService
	conversationCoordinator       *conversation.ConversationCoordinator
	conversationParticipants      conversation.ConversationParticipantSource
	conversationRouter            conversation.ConversationRouter
	conversationCoordinatorConfig conversation.ConversationCoordinatorConfig
	conversationRunScheduler      *conversationrun.ConversationRunScheduler
	conversationRunReconciler     *conversationrun.ConversationRunReconciler
	conversationRunConfig         *ConversationRunConfig
	conversationRunScopes         workqueue.WorkerScopeSource
	externalConversations         externalConversationRuntime
	callbacks                     callbackRuntime
	routineEvents                 routineEventRuntime
	conversationChanges           *conversation.ConversationChangeService
	collaboration                 *collaboration.CollaborationService
	turns                         *activity.AgentTurnService
	runQueue                      *agentrun.AgentRunScheduler
	wake                          *agentrun.AgentRunWakeService
	actions                       *action.ActionCoordinator
	approvals                     *action.ApprovalCoordinator
	artifacts                     *artifactcatalog.ArtifactCatalog
	runbookAudits                 *routine.RunbookExecutionAuditService
	actionPolicy                  action.ActionPolicyEvaluator
	routineOwnerAddresses         models.RoutineOwnerAddressResolver
	chatFiles                     routine.ChatFileResolver
	chatWorkspaces                modelstep.ChatWorkspaces
	actionValidators              []action.ActionProposalValidator
	agentManagementActions        bool
	agentBuilderHost              agentbuilder.AgentBuilderHost
	agentInstaller                *agentbuilder.AgentInstaller
	agentBuilderCutoverHost       agentbuilder.AgentBuilderCutoverHost
	runbookManagementActions      bool
	runManagementActions          bool
	teamManagementActions         bool
	skillManagementActions        bool
	skillDiscovery                skill.DiscoveryProvider
	appCatalog                    *skillbinding.AppCatalogRef
	approvalAuth                  action.ApprovalAuthorizer
	skillSources                  *sourceartifact.Service
	agentPoolSpecs                []agentRunWorkerSpec
	agentPools                    []*runworker.AgentRunWorkerPool
	agentSupervisorSpecs          []agentRunWorkerSupervisorSpec
	agentSupervisors              []*runworker.AgentRunWorkerSupervisor
	actionPoolSpecs               []actionWorkerSpec
	actionPools                   []*action.ActionWorkerPool
	actionSupervisorSpecs         []actionWorkerSupervisorSpec
	actionSupervisors             []*action.ActionWorkerSupervisor
	governedWorker                *action.ActionWorker
	governedTools                 *runloop.GovernedToolExecutor
	credentialAvailability        action.CredentialAvailabilityChecker
	skills                        *skill.Catalog
	skillReferenceUpgrades        *skillbinding.SkillReferenceUpgradeService
	agents                        *kernelagent.Registry
	teams                         *kernelteam.Registry
	progression                   *progression.Service
	logger                        *zap.SugaredLogger
	workerLimiter                 *workqueue.WorkerLimiter
	workSignals                   *workqueue.WorkSignalStatus
	workQueue                     workqueue.WorkQueue
	workPublish                   func(context.Context)
	workQueueCancel               context.CancelFunc
	workQueueDone                 chan struct{}
	workRelayRequests             chan struct{}
	workRebuilds                  chan struct{}
	workHandlersMu                sync.RWMutex
	workHandlers                  map[models.WorkFamily][]func(models.Scope)
}

type agentRunWorkerSpec struct {
	config   runworker.AgentRunWorkerConfig
	resolver conversation.TurnRunnerResolver
}

type agentRunWorkerSupervisorSpec struct {
	config   runworker.DynamicAgentRunWorkerConfig
	source   workqueue.WorkerScopeSource
	resolver conversation.TurnRunnerResolver
}

// ConversationRunConfig makes Team channel participation durable and
// server-owned. Worker concurrency controls channels processed per scope;
// each channel itself is always serialized across replicas.
type ConversationRunConfig struct {
	Scheduler  conversationrun.ConversationRunSchedulerConfig
	Runner     conversationrun.ConversationRunTurnRunnerConfig
	Reconciler conversationrun.ConversationRunReconcilerConfig
	Workers    runworker.DynamicAgentRunWorkerConfig
}

type ExternalConversationSupervisorConfig = externalconversation.ExternalConversationSupervisorConfig

type externalConversationRuntime struct {
	transport  *externalconversation.ExternalConversationTransportService
	endpoints  *externalconversation.ExternalConversationEndpointService
	gateways   *externalconversation.ExternalConversationGatewayService
	supervisor *externalconversation.ExternalConversationSupervisor
	config     *externalconversation.ExternalConversationSupervisorConfig
	scopes     workqueue.WorkerScopeSource
	host       externalconversation.ExternalConversationAdapterHost
}

type callbackRuntime struct {
	registry *callback.CallbackRegistry
	ingress  *callback.CallbackIngressService
	worker   *callback.CallbackEventWorker
}

type actionWorkerSpec struct {
	config      action.ActionWorkerConfig
	credentials action.CredentialResolver
	leaseIssuer action.ActionCredentialLeaseIssuer
	dispatcher  agentrun.ActionDispatcher
}

type actionWorkerSupervisorSpec struct {
	config      action.DynamicActionWorkerConfig
	source      workqueue.WorkerScopeSource
	credentials action.CredentialResolver
	leaseIssuer action.ActionCredentialLeaseIssuer
	dispatcher  agentrun.ActionDispatcher
}

// Option configures an Engine.
type Option func(*Engine) error

// New creates an Engine with the given options.
//
// The Engine's store is the one WithStore or WithPersistentStore gives (the
// last one wins). Without one, New opens an in-memory SQLite store whose
// state ends with the Engine. Every store-backed service is wired once, after
// all options ran, from that one store:
//   - Runs, turns, activity and other models.KernelStore services always
//     use it.
//   - Dependencies, conversations, embeds, spaces, collaboration, artifacts,
//     Run audits and Skill reference upgrades exist only when it implements
//     their store interface; otherwise they are unavailable.
//   - Agents, Teams and the Skill catalog never go without a store: each one
//     whose interface it lacks is kept in an in-memory store New opens, and
//     Teams kept there resolve Agents from that store too.
//   - WithSkillCatalog and WithSkillSourceArtifactStore win over the store
//     whatever their order; skill sources are otherwise unavailable unless
//     the store implements them.
//
// Durable worker pools are opt-in. Callers must Close the Engine to free a
// store New opened; a store passed in stays open for its owner to close.
func New(opts ...Option) (_ *Engine, err error) {
	logger, _ := zap.NewProduction()
	e := &Engine{
		workSignals:  workqueue.NewWorkSignalStatus(),
		approvalAuth: action.EligibleApprovalAuthorizer{},
		logger:       logger.Sugar(),
	}
	for _, opt := range opts {
		if err := opt(e); err != nil {
			return nil, fmt.Errorf("engine option: %w", err)
		}
	}
	defer func() {
		if err != nil {
			e.closeOwnedStore()
		}
	}()
	if err := e.wireStore(); err != nil {
		return nil, err
	}
	if e.actionPolicy == nil {
		e.actionPolicy = action.ActionPolicyEvaluatorFunc(e.evaluateAgentActionAuthority)
	}
	e.buildRoutineEvents()
	if err := e.rebuildConversationCoordinator(); err != nil {
		return nil, fmt.Errorf("conversation coordinator configuration: %w", err)
	}
	if err := e.rebuildConversationRuns(); err != nil {
		return nil, fmt.Errorf("conversation Run configuration: %w", err)
	}
	if e.conversations != nil {
		e.conversations.SetReactionProjector(e.ReactionProjector())
	}
	if err := e.rebuildExternalConversations(); err != nil {
		return nil, fmt.Errorf("external conversation configuration: %w", err)
	}
	if err := e.rebuildCallbacks(); err != nil {
		return nil, fmt.Errorf("callback registry configuration: %w", err)
	}
	// Guard the host dispatcher at the final external execution boundary before
	// adding kernel-owned management routes. Skill and Team management actions
	// enforce their own resource authority and must retain their fallthrough to
	// the host chain; ordinary Agent and Team Skill calls still pass through this
	// guard immediately before the embedding host is invoked.
	for index := range e.actionPoolSpecs {
		dispatcher, dispatchErr := skillbinding.NewTeamSkillActionDispatcher(e, e.actionPoolSpecs[index].dispatcher)
		if dispatchErr != nil {
			return nil, fmt.Errorf("Team Skill action worker configuration: %w", dispatchErr)
		}
		e.actionPoolSpecs[index].dispatcher = dispatcher
	}
	for index := range e.actionSupervisorSpecs {
		dispatcher, dispatchErr := skillbinding.NewTeamSkillActionDispatcher(e, e.actionSupervisorSpecs[index].dispatcher)
		if dispatchErr != nil {
			return nil, fmt.Errorf("Team Skill dynamic action worker configuration: %w", dispatchErr)
		}
		e.actionSupervisorSpecs[index].dispatcher = dispatcher
	}
	if err := e.configureSkillManagementActions(); err != nil {
		return nil, fmt.Errorf("Skill management action configuration: %w", err)
	}
	if err := e.configureAgentManagementActions(); err != nil {
		return nil, fmt.Errorf("Agent management action configuration: %w", err)
	}
	if err := e.configureRunbookManagementActions(); err != nil {
		return nil, fmt.Errorf("Runbook management action configuration: %w", err)
	}
	if err := e.configureRunManagementActions(); err != nil {
		return nil, fmt.Errorf("Run management action configuration: %w", err)
	}
	if err := e.configureTeamManagementActions(); err != nil {
		return nil, fmt.Errorf("Team management action configuration: %w", err)
	}
	teamSkillValidator, err := skillbinding.NewTeamSkillActionValidator(e)
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

// Start begins configured background workers. When the store keeps a work
// outbox (PostgreSQL), workers are driven by a work queue: committed changes
// are relayed from the outbox into the queue, loops schedule their next
// durable deadline into it, and a scope's workers only exist while the queue
// has work for it. Whenever the queue is not consuming, and for other
// stores, workers fall back to their configured poll intervals.
//
// Before any worker starts, Start moves the store onto the run loop
// (RunLoopCutover): Runs from before the loop are adopted or canceled. Then
// AgentBuilderCutover turns the retired change-set drafts into builder
// conversations. Both fail closed: on a store error no worker is started and
// the error is returned.
func (e *Engine) Start(ctx context.Context) error {
	report, err := e.RunLoopCutover(ctx)
	if err != nil {
		return err
	}
	if e.logger != nil && (report.Adopted > 0 || report.Canceled > 0) {
		e.logger.Infow("run loop cutover", "adopted", report.Adopted, "canceled", report.Canceled, "messages", report.Messages)
	}
	builderReport, err := e.AgentBuilderCutover(ctx)
	if err != nil {
		return err
	}
	if e.logger != nil && (builderReport.Converted > 0 || builderReport.Kept > 0 || builderReport.Dropped) {
		e.logger.Infow("agent builder cutover", "converted", builderReport.Converted, "kept", builderReport.Kept, "dropped", builderReport.Dropped)
	}
	if err := e.routineEvents.matcher.RebuildRoutineIndex(ctx); err != nil {
		return fmt.Errorf("build routine event index: %w", err)
	}
	e.applyWorkSignals()
	if e.conversationRunReconciler != nil {
		e.conversationRunReconciler.Start(ctx)
	}
	if e.externalConversations.supervisor != nil {
		e.externalConversations.supervisor.Start(ctx)
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
	e.startWorkQueue(ctx)
	return nil
}

// Stop gracefully shuts down background goroutines.
func (e *Engine) Stop() {
	e.stopWorkQueue()
	if e.externalConversations.supervisor != nil {
		e.externalConversations.supervisor.Stop()
	}
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
}

// Close stops the Engine and frees the in-memory store New opened, if any. A
// store passed with WithStore or WithPersistentStore stays open; its owner
// closes it. The Engine must not be used after Close.
func (e *Engine) Close() error {
	e.Stop()
	return e.closeOwnedStore()
}

// Store returns the execution store.
func (e *Engine) Store() models.KernelStore {
	return e.store
}

// WorkerConcurrencyStats exposes the process-wide worker admission state for
// host metrics and rollout diagnostics.
func (e *Engine) WorkerConcurrencyStats() workqueue.WorkerLimiterStats {
	return e.workerLimiter.Stats()
}

// Logger returns the engine's logger.
func (e *Engine) Logger() *zap.SugaredLogger {
	return e.logger
}

// WithStore sets the Engine's store in place of the default in-memory one.
// See New for which services a store that implements only part of the
// durable contract gets.
func WithStore(store models.KernelStore) Option {
	return func(e *Engine) error {
		if store == nil {
			return fmt.Errorf("store is required")
		}
		e.store = store
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

// WithPersistentStore wires the full durable kernel contract. Use this option
// for production deployments; WithStore remains available for runtime-only
// embeddings and tests.
func WithPersistentStore(store PersistentKernelStore) Option {
	return WithStore(store)
}

// WithConversationCoordinator connects Team channel routing to a host's
// authorized Agent roster and its router. The host keeps tenant, credential,
// model, and deployment concerns behind these interfaces.
func WithConversationCoordinator(
	participants conversation.ConversationParticipantSource,
	router conversation.ConversationRouter,
	config conversation.ConversationCoordinatorConfig,
) Option {
	return func(e *Engine) error {
		e.conversationParticipants = participants
		e.conversationRouter = router
		e.conversationCoordinatorConfig = config
		return nil
	}
}

// WithDynamicConversationRuns moves channel participation from an interactive
// caller into durable Run workers for every active host scope. The same host
// participant/proposal adapters configured by WithConversationCoordinator are
// used by the Run turn runner.
func WithDynamicConversationRuns(config ConversationRunConfig, scopes workqueue.WorkerScopeSource) Option {
	return func(e *Engine) error {
		if scopes == nil {
			return fmt.Errorf("conversation Run scope source is required")
		}
		e.conversationRunConfig = &config
		e.conversationRunScopes = scopes
		return nil
	}
}

// WithExternalConversationTransport enables the durable provider-neutral
// inbox, Runbook/Agent reply projection, and Skill-owned delivery outbox.
// Provider adapters and credential redemption stay behind host.
func WithExternalConversationTransport(
	config externalconversation.ExternalConversationSupervisorConfig,
	scopes workqueue.WorkerScopeSource,
	host externalconversation.ExternalConversationAdapterHost,
) Option {
	return func(e *Engine) error {
		if scopes == nil || host == nil {
			return fmt.Errorf("external conversation scope source and adapter host are required")
		}
		e.externalConversations.config = &config
		e.externalConversations.scopes = scopes
		e.externalConversations.host = host
		return nil
	}
}

func (e *Engine) rebuildConversationCoordinator() error {
	e.conversationCoordinator = nil
	if e.conversationParticipants == nil && e.conversationRouter == nil {
		return nil
	}
	coordinator, err := conversation.NewConversationCoordinator(
		e.conversations, e.conversationParticipants, e.conversationRouter, e.conversationCoordinatorConfig,
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
		return conversation.ErrConversationCoordinationUnavailable
	}
	conversationStore, ok := e.store.(models.ConversationStore)
	if !ok {
		return fmt.Errorf("persistent store does not implement conversation storage")
	}
	config := *e.conversationRunConfig
	if config.Scheduler.RequireParticipationOptIn != config.Runner.RequireParticipationOptIn {
		return fmt.Errorf("conversation scheduling and execution must use the same participation opt-in policy")
	}
	if config.Workers.Kind == "" {
		config.Workers.Kind = models.RunKindConversation
	}
	if config.Workers.Kind != models.RunKindConversation {
		return fmt.Errorf("conversation workers require Run kind %q", models.RunKindConversation)
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
	scheduler, err := conversationrun.NewConversationRunScheduler(conversationStore, e.store, config.Scheduler)
	if err != nil {
		return err
	}
	if e.embeds != nil {
		scheduler.SetSessionContextResolver(e.embeds)
	}
	runner, err := conversationrun.NewConversationRunTurnRunner(conversationStore, e.conversationCoordinator, config.Runner)
	if err != nil {
		return err
	}
	runner.SetReactionProjector(e.ReactionProjector())
	reconciler, err := conversationrun.NewConversationRunReconciler(scheduler, e.conversationRunScopes, e.logger, config.Reconciler)
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

func (e *Engine) rebuildExternalConversations() error {
	e.externalConversations.transport = nil
	e.externalConversations.endpoints = nil
	e.externalConversations.gateways = nil
	e.externalConversations.supervisor = nil
	store, ok := e.store.(models.ExternalConversationReplyStore)
	if !ok || e.skills == nil {
		if e.externalConversations.config != nil {
			return fmt.Errorf("persistent store does not implement external conversation storage")
		}
		return nil
	}
	e.externalConversations.transport = externalconversation.NewExternalConversationTransportService(store, e.skills)
	e.externalConversations.transport.SetRoutineEventOfferer(e.routineEvents.matcher)
	e.externalConversations.endpoints = externalconversation.NewExternalConversationEndpointService(store, e.skills)
	e.externalConversations.gateways = externalconversation.NewExternalConversationGatewayService(store, e.skills)
	if e.externalConversations.config == nil && e.externalConversations.scopes == nil && e.externalConversations.host == nil {
		return nil
	}
	if e.externalConversations.config == nil || e.externalConversations.scopes == nil || e.externalConversations.host == nil ||
		e.conversationRunScheduler == nil {
		return fmt.Errorf("external conversation transport requires dynamic conversation Runs, scopes, host, and config")
	}
	dispatcher := externalconversation.NewCanonicalExternalConversationDispatcher(e.conversationRunScheduler)
	inboxConfig := e.externalConversations.config.Inbox
	if contextHost, ok := e.externalConversations.host.(externalconversation.ExternalConversationContextHost); ok {
		inboxConfig.ContextHost = contextHost
		inboxConfig.ContextCatalog = e.skills
	}
	if attachmentHost, ok := e.externalConversations.host.(externalconversation.ExternalConversationAttachmentHost); ok {
		inboxConfig.AttachmentHost = attachmentHost
		inboxConfig.ContextCatalog = e.skills
	}
	inbox, err := externalconversation.NewExternalConversationInboxWorker(store, dispatcher, inboxConfig)
	if err != nil {
		return err
	}
	replies, err := externalconversation.NewExternalConversationReplyWorker(store, e.skills)
	if err != nil {
		return err
	}
	acknowledgementStore, ok := e.store.(models.RunProgressAcknowledgementStore)
	if !ok {
		return fmt.Errorf("persistent store does not implement Run progress acknowledgement storage")
	}
	acknowledgementRenderer, err := externalconversation.NewTurnRunnerProgressAcknowledgementRenderer(e.conversationRunConfig.Runner.AgentTurns)
	if err != nil {
		return err
	}
	acknowledgements, err := externalconversation.NewRunProgressAcknowledgementWorker(
		acknowledgementStore, e.skills, acknowledgementRenderer, e.externalConversations.config.Acknowledgements,
	)
	if err != nil {
		return err
	}
	delivery, err := externalconversation.NewExternalConversationDeliveryWorker(
		store, e.skills, e.externalConversations.host, e.externalConversations.config.Delivery,
	)
	if err != nil {
		return err
	}
	approvalStore, ok := e.store.(models.ApprovalNotificationStore)
	if !ok {
		return fmt.Errorf("persistent store does not implement approval notification storage")
	}
	approvals := externalconversation.NewApprovalNotificationWorker(approvalStore, e.externalConversations.transport, e.skills)
	e.externalConversations.supervisor, err = externalconversation.NewExternalConversationSupervisor(
		inbox, replies, delivery, approvals, e.externalConversations.scopes, e.logger, *e.externalConversations.config,
	)
	if err == nil {
		e.externalConversations.supervisor.SetAcknowledgementWorker(acknowledgements)
	}
	return err
}

func (e *Engine) rebuildCallbacks() error {
	e.callbacks.registry = nil
	e.callbacks.ingress = nil
	e.callbacks.worker = nil
	store, ok := e.store.(interface {
		models.CallbackRegistrationStore
		models.CallbackEventStore
		models.PortfolioStore
		models.ActionStore
	})
	if !ok || e.skills == nil {
		return nil
	}
	e.callbacks.registry = callback.NewCallbackRegistry(store, e.skills)
	e.callbacks.ingress = callback.NewCallbackIngressService(store, e.skills, map[string]callback.CallbackEventConsumer{
		"approvals": externalconversation.NewApprovalCallbackConsumer(store, e.externalConversations.transport),
		"routines":  externalconversation.NewRoutineCallbackConsumer(e.routineEvents.matcher),
	})
	if e.externalConversations.supervisor != nil && e.externalConversations.config != nil {
		callbackConfig := e.externalConversations.config.Callback
		if strings.TrimSpace(callbackConfig.WorkerID) == "" {
			callbackConfig.WorkerID = strings.TrimSpace(e.externalConversations.config.Inbox.WorkerID) + "-callbacks"
		}
		worker, err := callback.NewCallbackEventWorker(store, e.callbacks.ingress, callbackConfig)
		if err != nil {
			return err
		}
		e.callbacks.worker = worker
		e.externalConversations.supervisor.SetCallbackWorker(worker)
		e.callbacks.ingress.SetDurableDispatcher(e.externalConversations.supervisor.Wake)
	}
	return nil
}

// WithLogger replaces the default logger.
func WithLogger(logger *zap.SugaredLogger) Option {
	return func(e *Engine) error {
		e.logger = logger
		return nil
	}
}

// WithWorkerConcurrencyLimit bounds Agent, conversation, and action execution
// admitted by one Engine process. Replica safety remains durable in the store;
// this protects shared host resources as scope count grows and while rolling
// deployments overlap.
func WithWorkerConcurrencyLimit(limit int) Option {
	return func(e *Engine) error {
		limiter, err := workqueue.NewWorkerLimiter(limit)
		if err != nil {
			return err
		}
		e.workerLimiter = limiter
		return nil
	}
}

// WithAgentRunWorkers adds a scope-bound autonomous portfolio worker pool.
// The option may be repeated for additional scopes or agent deployments.
func WithAgentRunWorkers(config runworker.AgentRunWorkerConfig, resolver conversation.TurnRunnerResolver) Option {
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
	config runworker.DynamicAgentRunWorkerConfig,
	source workqueue.WorkerScopeSource,
	resolver conversation.TurnRunnerResolver,
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

func WithActionPolicy(policy action.ActionPolicyEvaluator) Option {
	return func(e *Engine) error {
		if policy == nil {
			return fmt.Errorf("action policy is required")
		}
		e.actionPolicy = policy
		return nil
	}
}

// WithRoutineOwnerAddresses lets routine briefs grant sends limited to the
// routine owner's own verified email addresses. Without it, owner-only grants
// are neither offered nor applied.
func WithRoutineOwnerAddresses(resolver models.RoutineOwnerAddressResolver) Option {
	return func(e *Engine) error {
		if resolver == nil {
			return fmt.Errorf("routine owner address resolver is required")
		}
		e.routineOwnerAddresses = resolver
		return nil
	}
}

// WithChatFileContent gives the kernel read access to chats' files, stored
// as chat filesystem artifacts with their bytes in content (see pkg/chatfs),
// so a routine can run a saved script: proposing one pins the script's
// current version, and a script routine reports when its file changed.
// Without it, routines cannot run scripts.
func WithChatFileContent(content models.ArtifactContentStore) Option {
	return func(e *Engine) error {
		if content == nil {
			return fmt.Errorf("chat file content store is required")
		}
		e.chatFiles = chatfs.NewStore(e, content)
		return nil
	}
}

// RoutineScriptStatus reports whether a script routine's chat file changed
// since its script was approved. The routine keeps running the approved
// version; the set_script action approves the current one.
func (e *Engine) RoutineScriptStatus(ctx context.Context, scope models.Scope, activationID string) (*routine.RoutineScriptStatus, error) {
	return routine.NewRunbookActivationService(e.store).RoutineScriptStatus(ctx, e.chatFiles, scope, activationID)
}

// RevokeRoutineGrants clears what a routine may do without asking. Only the
// user who asked for the routine may revoke; it is idempotent.
func (e *Engine) RevokeRoutineGrants(ctx context.Context, scope models.Scope, activationID string, req routine.RevokeRoutineGrantsRequest) (*models.RunbookActivation, error) {
	return routine.NewRunbookActivationService(e.store).RevokeGrants(ctx, scope, activationID, req)
}

// WithActionProposalValidators adds deterministic, kernel-owned validation to
// the governed action proposal path. Validators run before policy evaluation
// and before an ApprovalCheckpoint is persisted.
func WithActionProposalValidators(validators ...action.ActionProposalValidator) Option {
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

// WithAgentManagementActions enables the portable, governed self-amendment
// layer for Agent-owned conversation Runs.
func WithAgentManagementActions() Option {
	return func(e *Engine) error {
		e.agentManagementActions = true
		return nil
	}
}

// WithRunbookManagementActions enables conversational starts of existing,
// reviewed Runbook activations. It does not allow models to create or alter
// Runbook definitions, triggers, policy, authority, or budgets.
func WithRunbookManagementActions() Option {
	return func(e *Engine) error {
		e.runbookManagementActions = true
		return nil
	}
}

// WithRunManagementActions enables owner-scoped lifecycle controls for Runs
// referenced by Agent and Team conversations.
func WithRunManagementActions() Option {
	return func(e *Engine) error {
		e.runManagementActions = true
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
	if err := e.skills.Register(context.Background(), skillbinding.SkillManagementSkill()); err != nil && !errors.Is(err, skill.ErrDefinitionImmutable) {
		return err
	}
	validator, err := skillbinding.NewSkillBindingActionValidator(e.skills, e)
	if err != nil {
		return err
	}
	validator.UseAgentRegistry(e.agents)
	e.actionValidators = append(e.actionValidators, validator)
	if e.appCatalog == nil {
		e.appCatalog = &skillbinding.AppCatalogRef{}
	}
	for index := range e.actionPoolSpecs {
		dispatcher, dispatchErr := skillbinding.NewSkillBindingActionDispatcher(e.store, e.skills, e.actionPoolSpecs[index].dispatcher, optionalSkillDiscovery(e.skillDiscovery)...)
		if dispatchErr != nil {
			return dispatchErr
		}
		dispatcher.UseAppCatalog(e.appCatalog)
		dispatcher.UseAgentRegistry(e.agents)
		e.actionPoolSpecs[index].dispatcher = dispatcher
	}
	for index := range e.actionSupervisorSpecs {
		dispatcher, dispatchErr := skillbinding.NewSkillBindingActionDispatcher(e.store, e.skills, e.actionSupervisorSpecs[index].dispatcher, optionalSkillDiscovery(e.skillDiscovery)...)
		if dispatchErr != nil {
			return dispatchErr
		}
		dispatcher.UseAppCatalog(e.appCatalog)
		dispatcher.UseAgentRegistry(e.agents)
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

func (e *Engine) configureAgentManagementActions() error {
	if !e.agentManagementActions {
		return nil
	}
	if err := e.skills.Register(context.Background(), models.AgentManagementSkill()); err != nil && !errors.Is(err, skill.ErrDefinitionImmutable) {
		return err
	}
	validator, err := agentbuilder.NewAgentBehaviorActionValidator(e.agents, e.externalConversations.endpoints)
	if err != nil {
		return err
	}
	validator.SetTeamRoster(e)
	e.actionValidators = append(e.actionValidators, validator)
	for index := range e.actionPoolSpecs {
		dispatcher, dispatchErr := agentbuilder.NewAgentBehaviorActionDispatcher(e.store, e.agents, e.actionPoolSpecs[index].dispatcher, e.externalConversations.endpoints)
		if dispatchErr != nil {
			return dispatchErr
		}
		e.actionPoolSpecs[index].dispatcher = dispatcher
	}
	for index := range e.actionSupervisorSpecs {
		dispatcher, dispatchErr := agentbuilder.NewAgentBehaviorActionDispatcher(e.store, e.agents, e.actionSupervisorSpecs[index].dispatcher, e.externalConversations.endpoints)
		if dispatchErr != nil {
			return dispatchErr
		}
		e.actionSupervisorSpecs[index].dispatcher = dispatcher
	}
	return e.configureAgentBuilderActions()
}

func (e *Engine) configureRunbookManagementActions() error {
	if !e.runbookManagementActions {
		return nil
	}
	if err := e.skills.Register(context.Background(), routine.RunbookManagementSkill()); err != nil && !errors.Is(err, skill.ErrDefinitionImmutable) {
		return err
	}
	store, ok := e.store.(interface {
		models.KernelStore
		models.ConversationStore
	})
	if !ok {
		return errors.New("Runbook management actions require conversation-aware kernel storage")
	}
	validator, err := routine.NewRunbookActionValidator(store)
	if err != nil {
		return err
	}
	validator.SetRoutineTeams(e.teams)
	validator.SetRoutineGrantCatalog(e.skills)
	validator.SetRoutineOwnerAddresses(e.routineOwnerAddresses)
	validator.SetChatFiles(e.chatFiles)
	e.attachRoutineEventValidator(validator)
	e.actionValidators = append(e.actionValidators, validator)
	for index := range e.actionPoolSpecs {
		dispatcher, dispatchErr := routine.NewRunbookActionDispatcher(store, e.actionPoolSpecs[index].dispatcher)
		if dispatchErr != nil {
			return dispatchErr
		}
		dispatcher.SetRoutineTeams(e.teams)
		dispatcher.SetAcceptedRunExecutionPreparer(e)
		dispatcher.SetChatFiles(e.chatFiles)
		e.attachRoutineEventDispatcher(dispatcher)
		e.actionPoolSpecs[index].dispatcher = dispatcher
	}
	for index := range e.actionSupervisorSpecs {
		dispatcher, dispatchErr := routine.NewRunbookActionDispatcher(store, e.actionSupervisorSpecs[index].dispatcher)
		if dispatchErr != nil {
			return dispatchErr
		}
		dispatcher.SetRoutineTeams(e.teams)
		dispatcher.SetAcceptedRunExecutionPreparer(e)
		dispatcher.SetChatFiles(e.chatFiles)
		e.attachRoutineEventDispatcher(dispatcher)
		e.actionSupervisorSpecs[index].dispatcher = dispatcher
	}
	return nil
}

func (e *Engine) configureRunManagementActions() error {
	if !e.runManagementActions {
		return nil
	}
	if err := e.skills.Register(context.Background(), agentrun.RunManagementSkill()); err != nil && !errors.Is(err, skill.ErrDefinitionImmutable) {
		return err
	}
	validator, err := agentrun.NewRunActionValidator(e.store)
	if err != nil {
		return err
	}
	e.actionValidators = append(e.actionValidators, validator)
	for index := range e.actionPoolSpecs {
		dispatcher, dispatchErr := agentrun.NewRunActionDispatcher(e.store, e.actionPoolSpecs[index].dispatcher)
		if dispatchErr != nil {
			return dispatchErr
		}
		e.actionPoolSpecs[index].dispatcher = dispatcher
	}
	for index := range e.actionSupervisorSpecs {
		dispatcher, dispatchErr := agentrun.NewRunActionDispatcher(e.store, e.actionSupervisorSpecs[index].dispatcher)
		if dispatchErr != nil {
			return dispatchErr
		}
		e.actionSupervisorSpecs[index].dispatcher = dispatcher
	}
	return nil
}

func (e *Engine) configureTeamManagementActions() error {
	if !e.teamManagementActions {
		return nil
	}
	if err := e.skills.Register(context.Background(), collaboration.TeamManagementSkill()); err != nil && !errors.Is(err, skill.ErrDefinitionImmutable) {
		return err
	}
	validator, err := collaboration.NewTeamMemberActionValidator(e.teams)
	if err != nil {
		return err
	}
	e.actionValidators = append(e.actionValidators, validator)
	// create_team is a builder action: it runs in the
	// AgentBuilderActionDispatcher that configureAgentBuilderActions wraps
	// around every action worker, so it exists only with Agent management.
	if e.agentManagementActions {
		conversations, _ := e.store.(models.ConversationStore)
		createValidator, err := agentbuilder.NewTeamCreateActionValidator(e.agents, e.teams, e.spaces, conversations, e.store)
		if err != nil {
			return err
		}
		e.actionValidators = append(e.actionValidators, createValidator)
	}
	for index := range e.actionPoolSpecs {
		dispatcher, dispatchErr := collaboration.NewTeamMemberActionDispatcher(e.store, e.teams, e.actionPoolSpecs[index].dispatcher)
		if dispatchErr != nil {
			return dispatchErr
		}
		e.actionPoolSpecs[index].dispatcher = dispatcher
	}
	for index := range e.actionSupervisorSpecs {
		dispatcher, dispatchErr := collaboration.NewTeamMemberActionDispatcher(e.store, e.teams, e.actionSupervisorSpecs[index].dispatcher)
		if dispatchErr != nil {
			return dispatchErr
		}
		e.actionSupervisorSpecs[index].dispatcher = dispatcher
	}
	return nil
}

func WithActionWorkers(config action.ActionWorkerConfig, credentials action.CredentialResolver, dispatcher agentrun.ActionDispatcher) Option {
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
func WithActionCredentialLeaseWorkers(config action.ActionWorkerConfig, issuer action.ActionCredentialLeaseIssuer, dispatcher agentrun.ActionDispatcher) Option {
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
func WithDynamicActionWorkers(config action.DynamicActionWorkerConfig, source workqueue.WorkerScopeSource, credentials action.CredentialResolver, dispatcher agentrun.ActionDispatcher) Option {
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
func WithDynamicActionCredentialLeaseWorkers(config action.DynamicActionWorkerConfig, source workqueue.WorkerScopeSource, issuer action.ActionCredentialLeaseIssuer, dispatcher agentrun.ActionDispatcher) Option {
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

func WithApprovalAuthorizer(authorizer action.ApprovalAuthorizer) Option {
	return func(e *Engine) error {
		if authorizer == nil {
			return fmt.Errorf("approval authorizer is required")
		}
		e.approvalAuth = authorizer
		return nil
	}
}

func (e *Engine) rebuildGovernance() {
	e.actions = action.NewActionCoordinator(e.store, e.store, e.skills, e.actionPolicy, e.actionValidators)
	e.actions.SetRoutineOwnerAddresses(e.routineOwnerAddresses)
	e.approvals = action.NewApprovalCoordinator(e.store, e.store, e.approvalAuth)
}
func (e *Engine) rebuildActionWorkerPools() error {

	e.actionPools = make([]*action.ActionWorkerPool, 0, len(e.actionPoolSpecs))
	for _, spec := range e.actionPoolSpecs {
		var pool *action.ActionWorkerPool
		var err error
		if spec.leaseIssuer != nil {
			pool, err = action.NewActionCredentialLeaseWorkerPool(e.store, e.skills, spec.leaseIssuer, spec.dispatcher, e.logger, spec.config)
		} else {
			pool, err = action.NewActionWorkerPool(e.store, e.skills, spec.credentials, spec.dispatcher, e.logger, spec.config)
		}
		if err != nil {
			return err
		}
		pool.SetWorkerLimiter(e.workerLimiter)
		pool.SetCredentialAvailabilityChecker(e.credentialAvailability)
		e.actionPools = append(e.actionPools, pool)
	}
	return e.buildGovernedToolExecutor()
}

func (e *Engine) rebuildActionWorkerSupervisors() error {
	e.actionSupervisors = make([]*action.ActionWorkerSupervisor, 0, len(e.actionSupervisorSpecs))
	for _, spec := range e.actionSupervisorSpecs {
		var supervisor *action.ActionWorkerSupervisor
		var err error
		if spec.leaseIssuer != nil {
			supervisor, err = action.NewActionCredentialLeaseWorkerSupervisor(e.store, e.skills, spec.leaseIssuer, spec.dispatcher, spec.source, e.logger, spec.config)
		} else {
			supervisor, err = action.NewActionWorkerSupervisor(e.store, e.skills, spec.credentials, spec.dispatcher, spec.source, e.logger, spec.config)
		}
		if err != nil {
			return err
		}
		supervisor.SetWorkerLimiter(e.workerLimiter)
		supervisor.SetCredentialAvailabilityChecker(e.credentialAvailability)
		e.actionSupervisors = append(e.actionSupervisors, supervisor)
	}
	return nil
}

func (e *Engine) rebuildAgentWorkerPools() error {
	finalizer, err := e.newRunTerminalFinalizer()
	if err != nil {
		return err
	}
	e.agentPools = make([]*runworker.AgentRunWorkerPool, 0, len(e.agentPoolSpecs))
	for _, spec := range e.agentPoolSpecs {
		pool, err := runworker.NewAgentRunWorkerPool(e.store, spec.resolver, e.logger, spec.config)
		if err != nil {
			return err
		}
		pool.SetWorkerLimiter(e.workerLimiter)
		pool.SetAcceptedRunExecutionPreparer(e)
		pool.SetActionCoordinator(e.actions)
		pool.SetRunTerminalFinalizer(finalizer)
		e.agentPools = append(e.agentPools, pool)
	}
	return nil
}

func (e *Engine) rebuildAgentWorkerSupervisors() error {
	finalizer, err := e.newRunTerminalFinalizer()
	if err != nil {
		return err
	}
	e.agentSupervisors = make([]*runworker.AgentRunWorkerSupervisor, 0, len(e.agentSupervisorSpecs))
	for _, spec := range e.agentSupervisorSpecs {
		supervisor, err := runworker.NewAgentRunWorkerSupervisor(e.store, spec.resolver, spec.source, e.logger, spec.config)
		if err != nil {
			return err
		}
		supervisor.SetWorkerLimiter(e.workerLimiter)
		supervisor.SetAcceptedRunExecutionPreparer(e)
		supervisor.SetActionCoordinator(e.actions)
		supervisor.SetRunTerminalFinalizer(finalizer)
		e.agentSupervisors = append(e.agentSupervisors, supervisor)
	}
	return nil
}

// newRunTerminalFinalizer is the finalizer every worker pool runs on a
// terminal Run: the try_agent trial hand-off to its builder Run, then the
// Skill resource finalizers when this Engine executes actions.
func (e *Engine) newRunTerminalFinalizer() (agentrun.RunTerminalFinalizer, error) {
	var resources agentrun.RunTerminalFinalizer
	if len(e.actionSupervisorSpecs) > 0 || len(e.actionPoolSpecs) > 0 {
		var dispatcher agentrun.ActionDispatcher
		if len(e.actionSupervisorSpecs) > 0 {
			dispatcher = e.actionSupervisorSpecs[0].dispatcher
		} else {
			dispatcher = e.actionPoolSpecs[0].dispatcher
		}
		resources = agentrun.NewActionRunTerminalFinalizer(e.store, e.skills, dispatcher)
	}
	return agentbuilder.NewAgentTrialFinalizer(e.store, resources, e.wakeAgentWorkersForScope), nil
}

func (e *Engine) WakeAgentWorkers() {
	for _, pool := range e.agentPools {
		pool.Wake()
	}
	for _, supervisor := range e.agentSupervisors {
		supervisor.Wake()
	}
}

// WakeAgentWorkersForScope is a latency hint for hosts that observed newly
// runnable work in one scope, for example a Run committed by another
// process. Workers still claim through the durable store; a missed wake only
// delays work until the next poll.
func (e *Engine) WakeAgentWorkersForScope(scope models.Scope) {
	if e == nil {
		return
	}
	e.wakeAgentWorkersForScope(scope)
}

func (e *Engine) wakeAgentWorkersForScope(scope models.Scope) {
	for _, pool := range e.agentPools {
		pool.WakeScope(scope)
	}
	for _, supervisor := range e.agentSupervisors {
		supervisor.WakeScope(scope)
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

// WakeActionWorkersForScope is a latency hint for hosts that observed newly
// claimable ActionCalls in one scope. Workers still claim through the durable
// store.
func (e *Engine) WakeActionWorkersForScope(scope models.Scope) {
	if e == nil {
		return
	}
	e.wakeActionWorkersForScope(scope)
}

func (e *Engine) wakeActionWorkersForScope(scope models.Scope) {
	for _, pool := range e.actionPools {
		pool.WakeScope(scope)
	}
	for _, supervisor := range e.actionSupervisors {
		supervisor.WakeScope(scope)
	}
}

// BuildGraph is a convenience wrapper for executor.BuildGraph.
func BuildGraph(nodes []*executor.NodeDefinition, connections []*executor.ConnectionDefinition) (*executor.ExecutionGraph, error) {
	return executor.BuildGraph(nodes, connections)
}

func (e *Engine) CreateObjective(ctx context.Context, req portfolio.CreateObjectiveRequest) (*models.Objective, error) {
	return e.portfolio.CreateObjective(ctx, req)
}

func (e *Engine) CreateObjectiveIdempotent(ctx context.Context, req portfolio.CreateObjectiveRequest) (*portfolio.CreateObjectiveResult, error) {
	return e.portfolio.CreateObjectiveIdempotent(ctx, req)
}

func (e *Engine) GetObjective(ctx context.Context, scope models.Scope, objectiveID string) (*models.Objective, error) {
	return e.portfolio.GetObjective(ctx, scope, objectiveID)
}

func (e *Engine) ListObjectives(ctx context.Context, filter models.ObjectiveFilter) ([]*models.Objective, error) {
	return e.portfolio.ListObjectives(ctx, filter)
}

func (e *Engine) UpdateObjective(ctx context.Context, scope models.Scope, objectiveID string, req portfolio.UpdateObjectiveRequest) (*models.Objective, error) {
	return e.portfolio.UpdateObjective(ctx, scope, objectiveID, req)
}

func (e *Engine) CreateRunbookActivation(ctx context.Context, req routine.CreateRunbookActivationRequest) (*models.RunbookActivation, error) {
	return routine.NewRunbookActivationService(e.store).Create(ctx, req)
}

func (e *Engine) GetRunbookActivation(ctx context.Context, scope models.Scope, activationID string) (*models.RunbookActivation, error) {
	return e.store.GetRunbookActivation(ctx, scope, activationID)
}

func (e *Engine) GetRunbookDetail(ctx context.Context, scope models.Scope, activationID string) (*routine.RunbookDetail, error) {
	return routine.ResolveRunbookDetail(ctx, e.store, e.agents, scope, activationID)
}

func (e *Engine) GetRunbookExecutionAudit(ctx context.Context, scope models.Scope, runID string) (*routine.RunbookExecutionAudit, error) {
	if e.runbookAudits == nil {
		return nil, errors.New("Runbook execution audit is unavailable")
	}
	return e.runbookAudits.Get(ctx, scope, runID)
}

// GetRunExecutionAudit projects the canonical graph for conversational,
// delegated, and Runbook-backed Runs through one observation contract.
func (e *Engine) GetRunExecutionAudit(ctx context.Context, scope models.Scope, runID string) (*routine.RunExecutionAudit, error) {
	if e.runbookAudits == nil {
		return nil, errors.New("Run execution audit is unavailable")
	}
	return e.runbookAudits.Get(ctx, scope, runID)
}

func (e *Engine) ListRunbookActivations(ctx context.Context, filter models.RunbookActivationFilter) ([]*models.RunbookActivation, error) {
	return e.store.ListRunbookActivations(ctx, filter)
}

func (e *Engine) UpdateRunbookActivation(ctx context.Context, scope models.Scope, activationID string, req routine.UpdateRunbookActivationRequest) (*models.RunbookActivation, error) {
	// A resume re-runs the routine's source check with the host's Google catalog.
	return routine.NewRunbookActivationService(e.store).WithRoutineEventSources(e.routineEvents.sources).Update(ctx, scope, activationID, req)
}

func (e *Engine) StartRunbookActivation(ctx context.Context, scope models.Scope, activationID string, req routine.StartRunbookActivationRequest) (*agentrun.AgentRunCommandResult, error) {
	return routine.StartRunbookActivationWithExecutionPreparer(ctx, e.store, scope, activationID, req, e)
}

func (e *Engine) ReconcileRunbookSchedules(ctx context.Context, scope models.Scope, limit int) (*routine.RunbookScheduleResult, error) {
	scheduler := routine.NewRunbookScheduler(e.store)
	scheduler.SetAcceptedRunExecutionPreparer(e)
	return scheduler.ReconcileScope(ctx, scope, limit)
}

func (e *Engine) ReconcileAllRunbookSchedules(ctx context.Context, limitPerScope int) (*routine.RunbookScheduleResult, error) {
	scheduler := routine.NewRunbookScheduler(e.store)
	scheduler.SetAcceptedRunExecutionPreparer(e)
	return scheduler.ReconcileAll(ctx, limitPerScope)
}

func (e *Engine) CreateAgentRun(ctx context.Context, req models.CreateAgentRunRequest) (*models.AgentRun, error) {
	result, err := e.CreateAgentRunCommand(ctx, req)
	if err != nil {
		return nil, err
	}
	return result.Run, nil
}

// CreateAgentRunCommand returns both the durable run and its creation event.
// Idempotent replays return the existing run with a nil event.
func (e *Engine) CreateAgentRunCommand(ctx context.Context, req models.CreateAgentRunRequest) (*agentrun.AgentRunCommandResult, error) {
	if err := req.Scope.Validate(); err != nil {
		return nil, err
	}
	if err := req.Owner.Validate(); err != nil {
		return nil, err
	}
	// An accepted delivery retry keeps its original immutable method even if
	// the active deployment changed. Request-supplied snapshots are discarded.
	req.Context = maps.Clone(req.Context)
	delete(req.Context, models.AcceptedRunExecutionContextKey)
	if strings.TrimSpace(req.IdempotencyKey) != "" {
		if replay, err := agentrun.NewRunCommandService(e.store).FindCreatedAgentRun(ctx, req); err != nil || replay != nil {
			return replay, err
		}
	}
	if err := e.ValidateAgentRunEntrypoint(ctx, req.Scope, req.AssignedAgentID, req.Entrypoint); err != nil {
		return nil, err
	}
	if err := e.PrepareAcceptedRunExecution(ctx, &req); err != nil {
		return nil, err
	}
	return agentrun.NewRunCommandService(e.store).CreateAgentRun(ctx, req)
}

// ValidateAgentRunEntrypoint proves that an advertised runbook entrypoint is
// executable by the Agent's currently active immutable definition. Callers can
// use it for capability discovery; CreateAgentRunCommand enforces it again at
// the command boundary so stale UI or model-tool state fails before a Run is
// persisted.
func (e *Engine) ValidateAgentRunEntrypoint(ctx context.Context, scope models.Scope, agentID, entrypoint string) error {
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

func (e *Engine) CommandAgentRun(ctx context.Context, req agentrun.AgentRunCommandRequest) (*agentrun.AgentRunCommandResult, error) {
	return agentrun.NewRunCommandService(e.store).CommandAgentRun(ctx, req)
}

func (e *Engine) ConversationRetryEligibility(ctx context.Context, scope models.Scope, runID string) (*agentrun.RunRetryEligibility, error) {
	return agentrun.NewRunCommandService(e.store).ConversationRetryEligibility(ctx, scope, runID)
}

// ConversationTasksAvailable reports whether this store implements the optional
// durable conversation task contract.
func (e *Engine) ConversationTasksAvailable() bool {
	if e == nil {
		return false
	}
	_, ok := e.store.(models.ConversationTaskKernelStore)
	return ok
}

func (e *Engine) conversationTaskService() (*conversationrun.ConversationTaskService, error) {
	if e == nil {
		return nil, models.ErrInvalidConversationTask
	}
	store, ok := e.store.(models.ConversationTaskKernelStore)
	if !ok {
		return nil, errors.New("conversation task store is not configured")
	}
	service := conversationrun.NewConversationTaskService(store)
	service.SetAcceptedRunExecutionPreparer(e)
	return service, nil
}

func (e *Engine) GetConversationTask(ctx context.Context, scope models.Scope, id string) (*models.ConversationTask, error) {
	if e == nil {
		return nil, models.ErrInvalidConversationTask
	}
	store, ok := e.store.(models.ConversationTaskStore)
	if !ok {
		return nil, errors.New("conversation task store is not configured")
	}
	return store.GetConversationTask(ctx, scope, id)
}

func (e *Engine) FindConversationTaskByWorkRunID(ctx context.Context, scope models.Scope, id string) (*models.ConversationTask, error) {
	if e == nil {
		return nil, models.ErrInvalidConversationTask
	}
	store, ok := e.store.(models.ConversationTaskStore)
	if !ok {
		return nil, errors.New("conversation task store is not configured")
	}
	return store.FindConversationTaskByWorkRunID(ctx, scope, id)
}

func (e *Engine) GetConversationTaskSnapshot(ctx context.Context, request conversationrun.GetConversationTaskRequest) (*models.ConversationTaskResult, error) {
	service, err := e.conversationTaskService()
	if err != nil {
		return nil, err
	}
	return service.Get(ctx, request)
}

func (e *Engine) ListConversationTasks(ctx context.Context, filter models.ConversationTaskFilter) ([]*models.ConversationTaskResult, error) {
	service, err := e.conversationTaskService()
	if err != nil {
		return nil, err
	}
	return service.List(ctx, filter)
}

func (e *Engine) CancelConversationTask(ctx context.Context, request conversationrun.CancelConversationTaskRequest) (*models.ConversationTaskResult, error) {
	service, err := e.conversationTaskService()
	if err != nil {
		return nil, err
	}
	return service.Cancel(ctx, request)
}

func (e *Engine) GetAgentRun(ctx context.Context, scope models.Scope, runID string) (*models.AgentRun, error) {
	return e.portfolio.GetAgentRun(ctx, scope, runID)
}

func (e *Engine) ListAgentRuns(ctx context.Context, filter models.AgentRunFilter) ([]*models.AgentRun, error) {
	return e.portfolio.ListAgentRuns(ctx, filter)
}

func (e *Engine) SummarizeAgentRuns(ctx context.Context, scope models.Scope, owners []models.ObjectiveOwner) ([]models.AgentRunOwnerSummary, error) {
	return e.portfolio.SummarizeAgentRuns(ctx, scope, owners)
}

func (e *Engine) RegisterArtifact(ctx context.Context, request artifactcatalog.RegisterArtifactRequest) (*artifactcatalog.ArtifactRegistrationResult, error) {
	if e.artifacts == nil {
		return nil, fmt.Errorf("artifact store is not configured")
	}
	return e.artifacts.Register(ctx, request)
}

func (e *Engine) GetArtifact(ctx context.Context, scope models.Scope, artifactID string, version int64) (*models.Artifact, error) {
	if e.artifacts == nil {
		return nil, fmt.Errorf("artifact store is not configured")
	}
	return e.artifacts.Get(ctx, scope, artifactID, version)
}

func (e *Engine) ListArtifacts(ctx context.Context, filter models.ArtifactFilter) ([]*models.Artifact, error) {
	if e.artifacts == nil {
		return nil, fmt.Errorf("artifact store is not configured")
	}
	return e.artifacts.List(ctx, filter)
}

func (e *Engine) TransitionAgentRun(ctx context.Context, scope models.Scope, runID string, req activity.RunTransitionRequest) (*models.AgentRun, *models.ActivityEvent, error) {
	return e.activity.TransitionRun(ctx, scope, runID, req)
}

func (e *Engine) AppendActivity(ctx context.Context, event *models.ActivityEvent) (*models.ActivityEvent, error) {
	return e.activity.AppendActivity(ctx, event)
}

func (e *Engine) ListActivity(ctx context.Context, filter models.ActivityFilter) ([]*models.ActivityEvent, error) {
	return e.activity.ListActivity(ctx, filter)
}

func (e *Engine) ListActivityFeed(ctx context.Context, request activity.ActivityFeedRequest) (*activity.ActivityFeedPage, error) {
	return e.activity.ListActivityFeed(ctx, request)
}

func (e *Engine) CreateConversation(ctx context.Context, request conversation.CreateConversationRequest) (*models.Conversation, bool, error) {
	if e.conversations == nil {
		return nil, false, fmt.Errorf("conversation store is not configured")
	}
	return e.conversations.CreateConversation(ctx, request)
}

func (e *Engine) GetConversation(ctx context.Context, scope models.Scope, conversationID string) (*models.Conversation, error) {
	if e.conversations == nil {
		return nil, fmt.Errorf("conversation store is not configured")
	}
	return e.conversations.GetConversation(ctx, scope, conversationID)
}

func (e *Engine) UpdateConversation(ctx context.Context, request conversation.UpdateConversationRequest) (*models.Conversation, error) {
	if e.conversations == nil {
		return nil, fmt.Errorf("conversation store is not configured")
	}
	return e.conversations.UpdateConversation(ctx, request)
}

// SetConversationApprovalMode is the governed, revision-checked change of a
// conversation's approval mode. The host must first authorize the caller as a
// principal allowed to execute the conversation's Agent. Loosening the mode
// approves pending checkpoints in the conversation that the new mode allows.
func (e *Engine) SetConversationApprovalMode(ctx context.Context, request conversation.SetConversationApprovalModeRequest) (*conversation.SetConversationApprovalModeResult, error) {
	store, ok := e.store.(models.ConversationApprovalModeKernelStore)
	if !ok || e.conversations == nil {
		return nil, fmt.Errorf("conversation approval modes are not supported by this store")
	}
	return conversation.NewConversationApprovalModeService(store).SetApprovalMode(ctx, request)
}

func (e *Engine) ListConversations(ctx context.Context, filter models.ConversationFilter) ([]*models.Conversation, error) {
	if e.conversations == nil {
		return nil, fmt.Errorf("conversation store is not configured")
	}
	return e.conversations.ListConversations(ctx, filter)
}

// LastConversationActivity returns, by owner id, the newest UpdatedAt of each
// owner's active conversations that hold a message; an owner with none is
// absent. One store query answers for every owner.
func (e *Engine) LastConversationActivity(ctx context.Context, scope models.Scope, ownerType models.OwnerType, ownerIDs []string) (map[string]time.Time, error) {
	if e.conversations == nil {
		return nil, fmt.Errorf("conversation store is not configured")
	}
	return e.conversations.LastConversationActivity(ctx, scope, ownerType, ownerIDs)
}

var errSpacesUnsupported = errors.New("spaces are not supported by this store")

// ListSpaces returns the scope's spaces by position. It creates none.
func (e *Engine) ListSpaces(ctx context.Context, scope models.Scope) ([]*models.Space, error) {
	if e.spaces == nil {
		return nil, errSpacesUnsupported
	}
	return e.spaces.ListSpaces(ctx, scope)
}

// DefaultSpace returns the owner's default space in the scope: the first
// space they created there. An owner with no space gets ErrNoDefaultSpace.
func (e *Engine) DefaultSpace(ctx context.Context, scope models.Scope, ownerID string) (*models.Space, error) {
	if e.spaces == nil {
		return nil, errSpacesUnsupported
	}
	return e.spaces.DefaultSpace(ctx, scope, ownerID)
}

func (e *Engine) GetSpace(ctx context.Context, scope models.Scope, spaceID string) (*models.Space, error) {
	if e.spaces == nil {
		return nil, errSpacesUnsupported
	}
	return e.spaces.GetSpace(ctx, scope, spaceID)
}

func (e *Engine) CreateSpace(ctx context.Context, request portfolio.CreateSpaceRequest) (*models.Space, error) {
	if e.spaces == nil {
		return nil, errSpacesUnsupported
	}
	return e.spaces.CreateSpace(ctx, request)
}

func (e *Engine) UpdateSpace(ctx context.Context, request portfolio.UpdateSpaceRequest) (*models.Space, error) {
	if e.spaces == nil {
		return nil, errSpacesUnsupported
	}
	return e.spaces.UpdateSpace(ctx, request)
}

// DeleteSpace deletes a space that is not a default space. Its conversations,
// and the deployments that are in no other space, move to the default space
// of its owner.
func (e *Engine) DeleteSpace(ctx context.Context, scope models.Scope, spaceID string) error {
	if e.spaces == nil {
		return errSpacesUnsupported
	}
	return e.spaces.DeleteSpace(ctx, scope, spaceID)
}

func (e *Engine) ListSpaceMembers(ctx context.Context, scope models.Scope, spaceID string) ([]*models.SpaceMember, error) {
	if e.spaces == nil {
		return nil, errSpacesUnsupported
	}
	return e.spaces.ListSpaceMembers(ctx, scope, spaceID)
}

// PutSpaceMember adds an Agent or Team deployment to a space; a Team brings
// the Agents on its roster. The host must first verify the deployment belongs
// to the scope.
func (e *Engine) PutSpaceMember(ctx context.Context, request portfolio.PutSpaceMemberRequest) (*models.SpaceMember, bool, error) {
	if e.spaces == nil {
		return nil, false, errSpacesUnsupported
	}
	member, created, err := e.spaces.PutSpaceMember(ctx, request)
	// An Agent created in no space got no control conversation; its first
	// space gives it one.
	if err == nil && created && member.Type == models.SpaceMemberAgent && e.agents != nil {
		if deployment, getErr := e.agents.GetDeployment(ctx, skill.ScopeReference{Kind: request.Scope.Kind, ID: request.Scope.ID}, member.ID); getErr == nil {
			e.ensureAgentControlConversationAfterWrite(ctx, deployment, "")
		}
	}
	return member, created, err
}

// DeleteSpaceMember takes a deployment out of a space. An Agent also leaves
// the rosters of the Teams in that space, and a Team that cannot lose it is
// archived; until the request confirms that, nothing changes and the error is
// a *SpaceAgentRemovalNeedsConfirmation.
func (e *Engine) DeleteSpaceMember(ctx context.Context, request portfolio.DeleteSpaceMemberRequest) error {
	if e.spaces == nil {
		return errSpacesUnsupported
	}
	return e.spaces.DeleteSpaceMember(ctx, request)
}

// ListScopeSpaceMembers returns every space membership of the scope: which
// spaces each Agent and Team is in.
func (e *Engine) ListScopeSpaceMembers(ctx context.Context, scope models.Scope) ([]*models.SpaceMember, error) {
	if e.spaces == nil {
		return nil, errSpacesUnsupported
	}
	return e.spaces.ListScopeSpaceMembers(ctx, scope)
}

// SetSpaceAccess changes who may see the space.
func (e *Engine) SetSpaceAccess(ctx context.Context, scope models.Scope, spaceID string, access models.SpaceAccess) (*models.Space, error) {
	if e.spaces == nil {
		return nil, errSpacesUnsupported
	}
	return e.spaces.SetSpaceAccess(ctx, scope, spaceID, access)
}

// SetSpaceOwner hands a space to another owner. The host must first verify
// the person belongs to the scope. Each of the two owners then has the oldest
// space they own as their default space.
func (e *Engine) SetSpaceOwner(ctx context.Context, scope models.Scope, spaceID, ownerID string) (*models.Space, error) {
	if e.spaces == nil {
		return nil, errSpacesUnsupported
	}
	return e.spaces.SetSpaceOwner(ctx, scope, spaceID, ownerID)
}

// SetSpaceAccessState replaces the space's owner, access and people at once.
// The host must first verify the owner and people belong to the scope.
func (e *Engine) SetSpaceAccessState(ctx context.Context, request portfolio.SetSpaceAccessStateRequest) (*models.Space, error) {
	if e.spaces == nil {
		return nil, errSpacesUnsupported
	}
	return e.spaces.SetSpaceAccessState(ctx, request)
}

// AddSpacePerson adds a person to a space. The host must first verify the
// person belongs to the scope.
func (e *Engine) AddSpacePerson(ctx context.Context, request portfolio.AddSpacePersonRequest) (*models.SpacePerson, bool, error) {
	if e.spaces == nil {
		return nil, false, errSpacesUnsupported
	}
	return e.spaces.AddSpacePerson(ctx, request)
}

func (e *Engine) RemoveSpacePerson(ctx context.Context, scope models.Scope, spaceID, userID string) error {
	if e.spaces == nil {
		return errSpacesUnsupported
	}
	return e.spaces.RemoveSpacePerson(ctx, scope, spaceID, userID)
}

func (e *Engine) ListSpacePeople(ctx context.Context, scope models.Scope, spaceID string) ([]*models.SpacePerson, error) {
	if e.spaces == nil {
		return nil, errSpacesUnsupported
	}
	return e.spaces.ListSpacePeople(ctx, scope, spaceID)
}

// VisibleSpaces returns the ids of the spaces the person may see: the ones
// they own, the ones open to everyone, and the ones they were added to.
func (e *Engine) VisibleSpaces(ctx context.Context, scope models.Scope, userID string) ([]string, error) {
	if e.spaces == nil {
		return nil, errSpacesUnsupported
	}
	return e.spaces.VisibleSpaces(ctx, scope, userID)
}

// CanEditSpace reports whether the person owns the space.
func (e *Engine) CanEditSpace(ctx context.Context, scope models.Scope, spaceID, userID string) (bool, error) {
	if e.spaces == nil {
		return false, errSpacesUnsupported
	}
	return e.spaces.CanEditSpace(ctx, scope, spaceID, userID)
}

// CanManageSpace reports whether the person may change the space's access
// and people: its owner, or a tenant admin who can see it.
func (e *Engine) CanManageSpace(ctx context.Context, scope models.Scope, spaceID, userID string, isAdmin bool) (bool, error) {
	if e.spaces == nil {
		return false, errSpacesUnsupported
	}
	return e.spaces.CanManageSpace(ctx, scope, spaceID, userID, isAdmin)
}

func (e *Engine) CreateEmbedInstallation(ctx context.Context, request conversation.CreateEmbedInstallationRequest) (*models.EmbedInstallation, error) {
	if e.embeds == nil {
		return nil, fmt.Errorf("embed store is not configured")
	}
	return e.embeds.CreateInstallation(ctx, request)
}

func (e *Engine) GetEmbedInstallation(ctx context.Context, scope models.Scope, id string) (*models.EmbedInstallation, error) {
	if e.embeds == nil {
		return nil, fmt.Errorf("embed store is not configured")
	}
	return e.embeds.GetInstallation(ctx, scope, id)
}

func (e *Engine) ListEmbedInstallations(ctx context.Context, filter models.EmbedInstallationFilter) ([]*models.EmbedInstallation, error) {
	if e.embeds == nil {
		return nil, fmt.Errorf("embed store is not configured")
	}
	return e.embeds.ListInstallations(ctx, filter)
}

func (e *Engine) UpdateEmbedInstallation(ctx context.Context, request conversation.UpdateEmbedInstallationRequest) (*models.EmbedInstallation, error) {
	if e.embeds == nil {
		return nil, fmt.Errorf("embed store is not configured")
	}
	return e.embeds.UpdateInstallation(ctx, request)
}

func (e *Engine) OpenEmbedSession(ctx context.Context, request conversation.OpenEmbedSessionRequest) (*conversation.OpenEmbedSessionResult, error) {
	if e.embeds == nil {
		return nil, fmt.Errorf("embed store is not configured")
	}
	return e.embeds.OpenSession(ctx, request)
}

func (e *Engine) AuthenticateEmbedSession(ctx context.Context, route, sessionID, capability, origin string) (*models.EmbedInstallation, *models.EmbedSession, error) {
	if e.embeds == nil {
		return nil, nil, fmt.Errorf("embed store is not configured")
	}
	return e.embeds.Authenticate(ctx, route, sessionID, capability, origin)
}

func (e *Engine) ConsumeEmbedSessionMessage(ctx context.Context, installation *models.EmbedInstallation, session *models.EmbedSession) (*models.EmbedSession, error) {
	if e.embeds == nil {
		return nil, fmt.Errorf("embed store is not configured")
	}
	return e.embeds.ConsumeMessage(ctx, installation, session)
}

// ImportConversationArchive migrates user-visible historical collaboration
// facts without scheduling new conversation Runs for old messages.
func (e *Engine) ImportConversationArchive(ctx context.Context, request conversation.ImportConversationArchiveRequest) (*conversation.ConversationArchiveImportResult, error) {
	if e.conversations == nil {
		return nil, fmt.Errorf("conversation store is not configured")
	}
	return e.conversations.ImportArchive(ctx, request)
}

func (e *Engine) PostChannelMessage(ctx context.Context, request conversation.PostChannelMessageRequest) (*models.ChannelMessageCommitResult, error) {
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
		e.conversationRunReconciler.WakeScope(result.Message.Scope)
	}
	return result, nil
}

func (e *Engine) GetChannelMessage(ctx context.Context, scope models.Scope, conversationID, messageID string) (*models.ChannelMessage, error) {
	if e.conversations == nil {
		return nil, fmt.Errorf("conversation store is not configured")
	}
	return e.conversations.GetChannelMessage(ctx, scope, conversationID, messageID)
}

func (e *Engine) FindParticipationRoundByIdempotencyKey(ctx context.Context, scope models.Scope, conversationID, key string) (*models.ParticipationRoundResult, error) {
	if e.conversations == nil {
		return nil, fmt.Errorf("conversation store is not configured")
	}
	return e.conversations.FindParticipationRoundByIdempotencyKey(ctx, scope, conversationID, key)
}

func (e *Engine) ListChannelMessages(ctx context.Context, filter models.ChannelMessageFilter) ([]*models.ChannelMessage, error) {
	if e.conversations == nil {
		return nil, fmt.Errorf("conversation store is not configured")
	}
	return e.conversations.ListChannelMessages(ctx, filter)
}

func (e *Engine) GetVisibleChannelMessage(ctx context.Context, scope models.Scope, conversationID, messageID string, viewer models.ConversationViewer) (*models.ChannelMessage, error) {
	if e.conversations == nil {
		return nil, fmt.Errorf("conversation store is not configured")
	}
	return e.conversations.GetVisibleChannelMessage(ctx, scope, conversationID, messageID, viewer)
}

func (e *Engine) ListConversationChanges(ctx context.Context, request conversation.ConversationChangeRequest) (*conversation.ConversationChangeSet, error) {
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

func (e *Engine) ConversationCoordinationAvailable() bool {
	return e != nil && e.conversationCoordinator != nil
}

// ConversationRunsAvailable reports whether committed Team messages are
// automatically projected into durable, recoverable conversation Runs.
func (e *Engine) ConversationRunsAvailable() bool {
	return e != nil && e.conversationRunScheduler != nil && e.conversationRunReconciler != nil
}

// ReceiveExternalConversationEvent persists an event already verified and
// normalized by the exact Skill-owned adapter.
func (e *Engine) ReceiveExternalConversationEvent(
	ctx context.Context,
	request externalconversation.ReceiveExternalConversationEventRequest,
) (*externalconversation.ReceiveExternalConversationEventResult, error) {
	if e == nil || e.externalConversations.transport == nil {
		return nil, fmt.Errorf("external conversation transport is not configured")
	}
	result, err := e.externalConversations.transport.Receive(ctx, request)
	if err == nil && e.externalConversations.supervisor != nil {
		e.externalConversations.supervisor.WakeScope(request.Scope)
	}
	return result, err
}

func (e *Engine) NormalizeExternalConversationIngress(
	ctx context.Context,
	request externalconversation.ExternalConversationIngressRequest,
	host externalconversation.ExternalConversationIngressAdapterHost,
) (*externalconversation.ExternalConversationIngressResult, error) {
	if e == nil || e.externalConversations.transport == nil {
		return nil, fmt.Errorf("external conversation transport is not configured")
	}
	result, err := e.externalConversations.transport.NormalizeExternalConversationIngress(ctx, request, host)
	if err == nil && len(result.Received) > 0 {
		e.wakeExternalConversationsForReceived(result.Received)
	}
	return result, err
}

func (e *Engine) NormalizeExternalConversationPublicIngress(
	ctx context.Context,
	request externalconversation.ExternalConversationPublicIngressRequest,
	host externalconversation.ExternalConversationIngressAdapterHost,
) (*externalconversation.ExternalConversationIngressResult, error) {
	if e == nil || e.externalConversations.transport == nil {
		return nil, fmt.Errorf("external conversation transport is not configured")
	}
	result, err := e.externalConversations.transport.NormalizeExternalConversationPublicIngress(ctx, request, host)
	if err == nil && len(result.Received) > 0 {
		e.wakeExternalConversationsForReceived(result.Received)
	}
	return result, err
}

func (e *Engine) NormalizeExternalConversationGatewayIngress(
	ctx context.Context,
	gateway models.ExternalConversationIngressGateway,
	request externalconversation.ExternalConversationPublicIngressRequest,
	host externalconversation.ExternalConversationGatewayAdapterHost,
) (*externalconversation.ExternalConversationGatewayIngressResult, error) {
	if e == nil || e.externalConversations.transport == nil {
		return nil, fmt.Errorf("external conversation transport is not configured")
	}
	result, err := e.externalConversations.transport.NormalizeExternalConversationGatewayIngress(
		ctx, gateway, request, host,
	)
	if err == nil && len(result.Received) > 0 {
		e.wakeExternalConversationsForReceived(result.Received)
	}
	return result, err
}

func (e *Engine) NormalizeExternalConversationRegisteredGatewayIngress(
	ctx context.Context,
	request externalconversation.ExternalConversationPublicIngressRequest,
	host externalconversation.ExternalConversationGatewayAdapterHost,
) (*externalconversation.ExternalConversationGatewayIngressResult, error) {
	if e == nil || e.externalConversations.transport == nil {
		return nil, fmt.Errorf("external conversation transport is not configured")
	}
	result, err := e.externalConversations.transport.NormalizeExternalConversationRegisteredGatewayIngress(
		ctx, request, host,
	)
	if err == nil && len(result.Received) > 0 {
		e.wakeExternalConversationsForReceived(result.Received)
	}
	return result, err
}

func (e *Engine) CreateExternalConversationGateway(
	ctx context.Context,
	request externalconversation.CreateExternalConversationGatewayRequest,
) (*models.ExternalConversationGatewayRegistration, error) {
	if e == nil || e.externalConversations.gateways == nil {
		return nil, fmt.Errorf("external conversation gateways are not configured")
	}
	// A tenant gateway serves one existing Agent or Team; a platform gateway
	// lives in the host's own scope.
	if gateway := request.Gateway; gateway.Scope.Kind != "platform" {
		if err := e.checkUmbrella(ctx, skill.ScopeReference{Kind: gateway.Scope.Kind, ID: gateway.Scope.ID}, gateway.Owner); err != nil {
			return nil, fmt.Errorf("external conversation gateway owner: %w", err)
		}
	}
	return e.externalConversations.gateways.Create(ctx, request)
}

// CreateExternalConversationEndpoint registers one provider-neutral
// conversation destination against an exact, reviewed Skill binding revision.
func (e *Engine) CreateExternalConversationEndpoint(
	ctx context.Context,
	request externalconversation.CreateExternalConversationEndpointRequest,
) (*models.ExternalConversationEndpoint, error) {
	if e == nil || e.externalConversations.endpoints == nil {
		return nil, fmt.Errorf("external conversation endpoints are not configured")
	}
	if err := e.checkUmbrella(ctx, skill.ScopeReference{Kind: request.Scope.Kind, ID: request.Scope.ID}, request.Owner); err != nil {
		return nil, fmt.Errorf("external conversation endpoint owner: %w", err)
	}
	return e.externalConversations.endpoints.Create(ctx, request)
}

// GetExternalConversationEndpoint returns one tenant-scoped endpoint.
func (e *Engine) GetExternalConversationEndpoint(
	ctx context.Context,
	scope models.Scope,
	id string,
) (*models.ExternalConversationEndpoint, error) {
	if e == nil || e.externalConversations.endpoints == nil {
		return nil, fmt.Errorf("external conversation endpoints are not configured")
	}
	return e.externalConversations.endpoints.Get(ctx, scope, id)
}

// ListExternalConversationEndpoints returns endpoints visible in one exact
// scope. Provider credentials never enter this portable desired-state API.
func (e *Engine) ListExternalConversationEndpoints(
	ctx context.Context,
	filter models.ExternalConversationEndpointFilter,
) ([]*models.ExternalConversationEndpoint, error) {
	if e == nil || e.externalConversations.endpoints == nil {
		return nil, fmt.Errorf("external conversation endpoints are not configured")
	}
	return e.externalConversations.endpoints.List(ctx, filter)
}

// UpdateExternalConversationEndpoint applies a revision-checked lifecycle or
// configuration change without changing the pinned Skill adapter revision.
func (e *Engine) UpdateExternalConversationEndpoint(
	ctx context.Context,
	scope models.Scope,
	id string,
	request externalconversation.UpdateExternalConversationEndpointRequest,
) (*models.ExternalConversationEndpoint, error) {
	if e == nil || e.externalConversations.endpoints == nil {
		return nil, fmt.Errorf("external conversation endpoints are not configured")
	}
	return e.externalConversations.endpoints.Update(ctx, scope, id, request)
}

func (e *Engine) GetExternalConversationGateway(
	ctx context.Context,
	scope models.Scope,
	id string,
) (*models.ExternalConversationGatewayRegistration, error) {
	if e == nil || e.externalConversations.gateways == nil {
		return nil, fmt.Errorf("external conversation gateways are not configured")
	}
	return e.externalConversations.gateways.Get(ctx, scope, id)
}

func (e *Engine) ListExternalConversationGateways(
	ctx context.Context,
	filter models.ExternalConversationGatewayFilter,
) ([]*models.ExternalConversationGatewayRegistration, error) {
	if e == nil || e.externalConversations.gateways == nil {
		return nil, fmt.Errorf("external conversation gateways are not configured")
	}
	return e.externalConversations.gateways.List(ctx, filter)
}

func (e *Engine) UpdateExternalConversationGateway(
	ctx context.Context,
	scope models.Scope,
	id string,
	request externalconversation.UpdateExternalConversationGatewayRequest,
) (*models.ExternalConversationGatewayRegistration, error) {
	if e == nil || e.externalConversations.gateways == nil {
		return nil, fmt.Errorf("external conversation gateways are not configured")
	}
	return e.externalConversations.gateways.Update(ctx, scope, id, request)
}

func (e *Engine) CreateCallbackRegistration(
	ctx context.Context,
	request callback.CreateCallbackRegistrationRequest,
) (*models.CallbackRegistration, error) {
	if e == nil || e.callbacks.registry == nil {
		return nil, fmt.Errorf("callback registry is not configured")
	}
	return e.callbacks.registry.Create(ctx, request)
}

func (e *Engine) GetCallbackRegistration(
	ctx context.Context,
	scope models.Scope,
	id string,
) (*models.CallbackRegistration, error) {
	if e == nil || e.callbacks.registry == nil {
		return nil, fmt.Errorf("callback registry is not configured")
	}
	return e.callbacks.registry.Get(ctx, scope, id)
}

func (e *Engine) ListCallbackRegistrations(
	ctx context.Context,
	filter models.CallbackRegistrationFilter,
) ([]*models.CallbackRegistration, error) {
	if e == nil || e.callbacks.registry == nil {
		return nil, fmt.Errorf("callback registry is not configured")
	}
	return e.callbacks.registry.List(ctx, filter)
}

func (e *Engine) UpdateCallbackRegistration(
	ctx context.Context,
	scope models.Scope,
	id string,
	request callback.UpdateCallbackRegistrationRequest,
) (*models.CallbackRegistration, error) {
	if e == nil || e.callbacks.registry == nil {
		return nil, fmt.Errorf("callback registry is not configured")
	}
	return e.callbacks.registry.Update(ctx, scope, id, request)
}

func (e *Engine) ReceiveCallback(
	ctx context.Context,
	request callback.CallbackPublicRequest,
	host callback.CallbackAdapterHost,
) (*callback.CallbackIngressResult, error) {
	if e == nil || e.callbacks.ingress == nil {
		return nil, fmt.Errorf("callback ingress is not configured")
	}
	return e.callbacks.ingress.Receive(ctx, request, host)
}

func (e *Engine) CallbackRegistryAvailable() bool {
	return e != nil && e.callbacks.registry != nil && e.callbacks.ingress != nil
}

func (e *Engine) EnqueueExternalConversationDelivery(
	ctx context.Context,
	request externalconversation.EnqueueExternalConversationDeliveryRequest,
) (*externalconversation.EnqueueExternalConversationDeliveryResult, error) {
	if e == nil || e.externalConversations.transport == nil {
		return nil, fmt.Errorf("external conversation transport is not configured")
	}
	result, err := e.externalConversations.transport.Enqueue(ctx, request)
	if err == nil && e.externalConversations.supervisor != nil {
		e.externalConversations.supervisor.WakeScope(request.Scope)
	}
	return result, err
}

func (e *Engine) ExternalConversationTransportAvailable() bool {
	return e != nil && e.externalConversations.transport != nil
}

func (e *Engine) ExternalConversationWorkersAvailable() bool {
	return e != nil && e.externalConversations.supervisor != nil
}

func (e *Engine) GetParticipationRound(ctx context.Context, scope models.Scope, conversationID, roundID string) (*models.ParticipationRoundResult, error) {
	if e.conversations == nil {
		return nil, fmt.Errorf("conversation store is not configured")
	}
	return e.conversations.GetParticipationRound(ctx, scope, conversationID, roundID)
}

func (e *Engine) ListParticipationRounds(ctx context.Context, filter models.ParticipationRoundFilter) ([]*models.ParticipationRoundResult, error) {
	if e.conversations == nil {
		return nil, fmt.Errorf("conversation store is not configured")
	}
	return e.conversations.ListParticipationRounds(ctx, filter)
}

func (e *Engine) AdvanceConversationCursor(ctx context.Context, request conversation.AdvanceConversationCursorRequest) (*models.ConversationCursor, bool, error) {
	if e.conversations == nil {
		return nil, false, fmt.Errorf("conversation store is not configured")
	}
	return e.conversations.AdvanceCursor(ctx, request)
}

func (e *Engine) GetConversationCursor(ctx context.Context, scope models.Scope, conversationID string, participant models.ConversationParticipant) (*models.ConversationCursor, error) {
	if e.conversations == nil {
		return nil, fmt.Errorf("conversation store is not configured")
	}
	return e.conversations.GetCursor(ctx, scope, conversationID, participant)
}

func (e *Engine) ListConversationCursors(ctx context.Context, scope models.Scope, conversationID string) ([]*models.ConversationCursor, error) {
	if e == nil || e.conversations == nil {
		return nil, errors.New("conversation service is not configured")
	}
	return e.conversations.ListCursors(ctx, scope, conversationID)
}

func (e *Engine) SetConversationPresence(ctx context.Context, request conversation.SetConversationPresenceRequest) (*models.ConversationPresence, error) {
	if e.conversations == nil {
		return nil, fmt.Errorf("conversation store is not configured")
	}
	return e.conversations.SetPresence(ctx, request)
}

func (e *Engine) ReleaseConversationPresence(ctx context.Context, request models.ReleaseConversationPresenceRequest) error {
	if e.conversations == nil {
		return fmt.Errorf("conversation store is not configured")
	}
	return e.conversations.ReleasePresence(ctx, request)
}

func (e *Engine) ListConversationPresence(ctx context.Context, scope models.Scope, conversationID string) ([]*models.ConversationPresence, error) {
	if e.conversations == nil {
		return nil, fmt.Errorf("conversation store is not configured")
	}
	return e.conversations.ListPresence(ctx, scope, conversationID)
}

func (e *Engine) CreateRunDependencyGroup(ctx context.Context, request models.CreateRunDependencyGroupRequest) (*models.RunDependencyResult, error) {
	if e.dependencies == nil {
		return nil, fmt.Errorf("run dependency store is not configured")
	}
	result, err := e.dependencies.CreateRunDependencyGroup(ctx, request)
	if err == nil && result != nil && !result.Replayed {
		e.wakeAgentWorkersForScope(request.Scope)
	}
	return result, err
}

func (e *Engine) ResolveRunDependency(ctx context.Context, request portfolio.ResolveRunDependencyRequest) (*models.RunDependencyResult, error) {
	if e.dependencies == nil {
		return nil, fmt.Errorf("run dependency store is not configured")
	}
	result, err := e.dependencies.ResolveRunDependency(ctx, request)
	if err == nil && result != nil && !result.Replayed && result.Evaluation.Wake {
		e.wakeAgentWorkersForScope(request.Scope)
	}
	return result, err
}

func (e *Engine) GetRunDependencyGroup(ctx context.Context, scope models.Scope, groupID string) (*models.RunDependencyGroup, error) {
	if e.dependencies == nil {
		return nil, fmt.Errorf("run dependency store is not configured")
	}
	return e.dependencies.GetRunDependencyGroup(ctx, scope, groupID)
}

func (e *Engine) ListWaitingRunDependencyGroups(ctx context.Context, scope models.Scope, afterID string, limit int) ([]models.RunDependencyGroupWork, error) {
	if e.dependencies == nil {
		return nil, fmt.Errorf("run dependency store is not configured")
	}
	return e.dependencies.ListWaitingRunDependencyGroups(ctx, scope, afterID, limit)
}

func (e *Engine) ReconcileRunDependencyGroup(ctx context.Context, request portfolio.ReconcileRunDependencyGroupRequest) (*models.RunDependencyResult, error) {
	if e.dependencies == nil {
		return nil, fmt.Errorf("run dependency store is not configured")
	}
	result, err := e.dependencies.ReconcileRunDependencyGroup(ctx, request)
	if err == nil && result != nil && !result.Replayed && result.Evaluation.Wake {
		e.wakeAgentWorkersForScope(request.Scope)
	}
	return result, err
}

func (e *Engine) FindRunDependencyGroupByIdempotencyKey(ctx context.Context, scope models.Scope, key string) (*models.RunDependencyGroup, error) {
	if e.dependencies == nil {
		return nil, fmt.Errorf("run dependency store is not configured")
	}
	return e.dependencies.FindRunDependencyGroupByIdempotencyKey(ctx, scope, key)
}

func (e *Engine) ListRunDependencies(ctx context.Context, scope models.Scope, groupID string) ([]*models.RunDependency, error) {
	if e.dependencies == nil {
		return nil, fmt.Errorf("run dependency store is not configured")
	}
	return e.dependencies.ListRunDependencies(ctx, scope, groupID)
}

func (e *Engine) CreateAgentRequest(ctx context.Context, request collaboration.CreateAgentRequestRequest) (*collaboration.AgentRequestResult, error) {
	if e.collaboration == nil {
		return nil, fmt.Errorf("collaboration store is not configured")
	}
	result, err := e.collaboration.CreateAgentRequest(ctx, request)
	if err == nil && result != nil && len(result.Events) > 0 {
		e.wakeAgentWorkersForScope(request.Scope)
	}
	return result, err
}

func (e *Engine) CreateAgentRequestGroup(ctx context.Context, request collaboration.CreateAgentRequestGroupRequest) (*collaboration.AgentRequestGroupResult, error) {
	if e.collaboration == nil {
		return nil, fmt.Errorf("collaboration store is not configured")
	}
	result, err := e.collaboration.CreateAgentRequestGroup(ctx, request)
	if err == nil && result != nil && result.Group != nil && !result.Group.Replayed {
		e.wakeAgentWorkersForScope(request.Scope)
	}
	return result, err
}

func (e *Engine) RespondAgentRequest(ctx context.Context, request collaboration.RespondAgentRequestRequest) (*collaboration.AgentRequestResult, error) {
	if e.collaboration == nil {
		return nil, fmt.Errorf("collaboration store is not configured")
	}
	result, err := e.collaboration.RespondAgentRequest(ctx, request)
	if err == nil && result != nil && len(result.Events) > 0 {
		e.wakeAgentWorkersForScope(request.Scope)
	}
	return result, err
}

func (e *Engine) SubmitAgentRequestBid(ctx context.Context, request collaboration.SubmitAgentRequestBidRequest) (*collaboration.AgentRequestResult, error) {
	if e == nil || e.collaboration == nil {
		return nil, errors.New("agent request collaboration is not configured")
	}
	result, err := e.collaboration.SubmitAgentRequestBid(ctx, request)
	if err == nil && result != nil && len(result.Events) > 0 {
		e.wakeAgentWorkersForScope(request.Scope)
	}
	return result, err
}

func (e *Engine) CompleteAgentRequest(ctx context.Context, request collaboration.CompleteAgentRequestRequest) (*collaboration.AgentRequestResult, error) {
	if e.collaboration == nil {
		return nil, fmt.Errorf("collaboration store is not configured")
	}
	result, err := e.collaboration.CompleteAgentRequest(ctx, request)
	if err == nil && result != nil && len(result.Events) > 0 {
		e.wakeAgentWorkersForScope(request.Scope)
	}
	return result, err
}

func (e *Engine) ReviewAgentRequestCompletion(ctx context.Context, request collaboration.ReviewAgentRequestCompletionRequest) (*collaboration.AgentRequestResult, error) {
	if e.collaboration == nil {
		return nil, fmt.Errorf("collaboration store is not configured")
	}
	result, err := e.collaboration.ReviewAgentRequestCompletion(ctx, request)
	if err == nil && result != nil && len(result.Events) > 0 {
		e.wakeAgentWorkersForScope(request.Scope)
	}
	return result, err
}

func (e *Engine) GetAgentRequest(ctx context.Context, scope models.Scope, requestID string) (*models.AgentRequest, error) {
	if e.collaboration == nil {
		return nil, fmt.Errorf("collaboration store is not configured")
	}
	return e.collaboration.GetAgentRequest(ctx, scope, requestID)
}

func (e *Engine) ListAgentRequests(ctx context.Context, filter models.AgentRequestFilter) ([]*models.AgentRequest, error) {
	if e.collaboration == nil {
		return nil, fmt.Errorf("collaboration store is not configured")
	}
	return e.collaboration.ListAgentRequests(ctx, filter)
}

func (e *Engine) BeginAgentTurn(ctx context.Context, req activity.BeginAgentTurnRequest) (*models.AgentTurn, error) {
	return e.turns.BeginTurn(ctx, req)
}

func (e *Engine) FinishAgentTurn(ctx context.Context, scope models.Scope, turnID string, req activity.FinishAgentTurnRequest) (*models.AgentTurn, error) {
	return e.turns.FinishTurn(ctx, scope, turnID, req)
}

func (e *Engine) GetAgentTurn(ctx context.Context, scope models.Scope, turnID string) (*models.AgentTurn, error) {
	return e.turns.GetTurn(ctx, scope, turnID)
}

func (e *Engine) ListAgentTurns(ctx context.Context, filter models.AgentTurnFilter) ([]*models.AgentTurn, error) {
	return e.turns.ListTurns(ctx, filter)
}

func (e *Engine) ClaimNextAgentRun(ctx context.Context, req agentrun.AgentRunClaimRequest) (*models.AgentRun, error) {
	return e.runQueue.ClaimNext(ctx, req)
}

func (e *Engine) ClaimNextAgentRunDecision(ctx context.Context, req agentrun.AgentRunClaimRequest) (*models.AgentRunAdmissionDecision, error) {
	return e.runQueue.ClaimNextDecision(ctx, req)
}

func (e *Engine) InspectAgentRunAdmission(ctx context.Context, req agentrun.AgentRunClaimRequest) (*models.AgentRunAdmissionDecision, error) {
	return e.runQueue.Inspect(ctx, req)
}

func (e *Engine) RenewAgentRunLease(ctx context.Context, scope models.Scope, runID, workerID string, leaseDuration time.Duration) (*models.AgentRun, error) {
	return e.runQueue.RenewLease(ctx, scope, runID, workerID, leaseDuration)
}

func (e *Engine) WakeAgentRuns(ctx context.Context, signal agentrun.WakeSignal) (*agentrun.WakeResult, error) {
	return e.wake.Wake(ctx, signal)
}

func (e *Engine) WakeDueAgentRuns(ctx context.Context, scope models.Scope, at time.Time) (*agentrun.WakeResult, error) {
	return e.wake.WakeDueTimers(ctx, scope, at)
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

// ResolveCallbackAdapterBinding returns the exact callback adapter selected by
// one deployment binding. Hosts use this to materialize provider connection
// transports without learning provider-specific protocol semantics.
func (e *Engine) ResolveCallbackAdapterBinding(ctx context.Context, scope skill.ScopeReference, deploymentID, bindingID, adapterID string) (*skill.BoundCallbackAdapter, error) {
	return e.skills.ResolveCallbackAdapterBinding(ctx, scope, deploymentID, bindingID, adapterID)
}

func (e *Engine) PlanSkillReferenceUpgrade(ctx context.Context, request skillbinding.PlanSkillReferenceUpgradeRequest) (*models.SkillReferenceUpgradePlan, error) {
	if e == nil || e.skillReferenceUpgrades == nil {
		return nil, skillbinding.ErrSkillReferenceUpgradeUnavailable
	}
	return e.skillReferenceUpgrades.Plan(ctx, request)
}

func (e *Engine) ApplySkillReferenceUpgrade(ctx context.Context, request skillbinding.ApplySkillReferenceUpgradeRequest) (*models.SkillReferenceUpgradeReceipt, error) {
	if e == nil || e.skillReferenceUpgrades == nil {
		return nil, skillbinding.ErrSkillReferenceUpgradeUnavailable
	}
	return e.skillReferenceUpgrades.Apply(ctx, request)
}

// HasSkillRuntimeUsage reports whether replacing this executable version
// would invalidate outstanding calls or receipts still owned by an active Run.
func (e *Engine) HasSkillRuntimeUsage(ctx context.Context, filter models.SkillRuntimeUsageFilter) (bool, error) {
	if e == nil {
		return false, skillbinding.ErrSkillReferenceUpgradeUnavailable
	}
	store, ok := e.store.(models.SkillRuntimeUsageStore)
	if !ok {
		return false, skillbinding.ErrSkillReferenceUpgradeUnavailable
	}
	return store.HasSkillRuntimeUsage(ctx, filter)
}

// ListReferencedSkillRuntimeVersions provides tenant-scoped metadata for
// retaining exact execution versions while newer versions are staged.
func (e *Engine) ListReferencedSkillRuntimeVersions(ctx context.Context, scope models.Scope) ([]models.SkillRuntimeReference, error) {
	if e == nil {
		return nil, skillbinding.ErrSkillReferenceUpgradeUnavailable
	}
	store, ok := e.store.(models.SkillRuntimeUsageStore)
	if !ok {
		return nil, skillbinding.ErrSkillReferenceUpgradeUnavailable
	}
	return store.ListReferencedSkillRuntimeVersions(ctx, scope)
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

// SetHostSkillBindingProvider installs the host's run-aware bindings (for
// example connected apps), consulted at every Agent activation and action
// resolution of a run. Call it before workers start.
func (e *Engine) SetHostSkillBindingProvider(provider skill.HostBindingProvider) {
	e.skills.SetHostBindingProvider(provider)
}

const (
	SkillUmbrellaAgent = skill.UmbrellaAgent
	SkillUmbrellaTeam  = skill.UmbrellaTeam
)

// ErrRunUmbrellaUnavailable means a run no longer acts under a valid umbrella.
var ErrRunUmbrellaUnavailable = models.ErrRunUmbrellaUnavailable

// SkillBindingHoldsConnection reports whether a binding carries a connection.
func SkillBindingHoldsConnection(binding *skill.Binding) bool {
	return skill.BindingHoldsConnection(binding)
}

// WithoutSkillConnections marks ctx so activation leaves out every binding
// that holds a connection and attaches no host bindings.
func WithoutSkillConnections(ctx context.Context) context.Context {
	return skill.WithoutConnections(ctx)
}

// RunUmbrella returns the Agent or Team whose connections run uses, failing
// closed when the run no longer acts under it.
func (e *Engine) RunUmbrella(ctx context.Context, run *models.AgentRun) (ObjectiveOwner, error) {
	return models.RunUmbrella(ctx, e, run)
}

// RunSkillActor is the actor run activates and resolves Skills as.
func RunSkillActor(run *models.AgentRun) skill.RunActor { return models.RunSkillActor(run) }

// WithSkillRunActor marks ctx as acting for one run so activation and exact
// action resolution through it include that run's host bindings.
func WithSkillRunActor(ctx context.Context, actor skill.RunActor) context.Context {
	return skill.WithRunActor(ctx, actor)
}

// GetHostSkillBinding returns a host binding of the run ctx acts for (see
// WithSkillRunActor), or nil when the host does not supply it to that run.
func (e *Engine) GetHostSkillBinding(ctx context.Context, scope skill.ScopeReference, deploymentID, bindingID string) (*skill.Binding, error) {
	return e.skills.GetHostBinding(ctx, scope, deploymentID, bindingID)
}

func (e *Engine) ActivateSkills(ctx context.Context, scope skill.ScopeReference, deploymentID string, host skill.HostCapabilityState) (*skill.ActivationSnapshot, error) {
	return e.skills.Activate(ctx, scope, deploymentID, host)
}

// ActivateTeamSkillsForAgent returns the exact model-visible Skill surface an
// Agent may use while participating as one roster member of a Team. Team-owned
// bindings are first evaluated against the current host and then narrowed by
// the immutable roster assignment, semantic-role grants, Agent authority, and
// Team deployment restrictions. Embedding hosts should use this projection for
// every participant-specific Team turn instead of exposing the Team catalog
// directly.
func (e *Engine) ActivateTeamSkillsForAgent(ctx context.Context, scope skill.ScopeReference, teamDeploymentID, agentDeploymentID string, host skill.HostCapabilityState) (*skill.ActivationSnapshot, error) {
	if e == nil || e.skills == nil || e.agents == nil || e.teams == nil {
		return nil, errors.New("Team Skill activation is unavailable")
	}
	activation, err := e.skills.Activate(ctx, scope, teamDeploymentID, host)
	if err != nil {
		return nil, err
	}
	return skillbinding.AuthorizeTeamSkillActivation(ctx, e, &models.AgentRun{
		Scope:           models.Scope{Kind: scope.Kind, ID: scope.ID},
		Owner:           models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: teamDeploymentID},
		AssignedAgentID: agentDeploymentID,
	}, activation)
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

// CreateAgentDeployment creates the Agent in the default space of the person
// creating it (actorType "user"). When no person is behind the call, or they
// have no default space, the Agent is created in no space and gets its
// control conversation when it joins one.
func (e *Engine) CreateAgentDeployment(ctx context.Context, deployment *kernelagent.AgentDeployment, actorType, actorID, reason string) (*kernelagent.AgentDeployment, *kernelagent.DefinitionActivation, error) {
	created, activation, err := e.agents.CreateDeployment(ctx, deployment, actorType, actorID, reason)
	if err != nil {
		return nil, nil, err
	}
	person := actingPerson(actorType, actorID)
	e.ensureDeploymentHasSpace(ctx, created.Scope, person, models.SpaceMemberAgent, created.ID)
	if _, ensureErr := e.ensureAgentControlConversation(ctx, created, person); ensureErr != nil && !errors.Is(ensureErr, models.ErrNoDefaultSpace) {
		return created, activation, ensureErr
	}
	return created, activation, nil
}

// CreateAgentDeploymentInSpace creates the Agent as a member of the space,
// with its control conversation there. No default space is involved.
func (e *Engine) CreateAgentDeploymentInSpace(ctx context.Context, spaceID string, deployment *kernelagent.AgentDeployment, actorType, actorID, reason string) (*kernelagent.AgentDeployment, *kernelagent.DefinitionActivation, error) {
	if e.spaces == nil {
		return nil, nil, errSpacesUnsupported
	}
	if deployment == nil {
		return nil, nil, errors.New("agent deployment is required")
	}
	scope := models.Scope{Kind: deployment.Scope.Kind, ID: deployment.Scope.ID}
	if _, err := e.spaces.GetSpace(ctx, scope, spaceID); err != nil {
		return nil, nil, err
	}
	created, activation, err := e.agents.CreateDeployment(ctx, deployment, actorType, actorID, reason)
	if err != nil {
		return nil, nil, err
	}
	if _, _, err := e.spaces.PutSpaceMember(ctx, portfolio.PutSpaceMemberRequest{Scope: scope, SpaceID: spaceID, Type: models.SpaceMemberAgent, MemberID: created.ID}); err != nil {
		return created, activation, err
	}
	if _, err := e.ensureAgentControlConversation(ctx, created, actingPerson(actorType, actorID)); err != nil {
		return created, activation, err
	}
	return created, activation, nil
}

// actingPerson is the person behind a deployment write: its actor when that
// is a user, else no one.
func actingPerson(actorType, actorID string) string {
	if strings.TrimSpace(actorType) != "user" {
		return ""
	}
	return strings.TrimSpace(actorID)
}

// GetAgentDeployment and ListAgentDeployments are side-effect free. The
// Agent control conversation is created when a deployment is created,
// updated, activated, rolled back or applied from a workforce change set.
func (e *Engine) GetAgentDeployment(ctx context.Context, scope skill.ScopeReference, deploymentID string) (*kernelagent.AgentDeployment, error) {
	return e.agents.GetDeployment(ctx, scope, deploymentID)
}

func (e *Engine) ListAgentDeployments(ctx context.Context, filter AgentDeploymentFilter) ([]*kernelagent.AgentDeployment, error) {
	return e.agents.ListDeployments(ctx, filter)
}

// ensureAgentControlConversationAfterWrite runs after a deployment write has
// committed. The conversation is idempotent and the write must not appear
// to fail because of it, so a failure is logged and retried by the next
// deployment write. An Agent in no space, written by no person with a
// default space, has none until it joins a space.
func (e *Engine) ensureAgentControlConversationAfterWrite(ctx context.Context, deployment *kernelagent.AgentDeployment, person string) {
	if deployment == nil {
		return
	}
	if _, err := e.ensureAgentControlConversation(ctx, deployment, person); err != nil && !errors.Is(err, models.ErrNoDefaultSpace) && e.logger != nil {
		e.logger.Warnw("failed to ensure Agent control conversation", "deploymentId", deployment.ID, "error", err)
	}
}

// ensureAgentControlConversation creates the control conversation in the
// Agent's first space, else in the default space of person.
func (e *Engine) ensureAgentControlConversation(ctx context.Context, deployment *kernelagent.AgentDeployment, person string) (*models.Conversation, error) {
	if e == nil || e.conversations == nil || deployment == nil {
		return nil, errors.New("Agent control conversation service is unavailable")
	}
	scope := models.Scope{Kind: deployment.Scope.Kind, ID: deployment.Scope.ID}
	conversation, _, err := e.conversations.CreateConversation(ctx, conversation.CreateConversationRequest{
		Scope:          scope,
		Owner:          models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: deployment.ID},
		Title:          "Agent control",
		Origin:         &models.ConversationReference{Kind: models.ConversationReferenceAgentControl, ID: agentControlReferenceID(deployment.ID)},
		CreatedBy:      person,
		IdempotencyKey: "agent-control:" + deployment.ID,
	})
	if err != nil {
		return nil, fmt.Errorf("ensure Agent control channel for %s: %w", deployment.ID, err)
	}
	return conversation, nil
}

func agentControlReferenceID(deploymentID string) string {
	digest := sha256.Sum256([]byte(strings.TrimSpace(deploymentID)))
	return "agent-" + hex.EncodeToString(digest[:16])
}

func AgentApprovalsConversationReferenceID(owner ObjectiveOwner) string {
	return models.AgentApprovalsConversationReferenceID(owner)
}

func (e *Engine) UpdateAgentDeployment(ctx context.Context, deployment *kernelagent.AgentDeployment, expectedRevision int64, actorType, actorID, reason string) (*kernelagent.AgentDeployment, *kernelagent.DefinitionActivation, error) {
	updated, activation, err := e.agents.UpdateDeployment(ctx, deployment, expectedRevision, actorType, actorID, reason)
	if err == nil {
		e.ensureAgentControlConversationAfterWrite(ctx, updated, actingPerson(actorType, actorID))
	}
	return updated, activation, err
}

func (e *Engine) ActivateAgentDefinition(ctx context.Context, scope skill.ScopeReference, deploymentID, version string, expectedRevision int64, actorType, actorID, reason string) (*kernelagent.AgentDeployment, *kernelagent.DefinitionActivation, error) {
	activated, activation, err := e.agents.ActivateDefinition(ctx, scope, deploymentID, version, expectedRevision, actorType, actorID, reason)
	if err == nil {
		e.ensureAgentControlConversationAfterWrite(ctx, activated, actingPerson(actorType, actorID))
	}
	return activated, activation, err
}

func (e *Engine) RollbackAgentDefinition(ctx context.Context, scope skill.ScopeReference, deploymentID string, expectedRevision int64, actorType, actorID, reason string) (*kernelagent.AgentDeployment, *kernelagent.DefinitionActivation, error) {
	rolledBack, activation, err := e.agents.RollbackDefinition(ctx, scope, deploymentID, expectedRevision, actorType, actorID, reason)
	if err == nil {
		e.ensureAgentControlConversationAfterWrite(ctx, rolledBack, actingPerson(actorType, actorID))
	}
	return rolledBack, activation, err
}

func (e *Engine) ListAgentDefinitionActivations(ctx context.Context, scope skill.ScopeReference, deploymentID string) ([]kernelagent.DefinitionActivation, error) {
	return e.agents.ListActivations(ctx, scope, deploymentID)
}

func (e *Engine) ProposeAgentDefinitionAmendment(ctx context.Context, request kernelagent.ProposeAmendmentRequest) (*kernelagent.DefinitionAmendment, error) {
	return e.agents.ProposeAmendment(ctx, request)
}

// ChangedAgentDefinitionFields exposes the Agent registry's canonical
// top-level comparison for governed host amendment surfaces.
func ChangedAgentDefinitionFields(base, candidate *AgentDefinition) []string {
	return kernelagent.ChangedDefinitionFields(base, candidate)
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

// CreateTeamDeployment creates the Team in the default space of the person
// creating it (actorType "user"), which brings its Agents there. When no
// person is behind the call, or they have no default space, it creates
// nothing and returns ErrNoDefaultSpace; CreateTeamDeploymentInSpace names
// the space instead.
func (e *Engine) CreateTeamDeployment(ctx context.Context, deployment *kernelteam.Deployment, actorType, actorID, reason string) (*kernelteam.Deployment, *workforce.DefinitionActivation, error) {
	if e.spaces == nil {
		return e.teams.CreateDeployment(ctx, deployment, actorType, actorID, reason)
	}
	if deployment == nil {
		return nil, nil, errors.New("team deployment is required")
	}
	space, err := e.spaces.DefaultSpace(ctx, models.Scope{Kind: deployment.Scope.Kind, ID: deployment.Scope.ID}, actingPerson(actorType, actorID))
	if err != nil {
		return nil, nil, err
	}
	created, activation, err := e.teams.CreateDeployment(ctx, deployment, actorType, actorID, reason)
	if err == nil {
		e.placeTeam(ctx, created, space.ID)
	}
	return created, activation, err
}

// CreateTeamDeploymentInSpace creates the Team as a member of the space.
// Every Agent on its roster must already be in that space
// (ErrTeamAgentOutsideSpace).
func (e *Engine) CreateTeamDeploymentInSpace(ctx context.Context, spaceID string, deployment *kernelteam.Deployment, actorType, actorID, reason string) (*kernelteam.Deployment, *workforce.DefinitionActivation, error) {
	if e.spaces == nil {
		return nil, nil, errSpacesUnsupported
	}
	if deployment == nil {
		return nil, nil, errors.New("team deployment is required")
	}
	if err := e.spaces.CheckSpaceAgents(ctx, models.Scope{Kind: deployment.Scope.Kind, ID: deployment.Scope.ID}, spaceID, teamRosterAgents(deployment.Roster)); err != nil {
		return nil, nil, err
	}
	created, activation, err := e.teams.CreateDeployment(ctx, deployment, actorType, actorID, reason)
	if err == nil {
		e.placeTeam(ctx, created, spaceID)
	}
	return created, activation, err
}

// placeTeam runs after the Team's create has committed: it joins the space.
// A failure is logged, not returned; the Team exists.
func (e *Engine) placeTeam(ctx context.Context, team *kernelteam.Deployment, spaceID string) {
	scope := models.Scope{Kind: team.Scope.Kind, ID: team.Scope.ID}
	_, _, err := e.spaces.PutSpaceMember(ctx, portfolio.PutSpaceMemberRequest{Scope: scope, SpaceID: spaceID, Type: models.SpaceMemberTeam, MemberID: team.ID})
	if err != nil && e.logger != nil {
		e.logger.Warnw("team was not placed in a space", "deploymentId", team.ID, "spaceId", spaceID, "error", err)
	}
}

func teamRosterAgents(roster []kernelteam.RosterAssignment) []string {
	agents := make([]string, 0, len(roster))
	for _, assignment := range roster {
		agents = append(agents, assignment.AgentDeploymentID)
	}
	return agents
}

// ensureDeploymentHasSpace runs after a deployment create has committed: a
// deployment in no space joins the default space of the person who created
// it. Without a person, or when they have no default space, it stays in no
// space: a host provisions Agents before any space exists and adds them to
// spaces itself. Any other failure is logged, not returned; the deployment
// exists.
func (e *Engine) ensureDeploymentHasSpace(ctx context.Context, scope skill.ScopeReference, person string, memberType models.SpaceMemberType, deploymentID string) {
	if e == nil || e.spaces == nil {
		return
	}
	if _, err := e.spaces.EnsureMemberHasSpace(ctx, models.Scope{Kind: scope.Kind, ID: scope.ID}, person, memberType, deploymentID); err != nil && !errors.Is(err, models.ErrNoDefaultSpace) && e.logger != nil {
		e.logger.Warnw("deployment was not placed in a space", "deploymentId", deploymentID, "type", memberType, "error", err)
	}
}

func (e *Engine) GetTeamDeployment(ctx context.Context, scope skill.ScopeReference, deploymentID string) (*kernelteam.Deployment, error) {
	return e.teams.GetDeployment(ctx, scope, deploymentID)
}

func (e *Engine) ListTeamDeployments(ctx context.Context, scope skill.ScopeReference) ([]*kernelteam.Deployment, error) {
	return e.teams.ListDeployments(ctx, scope)
}

// UpdateTeamDeployment refuses an Agent joining the roster that is not in
// every space the Team is in (ErrTeamAgentOutsideSpace). Agents already on
// the roster are not checked, so a Team that predates the rule keeps working.
func (e *Engine) UpdateTeamDeployment(ctx context.Context, deployment *kernelteam.Deployment, expectedRevision int64, actorType, actorID, reason string) (*kernelteam.Deployment, *workforce.DefinitionActivation, error) {
	if e.spaces != nil && deployment != nil {
		if current, err := e.teams.GetDeployment(ctx, deployment.Scope, deployment.ID); err == nil {
			joining := slices.DeleteFunc(teamRosterAgents(deployment.Roster), func(agentID string) bool {
				return slices.Contains(teamRosterAgents(current.Roster), agentID)
			})
			if err := e.spaces.CheckTeamAgents(ctx, models.Scope{Kind: deployment.Scope.Kind, ID: deployment.Scope.ID}, deployment.ID, joining); err != nil {
				return nil, nil, err
			}
		}
	}
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

func (e *Engine) RecoverTeamParticipation(ctx context.Context, request kernelteam.RecoverParticipationRequest) (*kernelteam.ParticipationRecoveryResult, error) {
	return e.teams.RecoverParticipation(ctx, request)
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

func (e *Engine) ResolveApproval(ctx context.Context, req action.ResolveApprovalRequest) (*models.ApprovalResolutionResult, error) {
	return e.approvals.Resolve(ctx, req)
}

func (e *Engine) GetActionCall(ctx context.Context, scope models.Scope, actionID string) (*models.ActionCall, error) {
	return e.store.GetActionCall(ctx, scope, actionID)
}

func (e *Engine) ListActionCalls(ctx context.Context, filter models.ActionFilter) ([]*models.ActionCall, error) {
	return e.store.ListActionCalls(ctx, filter)
}

func (e *Engine) GetApproval(ctx context.Context, scope models.Scope, approvalID string) (*models.ApprovalCheckpoint, error) {
	return e.store.GetApproval(ctx, scope, approvalID)
}

func (e *Engine) ListApprovals(ctx context.Context, filter models.ApprovalFilter) ([]*models.ApprovalCheckpoint, error) {
	return e.store.ListApprovals(ctx, filter)
}
