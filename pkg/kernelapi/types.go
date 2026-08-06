// Package kernelapi defines the versioned HTTP contract shared by the
// standalone OpenSeal daemon and thin clients such as the terminal UI.
package kernelapi

import (
	"time"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runtime"
	"github.com/axiom-studio/openseal/pkg/skill/clawhub"
	"github.com/axiom-studio/openseal/pkg/source"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
	"github.com/axiom-studio/openseal/pkg/workforce"
)

const (
	APIVersion                                 = "agent-kernel/v1"
	AgentRunsCapabilityID                      = "agent-runs"
	AgentRunsCapabilityVersion                 = "2"
	ObjectivesCapabilityID                     = "objectives"
	ObjectivesCapabilityVersion                = "1"
	RunbooksCapabilityID                       = "runbooks"
	RunbooksCapabilityVersion                  = "2"
	ProjectsCapabilityID                       = "projects"
	ProjectsCapabilityVersion                  = "1"
	SourceMonitorsCapabilityID                 = "source-monitors"
	SourceMonitorsCapabilityVersion            = "1"
	OutreachCapabilityID                       = "outreach"
	OutreachCapabilityVersion                  = "1"
	SkillActionsCapabilityID                   = "skill-actions"
	SkillActionsCapabilityVersion              = "1"
	SkillBindingsCapabilityID                  = "skill-bindings"
	SkillBindingsCapabilityVersion             = "2"
	ArtifactsCapabilityID                      = "artifacts"
	ArtifactsCapabilityVersion                 = "1"
	ChannelsCapabilityID                       = "channels"
	ChannelsCapabilityVersion                  = "4"
	TeamDefinitionsCapabilityID                = "team-definitions"
	TeamDefinitionsCapabilityVersion           = "4"
	WorkforceAuthoringCapabilityID             = "workforce-authoring"
	WorkforceAuthoringCapabilityVersion        = "12"
	WorkforceExecutionTargetsCapabilityID      = "workforce-execution-targets"
	WorkforceExecutionTargetsCapabilityVersion = "1"
	WorkforceBundlesCapabilityID               = "workforce-bundles"
	WorkforceBundlesCapabilityVersion          = "1"
	ClawHubLifecycleCapabilityID               = "clawhub-lifecycle"
	ClawHubLifecycleCapabilityVersion          = clawhub.LifecycleAPIVersion
	AgentDefinitionsCapabilityID               = "agent-definitions"
	AgentDefinitionsCapabilityVersion          = "8"
	AgentRequestsCapabilityID                  = "agent-requests"
	AgentRequestsCapabilityVersion             = "1"
	ActionApprovalsCapabilityID                = "action-approvals"
	ActionApprovalsCapabilityVersion           = "1"
	ActionCallsCapabilityID                    = "action-calls"
	ActionCallsCapabilityVersion               = "1"
	AgentTurnsCapabilityID                     = "agent-turns"
	AgentTurnsCapabilityVersion                = "1"
	ActivityCapabilityID                       = "activity"
	ActivityCapabilityVersion                  = "1"
	EventRoutingCapabilityID                   = "event-routing"
	EventRoutingCapabilityVersion              = "1"
	ConversationGatewaysCapabilityID           = "conversation-gateways"
	ConversationGatewaysCapabilityVersion      = "2"
	CallbacksCapabilityID                      = "callbacks"
	CallbacksCapabilityVersion                 = "1"
	SourcePoliciesCapabilityID                 = "source-policies"
	SourcePoliciesCapabilityVersion            = source.LifecycleAPIVersion
)

const (
	EventSourceSubscriptionsCapabilityID      = "event-source-subscriptions"
	EventSourceSubscriptionsCapabilityVersion = "1"
)

