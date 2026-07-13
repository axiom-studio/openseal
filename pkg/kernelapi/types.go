// Package kernelapi defines the versioned HTTP contract shared by the
// standalone OpenSeal daemon and thin clients such as the terminal UI.
package kernelapi

import (
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runtime"
	"github.com/axiom-studio/openseal/pkg/skill/clawhub"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
	"github.com/axiom-studio/openseal/pkg/workforce"
)

const (
	APIVersion                          = "agent-kernel/v1"
	AgentRunsCapabilityID               = "agent-runs"
	AgentRunsCapabilityVersion          = "1"
	ObjectivesCapabilityID              = "objectives"
	ObjectivesCapabilityVersion         = "1"
	InitiativesCapabilityID             = "initiatives"
	InitiativesCapabilityVersion        = "1"
	SourceMonitorsCapabilityID          = "source-monitors"
	SourceMonitorsCapabilityVersion     = "1"
	OutreachCapabilityID                = "outreach"
	OutreachCapabilityVersion           = "1"
	SkillActionsCapabilityID            = "skill-actions"
	SkillActionsCapabilityVersion       = "1"
	ArtifactsCapabilityID               = "artifacts"
	ArtifactsCapabilityVersion          = "1"
	ChannelsCapabilityID                = "channels"
	ChannelsCapabilityVersion           = "4"
	TeamDefinitionsCapabilityID         = "team-definitions"
	TeamDefinitionsCapabilityVersion    = "2"
	WorkforceAuthoringCapabilityID      = "workforce-authoring"
	WorkforceAuthoringCapabilityVersion = "3"
	ClawHubLifecycleCapabilityID        = "clawhub-lifecycle"
	ClawHubLifecycleCapabilityVersion   = clawhub.LifecycleAPIVersion
	AgentDefinitionsCapabilityID        = "agent-definitions"
	AgentDefinitionsCapabilityVersion   = "2"
	AgentRequestsCapabilityID           = "agent-requests"
	AgentRequestsCapabilityVersion      = "1"
	ActionApprovalsCapabilityID         = "action-approvals"
	ActionApprovalsCapabilityVersion    = "1"
	ActivityCapabilityID                = "activity"
	ActivityCapabilityVersion           = "1"
)

const (
	OperationCreate            = "create"
	OperationGet               = "get"
	OperationList              = "list"
	OperationPause             = "pause"
	OperationResume            = "resume"
	OperationCancel            = "cancel"
	OperationIntervene         = "intervene"
	OperationUpdate            = "update"
	OperationRegister          = "register"
	OperationUpload            = "upload"
	OperationDownload          = "download"
	OperationResolve           = "resolve"
	OperationRespond           = "respond"
	OperationComplete          = "complete"
	OperationPost              = "post"
	OperationCoordinate        = "coordinate"
	OperationRead              = "read"
	OperationPresence          = "presence"
	OperationAudit             = "audit"
	OperationChanges           = "changes"
	OperationReceipts          = "receipts"
	OperationCoordinateAuto    = "coordinate-automatically"
	OperationStream            = "stream"
	OperationDeploy            = "deploy"
	OperationActivate          = "activate"
	OperationCompile           = "compile"
	OperationPropose           = "propose"
	OperationEvaluate          = "evaluate"
	OperationApprove           = "approve"
	OperationApply             = "apply"
	OperationRetry             = "retry"
	OperationPatch             = "patch"
	OperationListCompilations  = "list-compilations"
	OperationListObservations  = "list-observations"
	OperationGetCheckpoint     = "get-checkpoint"
	OperationDeliver           = "deliver"
	OperationProposeAmendment  = "propose-amendment"
	OperationEvaluateAmendment = "evaluate-amendment"
	OperationResolveAmendment  = "resolve-amendment"
	OperationActivateAmendment = "activate-amendment"
)

