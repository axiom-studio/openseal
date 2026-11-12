// Package kernelapi defines the versioned HTTP contract shared by the
// standalone OpenSeal daemon and thin clients such as the terminal UI.
package kernelapi

import (
	"time"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentbuilder"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/collaboration"
	"github.com/axiom-studio/openseal/pkg/runtime/routine"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
	"github.com/axiom-studio/openseal/pkg/workforce"
)

const (
	APIVersion                                 = "agent-kernel/v1"
	AgentRunsCapabilityID                      = "agent-runs"
	AgentRunsCapabilityVersion                 = "2"
	WorkspaceSearchCapabilityID                = "workspace-search"
	WorkspaceSearchCapabilityVersion           = "1"
	ObjectivesCapabilityID                     = "objectives"
	ObjectivesCapabilityVersion                = "1"
	RunbooksCapabilityID                       = "runbooks"
	RunbooksCapabilityVersion                  = "3"
	SkillActionsCapabilityID                   = "skill-actions"
	SkillActionsCapabilityVersion              = "1"
	SkillBindingsCapabilityID                  = "skill-bindings"
	SkillBindingsCapabilityVersion             = "3"
	AgentEmbedsCapabilityID                    = "agent-embeds"
	AgentEmbedsCapabilityVersion               = "1"
	ArtifactsCapabilityID                      = "artifacts"
	ArtifactsCapabilityVersion                 = "1"
	ChannelsCapabilityID                       = "channels"
	ChannelsCapabilityVersion                  = "4"
	TeamDefinitionsCapabilityID                = "team-definitions"
	TeamDefinitionsCapabilityVersion           = "5"
	WorkforceExecutionTargetsCapabilityID      = "workforce-execution-targets"
	WorkforceExecutionTargetsCapabilityVersion = "1"
	WorkforceBundlesCapabilityID               = "workforce-bundles"
	WorkforceBundlesCapabilityVersion          = "1"
	AgentDefinitionsCapabilityID               = "agent-definitions"
	AgentDefinitionsCapabilityVersion          = "8"
	AgentRequestsCapabilityID                  = "agent-requests"
	AgentRequestsCapabilityVersion             = "1"
	ActionApprovalsCapabilityID                = "action-approvals"
	ActionApprovalsCapabilityVersion           = "2"
	ActionCallsCapabilityID                    = "action-calls"
	ActionCallsCapabilityVersion               = "1"
	AgentTurnsCapabilityID                     = "agent-turns"
	AgentTurnsCapabilityVersion                = "2"
	ActivityCapabilityID                       = "activity"
	ActivityCapabilityVersion                  = "1"
	ConversationGatewaysCapabilityID           = "conversation-gateways"
	ConversationGatewaysCapabilityVersion      = "2"
	CallbacksCapabilityID                      = "callbacks"
	CallbacksCapabilityVersion                 = "1"
	SpacesCapabilityID                         = "spaces"
	SpacesCapabilityVersion                    = "1"
	AgentBuilderCapabilityID                   = "agent-builder"
	AgentBuilderCapabilityVersion              = "1"
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
	OperationParticipation        = "configure-participation"
	OperationApprovalMode         = "update-approval-mode"
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
	OperationProposeAmendment     = "propose-amendment"
	OperationListAmendments       = "list-amendments"
	OperationGetAmendment         = "get-amendment"
	OperationEvaluateAmendment    = "evaluate-amendment"
	OperationResolveAmendment     = "resolve-amendment"
	OperationActivateAmendment    = "activate-amendment"
	OperationRecoverParticipation = "recover-participation"
	OperationReconcile            = "reconcile"
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
	Runs                  bool
	ParticipationSettings bool
	Coordination          bool
	AutomaticCoordination bool
	Receipts              bool
	Changes               bool
	Streaming             bool
	// ApprovalMode advertises the governed per-conversation approval mode
	// update (PATCH .../conversations/{id}/approval-mode).
	ApprovalMode bool
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
	Revision                    int64                                        `json:"revision,omitempty"`
	CredentialBindings          []capability.CredentialBindingChoice         `json:"credentialBindings,omitempty"`
	BindingConfigurationFields  []capability.BindingConfigurationFieldChoice `json:"bindingConfigurationFields,omitempty"`
	BlockingRequirements        []CapabilityBlockingRequirement              `json:"blockingRequirements,omitempty"`
	ConversationGatewayAdapters []ConversationGatewayAdapterChoice           `json:"conversationGatewayAdapters,omitempty"`
	CallbackAdapters            []CallbackAdapterChoice                      `json:"callbackAdapters,omitempty"`
	// BuilderAgentID is the builder Agent's deployment id (agent-builder
	// capability): the home box opens a builder chat with it.
	BuilderAgentID string `json:"builderAgentId,omitempty"`
}