const (
	OperationCreate               = "create"
	OperationExecute              = "execute"
	OperationGet                  = "get"
	OperationList                 = "list"
	OperationPause                = "pause"
	OperationResume               = "resume"
	OperationRetire               = "retire"
	OperationCancel               = "cancel"
	OperationIntervene            = "intervene"
	OperationUpdate               = "update"
	OperationRegister             = "register"
	OperationUpload               = "upload"
	OperationDownload             = "download"
	OperationResolve              = "resolve"
	OperationRespond              = "respond"
	OperationComplete             = "complete"
	OperationPost                 = "post"
	OperationCoordinate           = "coordinate"
	OperationRead                 = "read"
	OperationPresence             = "presence"
	OperationAudit                = "audit"
	OperationChanges              = "changes"
	OperationReceipts             = "receipts"
	OperationCoordinateAuto       = "coordinate-automatically"
	OperationStream               = "stream"
	OperationDeploy               = "deploy"
	OperationActivate             = "activate"
	OperationRollback             = "rollback"
	OperationListActivations      = "list-activations"
	OperationCompile              = "compile"
	OperationSearch               = "search"
	OperationPropose              = "propose"
	OperationEvaluate             = "evaluate"
	OperationApprove              = "approve"
	OperationApply                = "apply"
	OperationRetry                = "retry"
	OperationPatch                = "patch"
	OperationListCompilations     = "list-compilations"
	OperationListObservations     = "list-observations"
	OperationGetCheckpoint        = "get-checkpoint"
	OperationDeliver              = "deliver"
	OperationProposeAmendment     = "propose-amendment"
	OperationListAmendments       = "list-amendments"
	OperationGetAmendment         = "get-amendment"
	OperationEvaluateAmendment    = "evaluate-amendment"
	OperationResolveAmendment     = "resolve-amendment"
	OperationActivateAmendment    = "activate-amendment"
	OperationRecoverParticipation = "recover-participation"
	OperationRoute                = "route"
	OperationReconcile            = "reconcile"
	OperationReportHealth         = "report-health"
	OperationAdvanceCheckpoint    = "advance-checkpoint"
	OperationUpsert               = "upsert"
	OperationDisable              = "disable"
	OperationPlanUpgrade          = "plan-upgrade"
	OperationApplyUpgrade         = "apply-upgrade"
	OperationValidate             = "validate"
	OperationInspect              = "inspect"
	OperationCompare              = "compare"
	OperationPreviewInstallation  = "preview-installation"
	OperationInstall              = "install"
	OperationRefine               = "refine"
	OperationRevoke               = "revoke"
	OperationInspectAdmission     = "inspect-admission"
	OperationListVersions         = "list-versions"
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

type SkillBindingList struct {
	APIVersion   string                `json:"apiVersion"`
	DeploymentID string                `json:"deploymentId"`
	Items        []*capability.Binding `json:"items"`
}

type SkillBindingMutationResult struct {
	APIVersion string              `json:"apiVersion"`
	Binding    *capability.Binding `json:"binding"`
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
	Amendments            bool
	ParticipationRecovery bool
}

// AgentDefinitionCapabilityFeatures describes optional lifecycle services
// backed by the same immutable definition and deployment registry. Hosts still
// remove individual operations after composing tenant authorization.
type AgentDefinitionCapabilityFeatures struct {
	Lifecycle            bool
	Amendments           bool
	PortableInstallation bool
}

// WorkforceAuthoringCapabilityFeatures describes host-wide authoring services.
// Resource-specific lifecycle authority is composed into a contextual
// capability response and must never be inferred from these feature flags.
type WorkforceAuthoringCapabilityFeatures struct {
	ChangeSets  bool
	SkillSearch bool
}

// WorkforceExecutionTarget is the product-neutral placement surface used to
// choose where authored work may execute. Embedding hosts retain cluster,
// credential, and transport details behind the opaque target ID.
type WorkforceExecutionTarget struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	Environment string `json:"environment"`
	Status      string `json:"status"`
	Revision    int64  `json:"revision"`
}

type WorkforceExecutionTargetCandidate struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	Environment string `json:"environment"`
}

type WorkforceExecutionTargetList struct {
	Items      []WorkforceExecutionTarget          `json:"items"`
	Candidates []WorkforceExecutionTargetCandidate `json:"candidates,omitempty"`
}

// ConversationGatewayAdapterChoice is a secret-free, exact adapter binding
// that the current principal may use to create shared message routing. Hosts
// derive these choices from their active deployment and Skill registries.
type ConversationGatewayAdapterChoice struct {
	ID              string `json:"id"`
	DisplayName     string `json:"displayName"`
	DeploymentID    string `json:"deploymentId"`
	Provider        string `json:"provider"`
	SkillID         string `json:"skillId"`
	SkillVersion    string `json:"skillVersion"`
	SourceIdentity  string `json:"sourceIdentity,omitempty"`
	BindingID       string `json:"bindingId"`
	BindingRevision int64  `json:"bindingRevision"`
	AdapterID       string `json:"adapterId"`
}

// CallbackAdapterChoice is a secret-free, exact callback adapter binding that
// the current principal may use to create a public callback registration.
type CallbackAdapterChoice struct {
	ID              string   `json:"id"`
	DisplayName     string   `json:"displayName"`
	DeploymentID    string   `json:"deploymentId"`
	Provider        string   `json:"provider"`
	SkillID         string   `json:"skillId"`
	SkillVersion    string   `json:"skillVersion"`
	SourceIdentity  string   `json:"sourceIdentity,omitempty"`
	BindingID       string   `json:"bindingId"`
	BindingRevision int64    `json:"bindingRevision"`
	AdapterID       string   `json:"adapterId"`
	EventTypes      []string `json:"eventTypes"`
}