// CapabilityDocument is the authoritative product surface advertised by an
// OpenSeal server. Clients must not infer operations that are absent here.
type CapabilityDocument struct {
	APIVersion   string       `json:"apiVersion"`
	Capabilities []Capability `json:"capabilities"`
}

type Capability struct {
	ID         string             `json:"id"`
	Version    string             `json:"version"`
	Available  bool               `json:"available"`
	Operations []string           `json:"operations"`
	Context    *CapabilityContext `json:"context,omitempty"`
}

type SkillActionList struct {
	DeploymentID string                   `json:"deploymentId"`
	Actions      []capability.ModelAction `json:"actions"`
}

// ChannelCapabilityFeatures describes the optional channel services wired by
// a host. The version and operation vocabulary are canonical; each host
// advertises only the portable or enterprise adapters it has actually wired.
type ChannelCapabilityFeatures struct {
	Coordination          bool
	AutomaticCoordination bool
	Receipts              bool
	Changes               bool
	Streaming             bool
}

type TeamDefinitionCapabilityFeatures struct {
	Amendments bool
}

// WorkforceAuthoringCapabilityFeatures describes host-wide authoring services.
// Resource-specific lifecycle authority is composed into a contextual
// capability response and must never be inferred from these feature flags.
type WorkforceAuthoringCapabilityFeatures struct {
	ChangeSets bool
}

type ActionApprovalCapabilityFeatures struct {
	Resolution bool
}

// CapabilityContext is server-authored authorization state for one explicitly
// requested resource. It is never durable policy input and clients must not
// infer authority from the underlying resource itself.
type CapabilityContext struct {
	ChangeSetID                  string                         `json:"changeSetId,omitempty"`
	Revision                     int64                          `json:"revision,omitempty"`
	EligibleApprovalRequirements []ApprovalRequirementReference `json:"eligibleApprovalRequirements,omitempty"`
}

type ClawHubVersionRequest struct {
	Version string `json:"version,omitempty"`
	Tag     string `json:"tag,omitempty"`
}
type ClawHubPinRequest struct {
	Reason string `json:"reason"`
}
type ClawHubFile struct {
	Path          string `json:"path"`
	Size          int    `json:"size"`
	ContentBase64 string `json:"contentBase64"`
}

type ApprovalRequirementReference struct {
	EvaluationID string `json:"evaluationId"`
	PolicyID     string `json:"policyId"`
	Role         string `json:"role"`
}

type CreateTeamDeploymentRequest struct {
	Deployment *kernelteam.Deployment `json:"deployment"`
	ActorType  string                 `json:"actorType"`
	ActorID    string                 `json:"actorId"`
	Reason     string                 `json:"reason,omitempty"`
}

type ActivateTeamDefinitionRequest struct {
	Scope            capability.ScopeReference `json:"scope"`
	Version          string                    `json:"version"`
	ExpectedRevision int64                     `json:"expectedRevision"`
	ActorType        string                    `json:"actorType"`
	ActorID          string                    `json:"actorId"`
	Reason           string                    `json:"reason,omitempty"`
}

type UpdateTeamDeploymentRequest struct {
	Deployment       *kernelteam.Deployment `json:"deployment"`
	ExpectedRevision int64                  `json:"expectedRevision"`
	ActorType        string                 `json:"actorType"`
	ActorID          string                 `json:"actorId"`
	Reason           string                 `json:"reason"`
}

type TeamDeploymentResult struct {
	Deployment *kernelteam.Deployment          `json:"deployment"`
	Activation *workforce.DefinitionActivation `json:"activation"`
}

type TeamDeploymentList struct {
	Deployments []*kernelteam.Deployment `json:"deployments"`
}

func (d CapabilityDocument) Find(id, version string) (Capability, bool) {
	for _, candidate := range d.Capabilities {
		if candidate.ID == id && candidate.Version == version {
			return candidate, true
		}
	}
	return Capability{}, false
}

func (c Capability) Supports(operation string) bool {
	if !c.Available {
		return false
	}
	for _, candidate := range c.Operations {
		if candidate == operation {
			return true
		}
	}
	return false
}