// AgentBuilderCapability advertises builder conversations with the builder
// Agent builderAgentID: start one, and list the viewer's drafts.
func AgentBuilderCapability(builderAgentID string) Capability {
	return Capability{
		ID: AgentBuilderCapabilityID, Version: AgentBuilderCapabilityVersion, Available: true,
		Operations: []string{OperationCreate, OperationList},
		Context:    &CapabilityContext{BuilderAgentID: builderAgentID},
	}
}

// StartAgentBuilderConversationRequest opens a builder conversation. The
// requester is the authenticated principal, never a body field. Prompt is the
// request as typed; name and model go only in Parameters.
type StartAgentBuilderConversationRequest struct {
	Scope          models.Scope                  `json:"scope"`
	Prompt         string                        `json:"prompt"`
	Parameters     models.AgentBuilderParameters `json:"parameters"`
	SpaceID        string                        `json:"spaceId,omitempty"`
	IdempotencyKey string                        `json:"idempotencyKey"`
}

type StartAgentBuilderConversationResponse struct {
	Conversation *models.Conversation   `json:"conversation"`
	Message      *models.ChannelMessage `json:"message"`
	Replayed     bool                   `json:"replayed"`
}

type ListAgentBuilderDraftsResponse struct {
	Drafts []agentbuilder.AgentBuilderDraft `json:"drafts"`
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

func WorkspaceSearchCapability() Capability {
	return Capability{
		ID: WorkspaceSearchCapabilityID, Version: WorkspaceSearchCapabilityVersion, Available: true,
		Operations: []string{OperationRead},
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

func SkillActionsCapability() Capability {
	return Capability{ID: SkillActionsCapabilityID, Version: SkillActionsCapabilityVersion, Available: true, Operations: []string{OperationList}}
}

func Capabilities() CapabilityDocument {
	return NewCapabilityDocument(ObjectivesCapability(), RunbooksCapability(), SkillActionsCapability(), SkillBindingsCapability(true), ActivityCapability(), AgentRunsCapability(), AgentTurnsCapability(), ActionCallsCapability(), AgentDefinitionsCapability(), ChannelsCapability(ChannelCapabilityFeatures{Coordination: true, Changes: true}), TeamDefinitionsCapability(TeamDefinitionCapabilityFeatures{}))
}

// ActivityCapability exposes the selector-bounded, redacted audit projection.
// Raw append authority is deliberately not part of the interactive contract.
func ActivityCapability() Capability {
	return Capability{ID: ActivityCapabilityID, Version: ActivityCapabilityVersion, Available: true, Operations: []string{OperationList}}
}

// SpacesCapability describes a tenant's spaces and their agent and team members.
func SpacesCapability() Capability {
	return Capability{
		ID: SpacesCapabilityID, Version: SpacesCapabilityVersion, Available: true,
		Operations: []string{OperationGet, OperationList, OperationCreate, OperationUpdate, OperationRetire},
	}
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

// AgentEmbedsCapability describes governed publication of an Agent as a
// capability-bound browser chat. Public session operations remain behind the
// opaque installation route and are not tenant management authorities.
func AgentEmbedsCapability(management bool) Capability {
	operations := []string{OperationGet, OperationList}
	if management {
		operations = append(operations, OperationCreate, OperationUpdate)
	}
	return Capability{ID: AgentEmbedsCapabilityID, Version: AgentEmbedsCapabilityVersion, Available: true, Operations: operations}
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
		Operations: []string{OperationCreate, OperationGet, OperationList, OperationUpdate, OperationPost, OperationRead, OperationPresence, OperationAudit},
	}
	if features.Coordination {
		capability.Operations = append(capability.Operations, OperationCoordinate)
	}
	if features.Runs {
		capability.Operations = append(capability.Operations, "runs")
	}
	if features.ParticipationSettings {
		capability.Operations = append(capability.Operations, OperationParticipation)
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
	if features.ApprovalMode {
		capability.Operations = append(capability.Operations, OperationApprovalMode)
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
	Scope                models.Scope              `json:"scope"`
	Kind                 models.RunKind            `json:"kind,omitempty"`
	ObjectiveID          string                    `json:"objectiveId,omitempty"`
	ParentRunID          string                    `json:"parentRunId,omitempty"`
	Owner                models.ObjectiveOwner     `json:"owner"`
	AssignedAgentID      string                    `json:"assignedAgentId,omitempty"`
	Entrypoint           string                    `json:"entrypoint,omitempty"`
	ConcurrencyKey       string                    `json:"concurrencyKey,omitempty"`
	ResourceRequirements map[string]int            `json:"resourceRequirements,omitempty"`
	Goal                 string                    `json:"goal"`
	Source               models.RunSource          `json:"source"`
	Priority             int                       `json:"priority,omitempty"`
	Deadline             *time.Time                `json:"deadline,omitempty"`
	AvailableAt          *time.Time                `json:"availableAt,omitempty"`
	Context              map[string]interface{}    `json:"context,omitempty"`
	Plan                 map[string]interface{}    `json:"plan,omitempty"`
	Checkpoint           map[string]interface{}    `json:"checkpoint,omitempty"`
	WakeCondition        *models.WakeCondition     `json:"wakeCondition,omitempty"`
	Budget               *models.BudgetPolicy      `json:"budget,omitempty"`
	Policy               map[string]interface{}    `json:"policy,omitempty"`
	IdempotencyKey       string                    `json:"idempotencyKey,omitempty"`
	Actor                models.ActivityActor      `json:"actor,omitempty"`
	Visibility           models.ActivityVisibility `json:"visibility,omitempty"`
}

// CreateAgentRequestRequest is the public request body for a durable request or
// handoff between Agents and Teams. Only explicitly shared, credential-free
// context crosses the collaboration boundary.
type CreateAgentRequestRequest struct {
	ID                   string                       `json:"id,omitempty"`
	Scope                models.Scope                 `json:"scope"`
	Kind                 models.AgentRequestKind      `json:"kind"`
	Requester            models.CollaborationParty    `json:"requester"`
	Recipient            models.CollaborationParty    `json:"recipient"`
	SourceRunID          string                       `json:"sourceRunId"`
	Goal                 string                       `json:"goal"`
	Instructions         string                       `json:"instructions,omitempty"`
	SemanticRole         string                       `json:"semanticRole,omitempty"`
	AcceptanceCriteria   map[string]interface{}       `json:"acceptanceCriteria,omitempty"`
	ArtifactRequirements []models.ArtifactRequirement `json:"artifactRequirements,omitempty"`
	SharedContext        map[string]interface{}       `json:"sharedContext,omitempty"`
	ChildCheckpoint      map[string]interface{}       `json:"childCheckpoint,omitempty"`
	ConversationRefs     []string                     `json:"conversationRefs,omitempty"`
	BudgetAllocation     *models.BudgetPolicy         `json:"budgetAllocation,omitempty"`
	IdempotencyKey       string                       `json:"idempotencyKey,omitempty"`
}

type RespondAgentRequestRequest struct {
	ExpectedRevision int64                              `json:"expectedRevision"`
	Decision         collaboration.AgentRequestDecision `json:"decision"`
	Principal        models.CollaborationParty          `json:"principal"`
	Actor            models.CollaborationParty          `json:"actor,omitempty"`
	AssignedAgentID  string                             `json:"assignedAgentId,omitempty"`
	Message          string                             `json:"message,omitempty"`
	DecisionRunID    string                             `json:"decisionRunId,omitempty"`
}

type CompleteAgentRequestRequest struct {
	ExpectedRevision      int64                      `json:"expectedRevision"`
	ExpectedChildRevision int64                      `json:"expectedChildRevision"`
	Principal             models.CollaborationParty  `json:"principal"`
	Actor                 models.CollaborationParty  `json:"actor,omitempty"`
	Summary               string                     `json:"summary"`
	AcceptanceEvidence    map[string]interface{}     `json:"acceptanceEvidence,omitempty"`
	Artifacts             []models.ArtifactReference `json:"artifacts,omitempty"`
	IdempotencyKey        string                     `json:"idempotencyKey,omitempty"`
}

type ReviewAgentRequestCompletionRequest struct {
	ExpectedRevision      int64                     `json:"expectedRevision"`
	ExpectedChildRevision int64                     `json:"expectedChildRevision"`
	Principal             models.CollaborationParty `json:"principal"`
	Actor                 models.CollaborationParty `json:"actor,omitempty"`
	Approve               bool                      `json:"approve"`
	Summary               string                    `json:"summary"`
	IdempotencyKey        string                    `json:"idempotencyKey,omitempty"`
}

type ResolveActionApprovalRequest struct {
	ExpectedRevision int64                    `json:"expectedRevision"`
	DecisionID       string                   `json:"decisionId,omitempty"`
	Decision         models.ApprovalDecision  `json:"decision"`
	Principal        models.ApprovalPrincipal `json:"principal"`
	Reason           string                   `json:"reason,omitempty"`
}

type CreateObjectiveRequest struct {
	Scope           models.Scope           `json:"scope"`
	Owner           models.ObjectiveOwner  `json:"owner"`
	Title           string                 `json:"title"`
	Goal            string                 `json:"goal"`
	Status          models.ObjectiveStatus `json:"status,omitempty"`
	Priority        int                    `json:"priority,omitempty"`
	Budget          *models.BudgetPolicy   `json:"budget,omitempty"`
	Constraints     map[string]interface{} `json:"constraints,omitempty"`
	SuccessCriteria map[string]interface{} `json:"successCriteria,omitempty"`
	IdempotencyKey  string                 `json:"idempotencyKey,omitempty"`
}

type UpdateObjectiveRequest struct {
	ExpectedRevision int64                   `json:"expectedRevision"`
	Title            *string                 `json:"title,omitempty"`
	Goal             *string                 `json:"goal,omitempty"`
	Status           *models.ObjectiveStatus `json:"status,omitempty"`
	Priority         *int                    `json:"priority,omitempty"`
	Budget           *models.BudgetPolicy    `json:"budget,omitempty"`
	Constraints      map[string]interface{}  `json:"constraints,omitempty"`
	SuccessCriteria  map[string]interface{}  `json:"successCriteria,omitempty"`
	ProgressSummary  *string                 `json:"progressSummary,omitempty"`
}

type ObjectiveDetail struct {
	Objective *models.Objective           `json:"objective"`
	Runbooks  []*models.RunbookActivation `json:"runbooks"`
	Runs      []*models.AgentRun          `json:"runs"`
}

type ReconcileRunbookSchedulesRequest struct {
	Scope models.Scope `json:"scope"`
	Limit int          `json:"limit,omitempty"`
}

type RunbookScheduleReconciliation struct {
	Scope        models.Scope                   `json:"scope"`
	ReconciledAt time.Time                      `json:"reconciledAt"`
	Result       *routine.RunbookScheduleResult `json:"result"`
}

type CreateExternalConversationGatewayRequest struct {
	ID      string                                    `json:"id,omitempty"`
	Name    string                                    `json:"name"`
	Gateway models.ExternalConversationIngressGateway `json:"gateway"`
	Status  models.ExternalConversationGatewayStatus  `json:"status,omitempty"`
	Reason  string                                    `json:"reason"`
}

type UpdateExternalConversationGatewayRequest struct {
	ExpectedRevision int64                                      `json:"expectedRevision"`
	Name             *string                                    `json:"name,omitempty"`
	Gateway          *models.ExternalConversationIngressGateway `json:"gateway,omitempty"`
	Status           *models.ExternalConversationGatewayStatus  `json:"status,omitempty"`
	Reason           string                                     `json:"reason"`
}

type AgentRunCommandRequest struct {
	ExpectedRevision    int64                        `json:"expectedRevision"`
	Kind                agentrun.AgentRunCommandKind `json:"kind"`
	Actor               models.ActivityActor         `json:"actor,omitempty"`
	Summary             string                       `json:"summary,omitempty"`
	Instruction         string                       `json:"instruction,omitempty"`
	InterventionID      string                       `json:"interventionId,omitempty"`
	HumanInterventionID string                       `json:"humanInterventionId,omitempty"`
	Visibility          models.ActivityVisibility    `json:"visibility,omitempty"`
}

type ResolveArtifactContentRequest struct {
	Actor      models.ActivityActor `json:"actor"`
	Purpose    string               `json:"purpose"`
	TTLSeconds int64                `json:"ttlSeconds"`
}

type CreateConversationRequest struct {
	ID             string                        `json:"id,omitempty"`
	Scope          models.Scope                  `json:"scope"`
	Owner          models.ObjectiveOwner         `json:"owner"`
	Title          string                        `json:"title"`
	Origin         *models.ConversationReference `json:"origin,omitempty"`
	SpaceID        string                        `json:"spaceId,omitempty"`
	IdempotencyKey string                        `json:"idempotencyKey,omitempty"`
}

type UpdateConversationRequest struct {
	ParticipationEnabled *bool                      `json:"participationEnabled,omitempty"`
	Scope                models.Scope               `json:"scope"`
	ExpectedRevision     int64                      `json:"expectedRevision"`
	Title                *string                    `json:"title,omitempty"`
	Status               *models.ConversationStatus `json:"status,omitempty"`
}

// SetConversationApprovalModeRequest changes a conversation's approval mode.
// Mode is manual, auto, or skip; ExpectedRevision is the conversation revision
// the caller last read. Actor is the authorized principal making the change.
type SetConversationApprovalModeRequest struct {
	Scope            models.Scope                    `json:"scope"`
	ExpectedRevision int64                           `json:"expectedRevision"`
	Mode             models.ConversationApprovalMode `json:"mode"`
	Actor            models.ActivityActor            `json:"actor"`
}

type PostChannelMessageRequest struct {
	ID                  string                           `json:"id,omitempty"`
	Scope               models.Scope                     `json:"scope"`
	ExpectedRevision    int64                            `json:"expectedRevision"`
	Sender              models.ConversationParticipant   `json:"sender"`
	Intent              models.ConversationMessageIntent `json:"intent"`
	Content             string                           `json:"content"`
	Audience            models.ConversationAudience      `json:"audience"`
	ReplyToMessageID    string                           `json:"replyToMessageId,omitempty"`
	QuotedMessageID     string                           `json:"quotedMessageId,omitempty"`
	BroadcastToChannel  bool                             `json:"broadcastToChannel,omitempty"`
	Mentions            []models.ConversationParticipant `json:"mentions,omitempty"`
	References          []models.ConversationReference   `json:"references,omitempty"`
	RequiresResponse    bool                             `json:"requiresResponse,omitempty"`
	ResolvesMessageID   string                           `json:"resolvesMessageId,omitempty"`
	SupersedesMessageID string                           `json:"supersedesMessageId,omitempty"`
	IdempotencyKey      string                           `json:"idempotencyKey,omitempty"`
}

type AdvanceConversationCursorRequest struct {
	Scope             models.Scope                   `json:"scope"`
	Participant       models.ConversationParticipant `json:"participant"`
	ExpectedRevision  int64                          `json:"expectedRevision,omitempty"`
	DeliveredSequence int64                          `json:"deliveredSequence"`
	ReadSequence      int64                          `json:"readSequence"`
}

type SetConversationPresenceRequest struct {
	Scope            models.Scope                     `json:"scope"`
	Participant      models.ConversationParticipant   `json:"participant"`
	State            models.ConversationPresenceState `json:"state"`
	Summary          string                           `json:"summary,omitempty"`
	RunID            string                           `json:"runId,omitempty"`
	LeaseID          string                           `json:"leaseId,omitempty"`
	ExpectedRevision int64                            `json:"expectedRevision,omitempty"`
	TTLSeconds       int64                            `json:"ttlSeconds,omitempty"`
}

// MessageReactionRequest adds (PUT) or removes (DELETE) the actor's emoji on
// a channel message at /api/v1/conversations/{id}/reactions. The host
// authenticates the actor; roles are the actor's audience roles.
type MessageReactionRequest struct {
	Scope     models.Scope                   `json:"scope"`
	Actor     models.ConversationParticipant `json:"actor"`
	Roles     []string                       `json:"roles,omitempty"`
	MessageID string                         `json:"messageId"`
	Emoji     string                         `json:"emoji"`
}

type ReleaseConversationPresenceRequest struct {
	Scope       models.Scope                   `json:"scope"`
	Participant models.ConversationParticipant `json:"participant"`
	LeaseID     string                         `json:"leaseId"`
}