type ActionApprovalCapabilityFeatures struct {
	Resolution bool
}

// CapabilityBlockingRequirement is a safe, server-authored explanation of
// configuration that must be supplied before an operation can run. Codes and
// credential keys are stable machine fields; Message is operator-facing and
// must never contain credential material.
type CapabilityBlockingRequirement struct {
	Code          string `json:"code"`
	CredentialKey string `json:"credentialKey,omitempty"`
	Message       string `json:"message"`
}

// CapabilityContext is server-authored authorization state for one explicitly
// requested resource. It is never durable policy input and clients must not
// infer authority from the underlying resource itself.
type CapabilityContext struct {
	ChangeSetID                  string                                       `json:"changeSetId,omitempty"`
	Revision                     int64                                        `json:"revision,omitempty"`
	EligibleApprovalRequirements []ApprovalRequirementReference               `json:"eligibleApprovalRequirements,omitempty"`
	CredentialBindings           []capability.CredentialBindingChoice         `json:"credentialBindings,omitempty"`
	BindingConfigurationFields   []capability.BindingConfigurationFieldChoice `json:"bindingConfigurationFields,omitempty"`
	BlockingRequirements         []CapabilityBlockingRequirement              `json:"blockingRequirements,omitempty"`
	ConversationGatewayAdapters  []ConversationGatewayAdapterChoice           `json:"conversationGatewayAdapters,omitempty"`
	CallbackAdapters             []CallbackAdapterChoice                      `json:"callbackAdapters,omitempty"`
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
	Items []TeamDeploymentCatalogEntry `json:"items"`
}

type TeamDefinitionAmendmentList struct {
	Items []*kernelteam.DefinitionAmendment `json:"items"`
}

type ActivateTeamDefinitionAmendmentRequest struct {
	Scope            capability.ScopeReference `json:"scope"`
	ExpectedRevision int64                     `json:"expectedRevision"`
	ActorType        string                    `json:"actorType"`
	ActorID          string                    `json:"actorId"`
	Reason           string                    `json:"reason,omitempty"`
}

type TeamDefinitionAmendmentActivationResult struct {
	Amendment  *kernelteam.DefinitionAmendment `json:"amendment"`
	Deployment *kernelteam.Deployment          `json:"deployment"`
	Activation *workforce.DefinitionActivation `json:"activation"`
}

type TeamDeploymentCatalogEntry struct {
	Deployment *kernelteam.Deployment `json:"deployment"`
	Definition *kernelteam.Definition `json:"definition"`
}

type AgentDeploymentList struct {
	Items []AgentDeploymentCatalogEntry `json:"items"`
}

type AgentDeploymentCatalogEntry struct {
	Deployment *kernelagent.AgentDeployment `json:"deployment"`
	Definition *kernelagent.AgentDefinition `json:"definition"`
}

type UpdateAgentDeploymentRequest struct {
	Deployment       *kernelagent.AgentDeployment `json:"deployment"`
	ExpectedRevision int64                        `json:"expectedRevision"`
	ActorType        string                       `json:"actorType"`
	ActorID          string                       `json:"actorId"`
	Reason           string                       `json:"reason"`
}

type AgentDeploymentUpdateResult struct {
	Deployment *kernelagent.AgentDeployment    `json:"deployment"`
	Audit      *workforce.DefinitionActivation `json:"audit"`
}

type ActivateAgentDefinitionRequest struct {
	Scope            capability.ScopeReference `json:"scope"`
	Version          string                    `json:"version"`
	ExpectedRevision int64                     `json:"expectedRevision"`
	ActorType        string                    `json:"actorType"`
	ActorID          string                    `json:"actorId"`
	Reason           string                    `json:"reason,omitempty"`
}

type RollbackAgentDefinitionRequest struct {
	Scope            capability.ScopeReference `json:"scope"`
	ExpectedRevision int64                     `json:"expectedRevision"`
	ActorType        string                    `json:"actorType"`
	ActorID          string                    `json:"actorId"`
	Reason           string                    `json:"reason,omitempty"`
}

type AgentDefinitionActivationResult struct {
	Deployment *kernelagent.AgentDeployment    `json:"deployment"`
	Activation *workforce.DefinitionActivation `json:"activation"`
}

type AgentDefinitionAmendmentList struct {
	Items []*kernelagent.DefinitionAmendment `json:"items"`
}

type ActivateAgentDefinitionAmendmentRequest struct {
	Scope            capability.ScopeReference `json:"scope"`
	ExpectedRevision int64                     `json:"expectedRevision"`
	ActorType        string                    `json:"actorType"`
	ActorID          string                    `json:"actorId"`
	Reason           string                    `json:"reason,omitempty"`
}