func AgentRunsCapability() Capability {
	return Capability{
		ID: AgentRunsCapabilityID, Version: AgentRunsCapabilityVersion, Available: true,
		Operations: []string{
			OperationCreate, OperationGet, OperationList, OperationPause,
			OperationResume, OperationCancel, OperationIntervene,
		},
	}
}

func ObjectivesCapability() Capability {
	return Capability{
		ID: ObjectivesCapabilityID, Version: ObjectivesCapabilityVersion, Available: true,
		Operations: []string{OperationCreate, OperationGet, OperationList, OperationUpdate},
	}
}

func InitiativesCapability() Capability {
	return Capability{
		ID: InitiativesCapabilityID, Version: InitiativesCapabilityVersion, Available: true,
		Operations: []string{OperationCreate, OperationGet, OperationList, OperationPatch},
	}
}

func SourceMonitorsCapability() Capability {
	return Capability{ID: SourceMonitorsCapabilityID, Version: SourceMonitorsCapabilityVersion, Available: true, Operations: []string{OperationListObservations, OperationGetCheckpoint}}
}

func OutreachCapability() Capability {
	return Capability{ID: OutreachCapabilityID, Version: OutreachCapabilityVersion, Available: true, Operations: []string{OperationCreate, OperationGet, OperationList, OperationDeliver}}
}

func SkillActionsCapability() Capability {
	return Capability{ID: SkillActionsCapabilityID, Version: SkillActionsCapabilityVersion, Available: true, Operations: []string{OperationList}}
}

func ClawHubLifecycleCapability(lifecycle clawhub.LifecycleCapability) Capability {
	operations := make([]string, 0, len(lifecycle.Operations))
	for _, operation := range lifecycle.Operations {
		operations = append(operations, string(operation))
	}
	return Capability{ID: ClawHubLifecycleCapabilityID, Version: ClawHubLifecycleCapabilityVersion, Available: len(operations) > 0, Operations: operations}
}

func Capabilities() CapabilityDocument {
	return NewCapabilityDocument(ObjectivesCapability(), InitiativesCapability(), SourceMonitorsCapability(), OutreachCapability(), SkillActionsCapability(), ActivityCapability(), AgentRunsCapability(), AgentDefinitionsCapability(), ChannelsCapability(ChannelCapabilityFeatures{Coordination: true, Changes: true}), TeamDefinitionsCapability(TeamDefinitionCapabilityFeatures{}))
}

// ActivityCapability exposes the selector-bounded, redacted audit projection.
// Raw append authority is deliberately not part of the interactive contract.
func ActivityCapability() Capability {
	return Capability{ID: ActivityCapabilityID, Version: ActivityCapabilityVersion, Available: true, Operations: []string{OperationList}}
}

func AgentDefinitionsCapability() Capability {
	return Capability{ID: AgentDefinitionsCapabilityID, Version: AgentDefinitionsCapabilityVersion, Available: true, Operations: []string{OperationListCompilations}}
}

// AgentRequestsCapability describes the portable collaboration lifecycle used
// by Agents and Teams to request work, hand it off, and return a durable result.
func AgentRequestsCapability() Capability {
	return Capability{
		ID: AgentRequestsCapabilityID, Version: AgentRequestsCapabilityVersion, Available: true,
		Operations: []string{OperationCreate, OperationGet, OperationList, OperationRespond, OperationComplete},
	}
}

// ActionApprovalsCapability describes the governed decision surface for
// deterministic actions that have paused at a durable approval checkpoint.
func ActionApprovalsCapability(features ActionApprovalCapabilityFeatures) Capability {
	capability := Capability{
		ID: ActionApprovalsCapabilityID, Version: ActionApprovalsCapabilityVersion, Available: true,
		Operations: []string{OperationGet, OperationList},
	}
	if features.Resolution {
		capability.Operations = append(capability.Operations, OperationResolve)
	}
	return capability
}

func ArtifactCapability(contentOperations ...string) Capability {
	capability := Capability{
		ID: ArtifactsCapabilityID, Version: ArtifactsCapabilityVersion, Available: true,
		Operations: []string{OperationRegister, OperationGet, OperationList},
	}
	for _, operation := range contentOperations {
		if operation != OperationUpload && operation != OperationDownload && operation != OperationResolve {
			continue
		}
		if !capability.Supports(operation) {
			capability.Operations = append(capability.Operations, operation)
		}
	}
	return capability
}

func ChannelsCapability(features ChannelCapabilityFeatures) Capability {
	capability := Capability{
		ID: ChannelsCapabilityID, Version: ChannelsCapabilityVersion, Available: true,
		Operations: []string{OperationCreate, OperationGet, OperationList, OperationPost, OperationRead, OperationPresence, OperationAudit},
	}
	if features.Coordination {
		capability.Operations = append(capability.Operations, OperationCoordinate)
	}
	if features.AutomaticCoordination {
		capability.Operations = append(capability.Operations, OperationCoordinateAuto)
	}
	if features.Receipts {
		capability.Operations = append(capability.Operations, OperationReceipts)
	}
	if features.Changes {
		capability.Operations = append(capability.Operations, OperationChanges)
	}
	if features.Streaming {
		capability.Operations = append(capability.Operations, OperationStream)
	}
	return capability
}

func TeamDefinitionsCapability(features TeamDefinitionCapabilityFeatures) Capability {
	capability := Capability{
		ID: TeamDefinitionsCapabilityID, Version: TeamDefinitionsCapabilityVersion, Available: true,
		Operations: []string{OperationRegister, OperationGet, OperationList, OperationDeploy, OperationUpdate, OperationActivate},
	}
	if features.Amendments {
		capability.Operations = append(capability.Operations, OperationProposeAmendment, OperationEvaluateAmendment, OperationResolveAmendment, OperationActivateAmendment)
	}
	return capability
}

func WorkforceAuthoringCapability(features WorkforceAuthoringCapabilityFeatures) Capability {
	capability := Capability{
		ID: WorkforceAuthoringCapabilityID, Version: WorkforceAuthoringCapabilityVersion, Available: true,
		Operations: []string{OperationCompile},
	}
	if features.ChangeSets {
		capability.Operations = append(capability.Operations, OperationPropose, OperationGet)
	}
	return capability
}

func NewCapabilityDocument(capabilities ...Capability) CapabilityDocument {
	return CapabilityDocument{APIVersion: APIVersion, Capabilities: append([]Capability(nil), capabilities...)}
}

// CreateAgentRunRequest is the public request body for canonical durable work.
// Scope is explicit because standalone OpenSeal can host more than one local
// workspace even though the TUI defaults to local/default.
type CreateAgentRunRequest struct {
	Scope           runtime.Scope              `json:"scope"`
	Kind            runtime.RunKind            `json:"kind,omitempty"`
	ObjectiveID     string                     `json:"objectiveId,omitempty"`
	ParentRunID     string                     `json:"parentRunId,omitempty"`
	Owner           runtime.ObjectiveOwner     `json:"owner"`
	AssignedAgentID string                     `json:"assignedAgentId,omitempty"`
	Entrypoint      string                     `json:"entrypoint,omitempty"`
	ConcurrencyKey  string                     `json:"concurrencyKey,omitempty"`
	Goal            string                     `json:"goal"`
	Source          runtime.RunSource          `json:"source"`
	Priority        int                        `json:"priority,omitempty"`
	Deadline        *time.Time                 `json:"deadline,omitempty"`
	AvailableAt     *time.Time                 `json:"availableAt,omitempty"`
	Context         map[string]interface{}     `json:"context,omitempty"`
	Plan            map[string]interface{}     `json:"plan,omitempty"`
	Checkpoint      map[string]interface{}     `json:"checkpoint,omitempty"`
	WakeCondition   *runtime.WakeCondition     `json:"wakeCondition,omitempty"`
	Budget          *runtime.BudgetPolicy      `json:"budget,omitempty"`
	Policy          map[string]interface{}     `json:"policy,omitempty"`
	IdempotencyKey  string                     `json:"idempotencyKey,omitempty"`
	Actor           runtime.ActivityActor      `json:"actor,omitempty"`
	Visibility      runtime.ActivityVisibility `json:"visibility,omitempty"`
}