type AgentDefinitionAmendmentActivationResult struct {
	Amendment  *kernelagent.DefinitionAmendment `json:"amendment"`
	Deployment *kernelagent.AgentDeployment     `json:"deployment"`
	Activation *workforce.DefinitionActivation  `json:"activation"`
}

// AgentDefinitionCompilationHistory is the canonical readiness history for an
// Agent deployment. Latest is projected explicitly so interactive clients do
// not need to infer ordering, while Items preserves the complete bounded
// history returned by the store.
type AgentDefinitionCompilationHistory struct {
	APIVersion string                               `json:"apiVersion"`
	Latest     *kernelagent.DefinitionCompilation   `json:"latest,omitempty"`
	Items      []*kernelagent.DefinitionCompilation `json:"items"`
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

func AgentRunsCapability(operations ...string) Capability {
	if len(operations) == 0 {
		operations = []string{
			OperationCreate, OperationGet, OperationList, OperationPause,
			OperationResume, OperationCancel, OperationIntervene, OperationAudit, OperationInspectAdmission,
		}
	}
	return Capability{
		ID: AgentRunsCapabilityID, Version: AgentRunsCapabilityVersion, Available: len(operations) > 0,
		Operations: append([]string(nil), operations...),
	}
}

func ObjectivesCapability() Capability {
	return Capability{
		ID: ObjectivesCapabilityID, Version: ObjectivesCapabilityVersion, Available: true,
		Operations: []string{OperationCreate, OperationGet, OperationList, OperationUpdate},
	}
}

func RunbooksCapability() Capability {
	return Capability{
		ID: RunbooksCapabilityID, Version: RunbooksCapabilityVersion, Available: true,
		Operations: []string{OperationGet, OperationList, OperationUpdate, OperationExecute, OperationReconcile},
	}
}

func EventSourceSubscriptionsCapability() Capability {
	return Capability{
		ID: EventSourceSubscriptionsCapabilityID, Version: EventSourceSubscriptionsCapabilityVersion, Available: true,
		Operations: []string{OperationCreate, OperationGet, OperationList, OperationUpdate, OperationRetire, OperationReportHealth, OperationGetCheckpoint, OperationAdvanceCheckpoint},
	}
}

// ConversationGatewaysCapability describes lifecycle management for durable,
// shared provider webhooks. Provider ingress itself is intentionally absent:
// it is an opaque-route transport boundary rather than an operator command.
func ConversationGatewaysCapability(management bool, choices ...[]ConversationGatewayAdapterChoice) Capability {
	operations := []string{OperationGet, OperationList}
	if management {
		operations = append(operations, OperationCreate, OperationUpdate)
	}
	result := Capability{
		ID: ConversationGatewaysCapabilityID, Version: ConversationGatewaysCapabilityVersion,
		Available: true, Operations: operations,
	}
	if len(choices) > 0 && len(choices[0]) > 0 {
		result.Context = &CapabilityContext{ConversationGatewayAdapters: choices[0]}
	}
	return result
}

// CallbacksCapability describes lifecycle management for durable callback
// registrations. Public ingress is a transport boundary, not an operator
// command, so it is intentionally absent from the operation list.
func CallbacksCapability(management bool, choices ...[]CallbackAdapterChoice) Capability {
	operations := []string{OperationGet, OperationList}
	if management {
		operations = append(operations, OperationCreate, OperationUpdate)
	}
	result := Capability{
		ID: CallbacksCapabilityID, Version: CallbacksCapabilityVersion,
		Available: true, Operations: operations,
	}
	if len(choices) > 0 && len(choices[0]) > 0 {
		result.Context = &CapabilityContext{CallbackAdapters: choices[0]}
	}
	return result
}

func ActionCallsCapability() Capability {
	return Capability{
		ID: ActionCallsCapabilityID, Version: ActionCallsCapabilityVersion, Available: true,
		Operations: []string{OperationGet, OperationList},
	}
}

func AgentTurnsCapability() Capability {
	return Capability{
		ID: AgentTurnsCapabilityID, Version: AgentTurnsCapabilityVersion, Available: true,
		Operations: []string{OperationGet, OperationList},
	}
}

func EventRoutingCapability() Capability {
	return Capability{
		ID: EventRoutingCapabilityID, Version: EventRoutingCapabilityVersion, Available: true,
		Operations: []string{OperationRoute},
	}
}

func ProjectsCapability() Capability {
	return Capability{
		ID: ProjectsCapabilityID, Version: ProjectsCapabilityVersion, Available: true,
		Operations: []string{OperationCreate, OperationGet, OperationList, OperationPatch},
	}
}

func SourceMonitorsCapability() Capability {
	return Capability{ID: SourceMonitorsCapabilityID, Version: SourceMonitorsCapabilityVersion, Available: true, Operations: []string{OperationListObservations, OperationGetCheckpoint}}
}

func OutreachCapability(operations ...string) Capability {
	if len(operations) == 0 {
		operations = []string{OperationCreate, OperationGet, OperationList, OperationDeliver}
	}
	return Capability{ID: OutreachCapabilityID, Version: OutreachCapabilityVersion, Available: len(operations) > 0, Operations: append([]string(nil), operations...)}
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
	return NewCapabilityDocument(ObjectivesCapability(), RunbooksCapability(), EventSourceSubscriptionsCapability(), EventRoutingCapability(), ProjectsCapability(), SourceMonitorsCapability(), OutreachCapability(), SkillActionsCapability(), SkillBindingsCapability(true), ActivityCapability(), AgentRunsCapability(), AgentTurnsCapability(), ActionCallsCapability(), AgentDefinitionsCapability(), ChannelsCapability(ChannelCapabilityFeatures{Coordination: true, Changes: true}), TeamDefinitionsCapability(TeamDefinitionCapabilityFeatures{}))
}

// ActivityCapability exposes the selector-bounded, redacted audit projection.
// Raw append authority is deliberately not part of the interactive contract.
func ActivityCapability() Capability {
	return Capability{ID: ActivityCapabilityID, Version: ActivityCapabilityVersion, Available: true, Operations: []string{OperationList}}
}

// SourcePoliciesCapability is the portable, credential-free governance
// contract for immutable source policy versions and their live authority.
func SourcePoliciesCapability() Capability {
	return Capability{ID: SourcePoliciesCapabilityID, Version: SourcePoliciesCapabilityVersion, Available: true,
		Operations: []string{OperationRegister, OperationGet, OperationList, OperationListVersions, OperationActivate, OperationRevoke, OperationListActivations}}
}

func AgentDefinitionsCapability(features ...AgentDefinitionCapabilityFeatures) Capability {
	operations := []string{OperationGet, OperationList, OperationUpdate, OperationListCompilations}
	if len(features) > 0 && features[0].PortableInstallation {
		operations = append(operations, OperationDeploy)
	}
	if len(features) > 0 && features[0].Lifecycle {
		operations = append(operations, OperationActivate, OperationRollback, OperationListActivations)
	}
	if len(features) > 0 && features[0].Amendments {
		operations = append(operations, OperationProposeAmendment, OperationListAmendments, OperationGetAmendment,
			OperationEvaluateAmendment, OperationResolveAmendment, OperationActivateAmendment)
	}
	return Capability{ID: AgentDefinitionsCapabilityID, Version: AgentDefinitionsCapabilityVersion, Available: true, Operations: operations}
}

func SkillBindingsCapability(management bool) Capability {
	result := Capability{ID: SkillBindingsCapabilityID, Version: SkillBindingsCapabilityVersion, Available: true, Operations: []string{OperationGet, OperationList}}
	if management {
		result.Operations = append(result.Operations, OperationUpsert, OperationDisable, OperationPlanUpgrade, OperationApplyUpgrade)
	}
	return result
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
		capability.Operations = append(capability.Operations, OperationProposeAmendment, OperationListAmendments, OperationGetAmendment, OperationEvaluateAmendment, OperationResolveAmendment, OperationActivateAmendment)
	}
	if features.ParticipationRecovery {
		capability.Operations = append(capability.Operations, OperationRecoverParticipation)
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
	if features.SkillSearch {
		capability.Operations = append(capability.Operations, OperationSearch)
	}
	return capability
}

func WorkforceExecutionTargetsCapability(management bool) Capability {
	operations := []string{OperationGet, OperationList}
	if management {
		operations = append(operations, OperationCreate, OperationUpdate)
	}
	return Capability{
		ID: WorkforceExecutionTargetsCapabilityID, Version: WorkforceExecutionTargetsCapabilityVersion,
		Available: true, Operations: operations,
	}
}

func WorkforceBundlesCapability(installation bool) Capability {
	operations := []string{OperationValidate, OperationInspect, OperationCompare, OperationPreviewInstallation, OperationPlanUpgrade}
	if installation {
		operations = append(operations, OperationInstall)
	}
	return Capability{ID: WorkforceBundlesCapabilityID, Version: WorkforceBundlesCapabilityVersion, Available: true, Operations: operations}
}

func NewCapabilityDocument(capabilities ...Capability) CapabilityDocument {
	return CapabilityDocument{APIVersion: APIVersion, Capabilities: append([]Capability(nil), capabilities...)}
}

// CreateAgentRunRequest is the public request body for canonical durable work.
// Scope is explicit because standalone OpenSeal can host more than one local
// workspace even though the TUI defaults to local/default.
type CreateAgentRunRequest struct {
	Scope                runtime.Scope              `json:"scope"`
	Kind                 runtime.RunKind            `json:"kind,omitempty"`
	ObjectiveID          string                     `json:"objectiveId,omitempty"`
	ParentRunID          string                     `json:"parentRunId,omitempty"`
	Owner                runtime.ObjectiveOwner     `json:"owner"`
	AssignedAgentID      string                     `json:"assignedAgentId,omitempty"`
	Entrypoint           string                     `json:"entrypoint,omitempty"`
	ConcurrencyKey       string                     `json:"concurrencyKey,omitempty"`
	ResourceRequirements map[string]int             `json:"resourceRequirements,omitempty"`
	Goal                 string                     `json:"goal"`
	Source               runtime.RunSource          `json:"source"`
	Priority             int                        `json:"priority,omitempty"`
	Deadline             *time.Time                 `json:"deadline,omitempty"`
	AvailableAt          *time.Time                 `json:"availableAt,omitempty"`
	Context              map[string]interface{}     `json:"context,omitempty"`
	Plan                 map[string]interface{}     `json:"plan,omitempty"`
	Checkpoint           map[string]interface{}     `json:"checkpoint,omitempty"`
	WakeCondition        *runtime.WakeCondition     `json:"wakeCondition,omitempty"`
	Budget               *runtime.BudgetPolicy      `json:"budget,omitempty"`
	Policy               map[string]interface{}     `json:"policy,omitempty"`
	IdempotencyKey       string                     `json:"idempotencyKey,omitempty"`
	Actor                runtime.ActivityActor      `json:"actor,omitempty"`
	Visibility           runtime.ActivityVisibility `json:"visibility,omitempty"`
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
	ChildCheckpoint      map[string]interface{}        `json:"childCheckpoint,omitempty"`
	ConversationRefs     []string                      `json:"conversationRefs,omitempty"`
	BudgetAllocation     *runtime.BudgetPolicy         `json:"budgetAllocation,omitempty"`
	IdempotencyKey       string                        `json:"idempotencyKey,omitempty"`
}

type RespondAgentRequestRequest struct {
	ExpectedRevision int64                        `json:"expectedRevision"`
	Decision         runtime.AgentRequestDecision `json:"decision"`
	Principal        runtime.CollaborationParty   `json:"principal"`
	Actor            runtime.CollaborationParty   `json:"actor,omitempty"`
	AssignedAgentID  string                       `json:"assignedAgentId,omitempty"`
	Message          string                       `json:"message,omitempty"`
	DecisionRunID    string                       `json:"decisionRunId,omitempty"`
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

type ReviewAgentRequestCompletionRequest struct {
	ExpectedRevision      int64                      `json:"expectedRevision"`
	ExpectedChildRevision int64                      `json:"expectedChildRevision"`
	Principal             runtime.CollaborationParty `json:"principal"`
	Actor                 runtime.CollaborationParty `json:"actor,omitempty"`
	Approve               bool                       `json:"approve"`
	Summary               string                     `json:"summary"`
	IdempotencyKey        string                     `json:"idempotencyKey,omitempty"`
}

type ResolveActionApprovalRequest struct {
	ExpectedRevision int64                     `json:"expectedRevision"`
	DecisionID       string                    `json:"decisionId,omitempty"`
	Approve          bool                      `json:"approve"`
	Principal        runtime.ApprovalPrincipal `json:"principal"`
	Reason           string                    `json:"reason,omitempty"`
}

type CreateObjectiveRequest struct {
	Scope           runtime.Scope           `json:"scope"`
	Owner           runtime.ObjectiveOwner  `json:"owner"`
	Title           string                  `json:"title"`
	Goal            string                  `json:"goal"`
	Status          runtime.ObjectiveStatus `json:"status,omitempty"`
	Priority        int                     `json:"priority,omitempty"`
	Budget          *runtime.BudgetPolicy   `json:"budget,omitempty"`
	Constraints     map[string]interface{}  `json:"constraints,omitempty"`
	SuccessCriteria map[string]interface{}  `json:"successCriteria,omitempty"`
	IdempotencyKey  string                  `json:"idempotencyKey,omitempty"`
}

type UpdateObjectiveRequest struct {
	ExpectedRevision int64                    `json:"expectedRevision"`
	Title            *string                  `json:"title,omitempty"`
	Goal             *string                  `json:"goal,omitempty"`
	Status           *runtime.ObjectiveStatus `json:"status,omitempty"`
	Priority         *int                     `json:"priority,omitempty"`
	Budget           *runtime.BudgetPolicy    `json:"budget,omitempty"`
	Constraints      map[string]interface{}   `json:"constraints,omitempty"`
	SuccessCriteria  map[string]interface{}   `json:"successCriteria,omitempty"`
	ProgressSummary  *string                  `json:"progressSummary,omitempty"`
}

type ObjectiveDetail struct {
	Objective *runtime.Objective           `json:"objective"`
	Runbooks  []*runtime.RunbookActivation `json:"runbooks"`
	Runs      []*runtime.AgentRun          `json:"runs"`
}

type ReconcileRunbookSchedulesRequest struct {
	Scope runtime.Scope `json:"scope"`
	Limit int           `json:"limit,omitempty"`
}

type RunbookScheduleReconciliation struct {
	Scope        runtime.Scope                  `json:"scope"`
	ReconciledAt time.Time                      `json:"reconciledAt"`
	Result       *runtime.RunbookScheduleResult `json:"result"`
}

type CreateEventSourceSubscriptionRequest struct {
	ID                  string                                `json:"id,omitempty"`
	Scope               runtime.Scope                         `json:"scope"`
	Owner               runtime.ObjectiveOwner                `json:"owner"`
	DisplayName         string                                `json:"displayName"`
	Description         string                                `json:"description,omitempty"`
	Source              string                                `json:"source"`
	Connector           runtime.EventSourceConnector          `json:"connector"`
	Status              runtime.EventSourceSubscriptionStatus `json:"status,omitempty"`
	EventTypes          []string                              `json:"eventTypes"`
	Parameters          map[string]interface{}                `json:"parameters,omitempty"`
	PollIntervalSeconds int64                                 `json:"pollIntervalSeconds,omitempty"`
}

type CreateExternalConversationGatewayRequest struct {
	ID      string                                     `json:"id,omitempty"`
	Name    string                                     `json:"name"`
	Gateway runtime.ExternalConversationIngressGateway `json:"gateway"`
	Status  runtime.ExternalConversationGatewayStatus  `json:"status,omitempty"`
	Reason  string                                     `json:"reason"`
}

type UpdateExternalConversationGatewayRequest struct {
	ExpectedRevision int64                                       `json:"expectedRevision"`
	Name             *string                                     `json:"name,omitempty"`
	Gateway          *runtime.ExternalConversationIngressGateway `json:"gateway,omitempty"`
	Status           *runtime.ExternalConversationGatewayStatus  `json:"status,omitempty"`
	Reason           string                                      `json:"reason"`
}

type UpdateEventSourceSubscriptionRequest struct {
	ExpectedRevision    int64                                  `json:"expectedRevision"`
	DisplayName         *string                                `json:"displayName,omitempty"`
	Description         *string                                `json:"description,omitempty"`
	Connector           *runtime.EventSourceConnector          `json:"connector,omitempty"`
	Status              *runtime.EventSourceSubscriptionStatus `json:"status,omitempty"`
	EventTypes          *[]string                              `json:"eventTypes,omitempty"`
	Parameters          map[string]interface{}                 `json:"parameters,omitempty"`
	ReplaceParameters   bool                                   `json:"replaceParameters,omitempty"`
	PollIntervalSeconds *int64                                 `json:"pollIntervalSeconds,omitempty"`
}

type RetireEventSourceSubscriptionRequest struct {
	ExpectedRevision int64 `json:"expectedRevision"`
}

type ReportEventSourceHealthRequest struct {
	ExpectedHealthRevision       int64                          `json:"expectedHealthRevision"`
	ObservedSubscriptionRevision int64                          `json:"observedSubscriptionRevision"`
	State                        runtime.EventSourceHealthState `json:"state"`
	LastEventAt                  time.Time                      `json:"lastEventAt,omitempty"`
	ErrorCode                    string                         `json:"errorCode,omitempty"`
	Summary                      string                         `json:"summary,omitempty"`
}

type AdvanceEventSourceCheckpointRequest struct {
	ExpectedRevision             int64     `json:"expectedRevision"`
	ObservedSubscriptionRevision int64     `json:"observedSubscriptionRevision"`
	Cursor                       string    `json:"cursor,omitempty"`
	EventIDs                     []string  `json:"eventIds,omitempty"`
	Watermark                    time.Time `json:"watermark,omitempty"`
}

type CreateProjectRequest struct {
	ID             string                           `json:"id,omitempty"`
	Scope          runtime.Scope                    `json:"scope"`
	Owner          runtime.ObjectiveOwner           `json:"owner"`
	Title          string                           `json:"title"`
	Purpose        string                           `json:"purpose"`
	Status         runtime.ProjectStatus            `json:"status,omitempty"`
	AgentRefs      []runtime.ResourceReference      `json:"agentRefs,omitempty"`
	TeamRefs       []runtime.ResourceReference      `json:"teamRefs,omitempty"`
	ObjectiveRefs  []string                         `json:"objectiveRefs"`
	Milestones     []runtime.ProjectMilestone       `json:"milestones,omitempty"`
	Hypotheses     []runtime.ProjectHypothesis      `json:"hypotheses,omitempty"`
	SourceMonitors []runtime.SourceMonitorReference `json:"sourceMonitors,omitempty"`
	Deliverables   []runtime.ProjectDeliverable     `json:"deliverables,omitempty"`
	Budget         *runtime.BudgetPolicy            `json:"budget,omitempty"`
	Policy         map[string]interface{}           `json:"policy,omitempty"`
	Checkpoint     map[string]interface{}           `json:"checkpoint,omitempty"`
	IdempotencyKey string                           `json:"idempotencyKey,omitempty"`
}

type UpdateProjectRequest struct {
	ExpectedRevision int64                             `json:"expectedRevision"`
	Title            *string                           `json:"title,omitempty"`
	Purpose          *string                           `json:"purpose,omitempty"`
	Status           *runtime.ProjectStatus            `json:"status,omitempty"`
	AgentRefs        *[]runtime.ResourceReference      `json:"agentRefs,omitempty"`
	TeamRefs         *[]runtime.ResourceReference      `json:"teamRefs,omitempty"`
	ObjectiveRefs    *[]string                         `json:"objectiveRefs,omitempty"`
	Milestones       *[]runtime.ProjectMilestone       `json:"milestones,omitempty"`
	Hypotheses       *[]runtime.ProjectHypothesis      `json:"hypotheses,omitempty"`
	SourceMonitors   *[]runtime.SourceMonitorReference `json:"sourceMonitors,omitempty"`
	Deliverables     *[]runtime.ProjectDeliverable     `json:"deliverables,omitempty"`
	Budget           *runtime.BudgetPolicy             `json:"budget,omitempty"`
	ClearBudget      bool                              `json:"clearBudget,omitempty"`
	Policy           map[string]interface{}            `json:"policy,omitempty"`
	Checkpoint       map[string]interface{}            `json:"checkpoint,omitempty"`
}

type OutreachCapabilitySelection struct {
	BindingID       string                 `json:"bindingId"`
	BindingRevision int64                  `json:"bindingRevision"`
	SkillID         string                 `json:"skillId"`
	SkillVersion    string                 `json:"skillVersion"`
	Action          string                 `json:"action"`
	Arguments       map[string]interface{} `json:"arguments,omitempty"`
}

type CreateOutreachMessageRequest struct {
	ID         string                        `json:"id,omitempty"`
	Intent     runtime.OutreachMessageIntent `json:"intent"`
	Body       string                        `json:"body"`
	Capability OutreachCapabilitySelection   `json:"capability"`
}

type CreateOutreachThreadRequest struct {
	ID                  string                       `json:"id,omitempty"`
	Scope               runtime.Scope                `json:"scope"`
	ProjectID           string                       `json:"projectId"`
	SourceObservationID string                       `json:"sourceObservationId"`
	ApprovalPolicyRef   string                       `json:"approvalPolicyRef"`
	Identity            runtime.OutreachIdentity     `json:"identity"`
	Message             CreateOutreachMessageRequest `json:"message"`
	IdempotencyKey      string                       `json:"idempotencyKey,omitempty"`
}

type DeliverOutreachMessageRequest struct {
	Scope          runtime.Scope         `json:"scope"`
	Priority       int                   `json:"priority,omitempty"`
	Budget         *runtime.BudgetPolicy `json:"budget,omitempty"`
	IdempotencyKey string                `json:"idempotencyKey,omitempty"`
}

type AgentRunCommandRequest struct {
	ExpectedRevision    int64                       `json:"expectedRevision"`
	Kind                runtime.AgentRunCommandKind `json:"kind"`
	Actor               runtime.ActivityActor       `json:"actor,omitempty"`
	Summary             string                      `json:"summary,omitempty"`
	Instruction         string                      `json:"instruction,omitempty"`
	HumanInterventionID string                      `json:"humanInterventionId,omitempty"`
	Visibility          runtime.ActivityVisibility  `json:"visibility,omitempty"`
}

type ResolveArtifactContentRequest struct {
	Actor      runtime.ActivityActor `json:"actor"`
	Purpose    string                `json:"purpose"`
	TTLSeconds int64                 `json:"ttlSeconds"`
}

type CreateConversationRequest struct {
	ID             string                         `json:"id,omitempty"`
	Scope          runtime.Scope                  `json:"scope"`
	Owner          runtime.ObjectiveOwner         `json:"owner"`
	Title          string                         `json:"title"`
	Origin         *runtime.ConversationReference `json:"origin,omitempty"`
	IdempotencyKey string                         `json:"idempotencyKey,omitempty"`
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
	BroadcastToChannel  bool                              `json:"broadcastToChannel,omitempty"`
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