// CreateAgentRequestRequest is the public request body for a durable request or
// handoff between Agents and Teams. Only explicitly shared, credential-free
// context crosses the collaboration boundary.
type CreateAgentRequestRequest struct {
	ID                   string                        `json:"id,omitempty"`
	Scope                runtime.Scope                 `json:"scope"`
	Kind                 runtime.AgentRequestKind      `json:"kind"`
	Requester            runtime.CollaborationParty    `json:"requester"`
	Recipient            runtime.CollaborationParty    `json:"recipient"`
	SourceRunID          string                        `json:"sourceRunId"`
	Goal                 string                        `json:"goal"`
	Instructions         string                        `json:"instructions,omitempty"`
	SemanticRole         string                        `json:"semanticRole,omitempty"`
	AcceptanceCriteria   map[string]interface{}        `json:"acceptanceCriteria,omitempty"`
	ArtifactRequirements []runtime.ArtifactRequirement `json:"artifactRequirements,omitempty"`
	SharedContext        map[string]interface{}        `json:"sharedContext,omitempty"`
	ConversationRefs     []string                      `json:"conversationRefs,omitempty"`
	BudgetAllocation     *runtime.BudgetPolicy         `json:"budgetAllocation,omitempty"`
	IdempotencyKey       string                        `json:"idempotencyKey,omitempty"`
}

type RespondAgentRequestRequest struct {
	ExpectedRevision int64                        `json:"expectedRevision"`
	Decision         runtime.AgentRequestDecision `json:"decision"`
	Principal        runtime.CollaborationParty   `json:"principal"`
	AssignedAgentID  string                       `json:"assignedAgentId,omitempty"`
	Message          string                       `json:"message,omitempty"`
}

type CompleteAgentRequestRequest struct {
	ExpectedRevision      int64                       `json:"expectedRevision"`
	ExpectedChildRevision int64                       `json:"expectedChildRevision"`
	Principal             runtime.CollaborationParty  `json:"principal"`
	Actor                 runtime.CollaborationParty  `json:"actor,omitempty"`
	Summary               string                      `json:"summary"`
	AcceptanceEvidence    map[string]interface{}      `json:"acceptanceEvidence,omitempty"`
	Artifacts             []runtime.ArtifactReference `json:"artifacts,omitempty"`
	IdempotencyKey        string                      `json:"idempotencyKey,omitempty"`
}

type ResolveActionApprovalRequest struct {
	ExpectedRevision int64                     `json:"expectedRevision"`
	DecisionID       string                    `json:"decisionId,omitempty"`
	Approve          bool                      `json:"approve"`
	Principal        runtime.ApprovalPrincipal `json:"principal"`
	Reason           string                    `json:"reason,omitempty"`
}

type CreateObjectiveRequest struct {
	Scope            runtime.Scope             `json:"scope"`
	Owner            runtime.ObjectiveOwner    `json:"owner"`
	Title            string                    `json:"title"`
	Goal             string                    `json:"goal"`
	Status           runtime.ObjectiveStatus   `json:"status,omitempty"`
	Priority         int                       `json:"priority,omitempty"`
	Cadence          *runtime.ObjectiveCadence `json:"cadence,omitempty"`
	EventRules       map[string]interface{}    `json:"eventRules,omitempty"`
	Budget           *runtime.BudgetPolicy     `json:"budget,omitempty"`
	Constraints      map[string]interface{}    `json:"constraints,omitempty"`
	SuccessCriteria  map[string]interface{}    `json:"successCriteria,omitempty"`
	NextEvaluationAt *time.Time                `json:"nextEvaluationAt,omitempty"`
	IdempotencyKey   string                    `json:"idempotencyKey,omitempty"`
}

type UpdateObjectiveRequest struct {
	ExpectedRevision int64                     `json:"expectedRevision"`
	Title            *string                   `json:"title,omitempty"`
	Goal             *string                   `json:"goal,omitempty"`
	Status           *runtime.ObjectiveStatus  `json:"status,omitempty"`
	Priority         *int                      `json:"priority,omitempty"`
	Cadence          *runtime.ObjectiveCadence `json:"cadence,omitempty"`
	EventRules       map[string]interface{}    `json:"eventRules,omitempty"`
	Budget           *runtime.BudgetPolicy     `json:"budget,omitempty"`
	Constraints      map[string]interface{}    `json:"constraints,omitempty"`
	SuccessCriteria  map[string]interface{}    `json:"successCriteria,omitempty"`
	ProgressSummary  *string                   `json:"progressSummary,omitempty"`
	NextEvaluationAt *time.Time                `json:"nextEvaluationAt,omitempty"`
}

type ObjectiveDetail struct {
	Objective *runtime.Objective  `json:"objective"`
	Runs      []*runtime.AgentRun `json:"runs"`
}

type CreateInitiativeRequest struct {
	ID             string                           `json:"id,omitempty"`
	Scope          runtime.Scope                    `json:"scope"`
	Owner          runtime.ObjectiveOwner           `json:"owner"`
	Title          string                           `json:"title"`
	Purpose        string                           `json:"purpose"`
	Status         runtime.InitiativeStatus         `json:"status,omitempty"`
	AgentRefs      []runtime.ResourceReference      `json:"agentRefs,omitempty"`
	TeamRefs       []runtime.ResourceReference      `json:"teamRefs,omitempty"`
	ObjectiveRefs  []string                         `json:"objectiveRefs"`
	RunRefs        []string                         `json:"runRefs,omitempty"`
	Milestones     []runtime.InitiativeMilestone    `json:"milestones,omitempty"`
	Hypotheses     []runtime.InitiativeHypothesis   `json:"hypotheses,omitempty"`
	SourceMonitors []runtime.SourceMonitorReference `json:"sourceMonitors,omitempty"`
	Deliverables   []runtime.InitiativeDeliverable  `json:"deliverables,omitempty"`
	Budget         *runtime.BudgetPolicy            `json:"budget,omitempty"`
	Policy         map[string]interface{}           `json:"policy,omitempty"`
	Checkpoint     map[string]interface{}           `json:"checkpoint,omitempty"`
	IdempotencyKey string                           `json:"idempotencyKey,omitempty"`
}

type UpdateInitiativeRequest struct {
	ExpectedRevision int64                             `json:"expectedRevision"`
	Title            *string                           `json:"title,omitempty"`
	Purpose          *string                           `json:"purpose,omitempty"`
	Status           *runtime.InitiativeStatus         `json:"status,omitempty"`
	AgentRefs        *[]runtime.ResourceReference      `json:"agentRefs,omitempty"`
	TeamRefs         *[]runtime.ResourceReference      `json:"teamRefs,omitempty"`
	ObjectiveRefs    *[]string                         `json:"objectiveRefs,omitempty"`
	RunRefs          *[]string                         `json:"runRefs,omitempty"`
	Milestones       *[]runtime.InitiativeMilestone    `json:"milestones,omitempty"`
	Hypotheses       *[]runtime.InitiativeHypothesis   `json:"hypotheses,omitempty"`
	SourceMonitors   *[]runtime.SourceMonitorReference `json:"sourceMonitors,omitempty"`
	Deliverables     *[]runtime.InitiativeDeliverable  `json:"deliverables,omitempty"`
	Budget           *runtime.BudgetPolicy             `json:"budget,omitempty"`
	ClearBudget      bool                              `json:"clearBudget,omitempty"`
	Policy           map[string]interface{}            `json:"policy,omitempty"`
	Checkpoint       map[string]interface{}            `json:"checkpoint,omitempty"`
}

type AgentRunCommandRequest struct {
	ExpectedRevision int64                       `json:"expectedRevision"`
	Kind             runtime.AgentRunCommandKind `json:"kind"`
	Actor            runtime.ActivityActor       `json:"actor,omitempty"`
	Summary          string                      `json:"summary,omitempty"`
	Instruction      string                      `json:"instruction,omitempty"`
	Visibility       runtime.ActivityVisibility  `json:"visibility,omitempty"`
}

type ResolveArtifactContentRequest struct {
	Actor      runtime.ActivityActor `json:"actor"`
	Purpose    string                `json:"purpose"`
	TTLSeconds int64                 `json:"ttlSeconds"`
}

type CreateConversationRequest struct {
	ID             string                 `json:"id,omitempty"`
	Scope          runtime.Scope          `json:"scope"`
	Owner          runtime.ObjectiveOwner `json:"owner"`
	Title          string                 `json:"title"`
	IdempotencyKey string                 `json:"idempotencyKey,omitempty"`
}

type PostChannelMessageRequest struct {
	ID                  string                            `json:"id,omitempty"`
	Scope               runtime.Scope                     `json:"scope"`
	ExpectedRevision    int64                             `json:"expectedRevision"`
	Sender              runtime.ConversationParticipant   `json:"sender"`
	Intent              runtime.ConversationMessageIntent `json:"intent"`
	Content             string                            `json:"content"`
	Audience            runtime.ConversationAudience      `json:"audience"`
	ReplyToMessageID    string                            `json:"replyToMessageId,omitempty"`
	Mentions            []runtime.ConversationParticipant `json:"mentions,omitempty"`
	References          []runtime.ConversationReference   `json:"references,omitempty"`
	RequiresResponse    bool                              `json:"requiresResponse,omitempty"`
	ResolvesMessageID   string                            `json:"resolvesMessageId,omitempty"`
	SupersedesMessageID string                            `json:"supersedesMessageId,omitempty"`
	IdempotencyKey      string                            `json:"idempotencyKey,omitempty"`
}

type CoordinateParticipationRequest struct {
	ID               string                                `json:"id,omitempty"`
	Scope            runtime.Scope                         `json:"scope"`
	ExpectedRevision int64                                 `json:"expectedRevision"`
	TriggerMessageID string                                `json:"triggerMessageId,omitempty"`
	Policy           runtime.ConversationArbitrationPolicy `json:"policy,omitempty"`
	Proposals        []runtime.ParticipationProposal       `json:"proposals"`
	IdempotencyKey   string                                `json:"idempotencyKey,omitempty"`
}

type AdvanceConversationCursorRequest struct {
	Scope             runtime.Scope                   `json:"scope"`
	Participant       runtime.ConversationParticipant `json:"participant"`
	ExpectedRevision  int64                           `json:"expectedRevision,omitempty"`
	DeliveredSequence int64                           `json:"deliveredSequence"`
	ReadSequence      int64                           `json:"readSequence"`
}

type SetConversationPresenceRequest struct {
	Scope            runtime.Scope                     `json:"scope"`
	Participant      runtime.ConversationParticipant   `json:"participant"`
	State            runtime.ConversationPresenceState `json:"state"`
	Summary          string                            `json:"summary,omitempty"`
	RunID            string                            `json:"runId,omitempty"`
	LeaseID          string                            `json:"leaseId,omitempty"`
	ExpectedRevision int64                             `json:"expectedRevision,omitempty"`
	TTLSeconds       int64                             `json:"ttlSeconds,omitempty"`
}

type ReleaseConversationPresenceRequest struct {
	Scope       runtime.Scope                   `json:"scope"`
	Participant runtime.ConversationParticipant `json:"participant"`
	LeaseID     string                          `json:"leaseId"`
}
