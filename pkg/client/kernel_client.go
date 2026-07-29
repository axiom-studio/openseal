package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/authoring"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/kernelapi"
	"github.com/axiom-studio/openseal/pkg/runtime"
	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/axiom-studio/openseal/pkg/skill/clawhub"
	"github.com/axiom-studio/openseal/pkg/source"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
	"github.com/axiom-studio/openseal/pkg/workforce"
)

const DefaultKernelBaseURL = "http://127.0.0.1:8080"

// KernelClient is the thin HTTP boundary used by OpenSeal interactive
// surfaces. It deliberately exposes only versioned public kernel operations.
type KernelClient interface {
	ArtifactClient
	TeamClient
	Capabilities(context.Context) (kernelapi.CapabilityDocument, error)
	WorkforceChangeSetCapabilities(context.Context, capability.ScopeReference, string) (kernelapi.CapabilityDocument, error)
	CreateObjective(context.Context, kernelapi.CreateObjectiveRequest, string) (*runtime.Objective, error)
	ListObjectives(context.Context, runtime.ObjectiveFilter) ([]*runtime.Objective, error)
	GetObjective(context.Context, runtime.Scope, string) (*kernelapi.ObjectiveDetail, error)
	UpdateObjective(context.Context, runtime.Scope, string, kernelapi.UpdateObjectiveRequest) (*runtime.Objective, error)
	ListRunbooks(context.Context, runtime.RunbookActivationFilter) ([]*runtime.RunbookActivation, error)
	GetRunbook(context.Context, runtime.Scope, string) (*runtime.RunbookDetail, error)
	UpdateRunbook(context.Context, runtime.Scope, string, runtime.UpdateRunbookActivationRequest) (*runtime.RunbookActivation, error)
	StartRunbook(context.Context, runtime.Scope, string, runtime.StartRunbookActivationRequest) (*runtime.AgentRunCommandResult, error)
	ReconcileRunbookSchedules(context.Context, kernelapi.ReconcileRunbookSchedulesRequest) (*kernelapi.RunbookScheduleReconciliation, error)
	CreateEventSourceSubscription(context.Context, kernelapi.CreateEventSourceSubscriptionRequest) (*runtime.EventSourceSubscription, error)
	ListEventSourceSubscriptions(context.Context, runtime.EventSourceSubscriptionFilter) ([]*runtime.EventSourceSubscription, error)
	GetEventSourceSubscription(context.Context, runtime.Scope, string) (*runtime.EventSourceSubscriptionDetail, error)
	UpdateEventSourceSubscription(context.Context, runtime.Scope, string, kernelapi.UpdateEventSourceSubscriptionRequest) (*runtime.EventSourceSubscription, error)
	RetireEventSourceSubscription(context.Context, runtime.Scope, string, kernelapi.RetireEventSourceSubscriptionRequest) (*runtime.EventSourceSubscription, error)
	ReportEventSourceHealth(context.Context, runtime.Scope, string, kernelapi.ReportEventSourceHealthRequest) (*runtime.EventSourceHealth, error)
	GetEventSourceCheckpoint(context.Context, runtime.Scope, string) (*runtime.EventSourceCheckpoint, error)
	AdvanceEventSourceCheckpoint(context.Context, runtime.Scope, string, kernelapi.AdvanceEventSourceCheckpointRequest) (*runtime.EventSourceCheckpoint, error)
	RouteEvent(context.Context, runtime.EventEnvelope) (*runtime.EventRouteResult, error)
	CreateProject(context.Context, kernelapi.CreateProjectRequest, string) (*runtime.Project, error)
	ListProjects(context.Context, runtime.ProjectFilter) ([]*runtime.Project, error)
	GetProject(context.Context, runtime.Scope, string) (*runtime.Project, error)
	PatchProject(context.Context, runtime.Scope, string, kernelapi.UpdateProjectRequest) (*runtime.Project, error)
	CreateOutreachThread(context.Context, kernelapi.CreateOutreachThreadRequest, string) (*runtime.OutreachThread, error)
	ListOutreachThreads(context.Context, runtime.OutreachThreadFilter) ([]*runtime.OutreachThread, error)
	GetOutreachThread(context.Context, runtime.Scope, string, string) (*runtime.OutreachThread, error)
	DeliverOutreachMessage(context.Context, string, string, string, kernelapi.DeliverOutreachMessageRequest, string) (*runtime.AgentRun, error)
	ListActivity(context.Context, runtime.ActivityFeedRequest) (*runtime.ActivityFeedPage, error)
	ListSourceObservations(context.Context, runtime.SourceObservationFilter) ([]*runtime.SourceObservation, error)
	GetSourceMonitorCheckpoint(context.Context, runtime.Scope, string, string) (*runtime.SourceMonitorCheckpoint, error)
	CreateAgentRun(context.Context, kernelapi.CreateAgentRunRequest, string) (*runtime.AgentRunCommandResult, error)
	ListAgentRuns(context.Context, runtime.AgentRunFilter) ([]*runtime.AgentRun, error)
	GetAgentRun(context.Context, runtime.Scope, string) (*runtime.AgentRun, error)
	CommandAgentRun(context.Context, runtime.Scope, string, kernelapi.AgentRunCommandRequest) (*runtime.AgentRunCommandResult, error)
	ListAgentTurns(context.Context, runtime.AgentTurnFilter) ([]*kernelapi.AgentTurnRecord, error)
	GetAgentTurn(context.Context, runtime.Scope, string) (*kernelapi.AgentTurnRecord, error)
	CreateAgentRequest(context.Context, kernelapi.CreateAgentRequestRequest, string) (*runtime.AgentRequestResult, error)
	ListAgentRequests(context.Context, runtime.AgentRequestFilter) ([]*runtime.AgentRequest, error)
	GetAgentRequest(context.Context, runtime.Scope, string) (*runtime.AgentRequest, error)
	RespondAgentRequest(context.Context, runtime.Scope, string, kernelapi.RespondAgentRequestRequest) (*runtime.AgentRequestResult, error)
	CompleteAgentRequest(context.Context, runtime.Scope, string, kernelapi.CompleteAgentRequestRequest, string) (*runtime.AgentRequestResult, error)
	ListActionCalls(context.Context, runtime.ActionFilter) ([]*runtime.ActionCall, error)
	GetActionCall(context.Context, runtime.Scope, string) (*runtime.ActionCall, error)
	ListActionApprovals(context.Context, runtime.ApprovalFilter) ([]*runtime.ApprovalCheckpoint, error)
	GetActionApproval(context.Context, runtime.Scope, string) (*runtime.ApprovalCheckpoint, error)
	ResolveActionApproval(context.Context, runtime.Scope, string, kernelapi.ResolveActionApprovalRequest, string) (*runtime.ApprovalResolutionResult, error)
	ListAgentDefinitionCompilations(context.Context, capability.ScopeReference, string) ([]*kernelagent.DefinitionCompilation, error)
	InstallAgentManifest(context.Context, kernelagent.ManifestInstallationRequest, string) (*kernelagent.ManifestInstallationResult, error)
	GetAgentDeployment(context.Context, capability.ScopeReference, string) (*kernelapi.AgentDeploymentCatalogEntry, error)
	ListAgentDeployments(context.Context, capability.ScopeReference) (*kernelapi.AgentDeploymentList, error)
	UpdateAgentDeployment(context.Context, string, kernelapi.UpdateAgentDeploymentRequest) (*kernelapi.AgentDeploymentUpdateResult, error)
	ListAgentSkillActions(context.Context, capability.ScopeReference, string, []string, capability.SideEffect) (*kernelapi.SkillActionList, error)
	CompileWorkforce(context.Context, authoring.GenerateRequest) (*authoring.CompileResult, error)
	CreateWorkforceChangeSet(context.Context, authoring.CreateChangeSetRequest, string) (*authoring.ChangeSet, error)
	GetWorkforceChangeSet(context.Context, capability.ScopeReference, string) (*authoring.ChangeSet, error)
	AnswerWorkforceChangeSetRefinement(context.Context, authoring.AnswerChangeSetRefinementRequest, string) (*authoring.ChangeSet, error)
	UpdateWorkforceChangeSetPlacement(context.Context, authoring.UpdateChangeSetPlacementRequest, string) (*authoring.ChangeSet, error)
	RetryWorkforceChangeSetGeneration(context.Context, authoring.RetryChangeSetGenerationRequest, string) (*authoring.ChangeSet, error)
	EvaluateWorkforceChangeSet(context.Context, authoring.SubmitChangeSetEvaluationRequest, string) (*authoring.ChangeSet, error)
	ResolveWorkforceChangeSetApproval(context.Context, authoring.ResolveChangeSetApprovalRequest, string) (*authoring.ChangeSet, error)
	PrepareWorkforceChangeSetActivation(context.Context, authoring.PrepareChangeSetActivationRequest, string) (*authoring.ChangeSet, error)
	ApplyWorkforceChangeSet(context.Context, authoring.ApplyChangeSetRequest, string) (*authoring.ChangeSet, error)
}

// RunbookExecutionAuditClient exposes the optional Runbook-first audit read.
// Interactive surfaces capability-gate it with agent-runs.audit so clients
// connected to an older or deliberately minimal kernel remain valid.
type RunbookExecutionAuditClient interface {
	GetRunbookExecutionAudit(context.Context, runtime.Scope, string) (*runtime.RunbookExecutionAudit, error)
}

var _ RunbookExecutionAuditClient = (*KernelHTTPClient)(nil)

// ExternalConversationGatewayClient is the narrow lifecycle surface used by
// hosts that advertise conversation-gateways/v2. Keeping it separate from the
// base KernelClient lets older or read-only embedding surfaces remain honest.
type ExternalConversationGatewayClient interface {
	CreateExternalConversationGateway(context.Context, kernelapi.CreateExternalConversationGatewayRequest) (*runtime.ExternalConversationGatewayRegistration, error)
	ListExternalConversationGateways(context.Context, runtime.ExternalConversationGatewayFilter) ([]*runtime.ExternalConversationGatewayRegistration, error)
	GetExternalConversationGateway(context.Context, runtime.Scope, string) (*runtime.ExternalConversationGatewayRegistration, error)
	UpdateExternalConversationGateway(context.Context, runtime.Scope, string, kernelapi.UpdateExternalConversationGatewayRequest) (*runtime.ExternalConversationGatewayRegistration, error)
}

// AgentDefinitionCapabilityClient discovers the operations authorized for one
// exact Agent deployment. Hosts may expose read-only Agent definition
// capabilities globally and add governed mutations only after applying the
// deployment's scope, revision, and caller policy.
type AgentDefinitionCapabilityClient interface {
	AgentDefinitionCapabilities(context.Context, capability.ScopeReference, string) (kernelapi.CapabilityDocument, error)
}

type ClawHubClient interface {
	InspectClawHubSkill(context.Context, clawhub.SkillReference) (*clawhub.SkillDetail, error)
	ListClawHubSkillVersions(context.Context, clawhub.SkillReference, int, string) (*clawhub.VersionPage, error)
	VerifyClawHubSkill(context.Context, clawhub.SkillReference, kernelapi.ClawHubVersionRequest) (*clawhub.Verification, error)
	ListInstalledClawHubSkills(context.Context) ([]clawhub.InstalledState, error)
	InstallClawHubSkill(context.Context, clawhub.SkillReference, kernelapi.ClawHubVersionRequest) (*clawhub.LifecycleResult, error)
	VerifyInstalledClawHubSkill(context.Context, string) (*clawhub.Verification, error)
	PinClawHubSkill(context.Context, string, string) (*clawhub.LifecycleResult, error)
	UnpinClawHubSkill(context.Context, string) (*clawhub.LifecycleResult, error)
	UpdateClawHubSkill(context.Context, string) (*clawhub.LifecycleResult, error)
	UpdateAllClawHubSkills(context.Context) (*clawhub.LifecycleBatchResult, error)
	UninstallClawHubSkill(context.Context, string) (*clawhub.LifecycleResult, error)
}

// WorkforceRefinementClient lets narrower embedding surfaces depend only on
// the governed refinement operation. Interactive clients still capability-gate
// the operation against the contextual workforce-authoring v7 document.
type WorkforceRefinementClient interface {
	AnswerWorkforceChangeSetRefinement(context.Context, authoring.AnswerChangeSetRefinementRequest, string) (*authoring.ChangeSet, error)
}

// WorkforceSkillSearchClient is the narrow discovery boundary used by
// prompt-first authoring surfaces.
type WorkforceSkillSearchClient interface {
	SearchWorkforceSkills(context.Context, authoring.SkillSearchRequest) (*authoring.SkillSearchPage, error)
}

var _ WorkforceRefinementClient = (*KernelHTTPClient)(nil)
var _ WorkforceSkillSearchClient = (*KernelHTTPClient)(nil)

// AgentDefinitionLifecycleClient is the narrow public boundary for governed
// immutable definition rollout, rollback, and amendment review. Read-only
// Agent browsers can keep depending on KernelClient without acquiring these
// mutation methods.
type AgentDefinitionLifecycleClient interface {
	ActivateAgentDefinition(context.Context, string, kernelapi.ActivateAgentDefinitionRequest) (*kernelapi.AgentDefinitionActivationResult, error)
	RollbackAgentDefinition(context.Context, string, kernelapi.RollbackAgentDefinitionRequest) (*kernelapi.AgentDefinitionActivationResult, error)
	ListAgentDefinitionActivations(context.Context, capability.ScopeReference, string) ([]workforce.DefinitionActivation, error)
	ProposeAgentDefinitionAmendment(context.Context, kernelagent.ProposeAmendmentRequest) (*kernelagent.DefinitionAmendment, error)
	GetAgentDefinitionAmendment(context.Context, capability.ScopeReference, string, string) (*kernelagent.DefinitionAmendment, error)
	ListAgentDefinitionAmendments(context.Context, capability.ScopeReference, string) (*kernelapi.AgentDefinitionAmendmentList, error)
	SubmitAgentDefinitionAmendmentEvaluation(context.Context, string, kernelagent.SubmitAmendmentEvaluationRequest) (*kernelagent.DefinitionAmendment, error)
	ResolveAgentDefinitionAmendment(context.Context, string, kernelagent.ResolveAmendmentRequest) (*kernelagent.DefinitionAmendment, error)
	ActivateAgentDefinitionAmendment(context.Context, string, string, kernelapi.ActivateAgentDefinitionAmendmentRequest) (*kernelapi.AgentDefinitionAmendmentActivationResult, error)
}

var _ AgentDefinitionLifecycleClient = (*KernelHTTPClient)(nil)

// SourcePolicyLifecycleClient is the narrow credential-free governance
// boundary. Hosts may wrap it with tenant RBAC without exposing other kernel
// mutation APIs.
type SourcePolicyLifecycleClient interface {
	RegisterSourcePolicyVersion(context.Context, source.RegisterVersionRequest) (*source.PolicyVersion, error)
	GetSourcePolicy(context.Context, capability.ScopeReference, string) (*source.LifecycleDetail, error)
	ListSourcePolicies(context.Context, capability.ScopeReference) (*source.LifecycleList, error)
	GetSourcePolicyVersion(context.Context, capability.ScopeReference, string, string) (*source.PolicyVersion, error)
	ListSourcePolicyVersions(context.Context, capability.ScopeReference, string) (*source.PolicyVersionList, error)
	ActivateSourcePolicy(context.Context, string, source.ActivateRequest) (*source.LifecycleResult, error)
	RevokeSourcePolicy(context.Context, string, source.RevokeRequest) (*source.LifecycleResult, error)
	ListSourcePolicyActivations(context.Context, capability.ScopeReference, string) (*source.LifecycleEventList, error)
}

var _ SourcePolicyLifecycleClient = (*KernelHTTPClient)(nil)

type TeamClient interface {
	RegisterTeamDefinition(context.Context, *kernelteam.Definition) (*kernelteam.Definition, error)
	GetTeamDefinition(context.Context, string, string) (*kernelteam.Definition, error)
	ListTeamDefinitionVersions(context.Context, string) ([]*kernelteam.Definition, error)
	CreateTeamDeployment(context.Context, kernelapi.CreateTeamDeploymentRequest) (*kernelapi.TeamDeploymentResult, error)
	GetTeamDeployment(context.Context, capability.ScopeReference, string) (*kernelteam.Deployment, error)
	ListTeamDeployments(context.Context, capability.ScopeReference) (*kernelapi.TeamDeploymentList, error)
	UpdateTeamDeployment(context.Context, string, kernelapi.UpdateTeamDeploymentRequest) (*kernelapi.TeamDeploymentResult, error)
	ActivateTeamDefinition(context.Context, string, kernelapi.ActivateTeamDefinitionRequest) (*kernelapi.TeamDeploymentResult, error)
	ListTeamDefinitionActivations(context.Context, capability.ScopeReference, string) ([]workforce.DefinitionActivation, error)
	ProposeTeamDefinitionAmendment(context.Context, kernelteam.ProposeAmendmentRequest) (*kernelteam.DefinitionAmendment, error)
	GetTeamDefinitionAmendment(context.Context, capability.ScopeReference, string, string) (*kernelteam.DefinitionAmendment, error)
	ListTeamDefinitionAmendments(context.Context, capability.ScopeReference, string) (*kernelapi.TeamDefinitionAmendmentList, error)
	SubmitTeamDefinitionAmendmentEvaluation(context.Context, string, kernelteam.SubmitAmendmentEvaluationRequest) (*kernelteam.DefinitionAmendment, error)
	ResolveTeamDefinitionAmendment(context.Context, string, kernelteam.ResolveAmendmentRequest) (*kernelteam.DefinitionAmendment, error)
	ActivateTeamDefinitionAmendment(context.Context, string, string, kernelapi.ActivateTeamDefinitionAmendmentRequest) (*kernelapi.TeamDefinitionAmendmentActivationResult, error)
}

// TeamSkillClient is the narrow public boundary for first-class Team-owned
// Skill portfolios. It stays separate from TeamClient so read-only Team
// browsers do not accidentally acquire mutation authority.
type TeamSkillClient interface {
	ListTeamSkillBindings(context.Context, capability.ScopeReference, string) (*kernelapi.SkillBindingList, error)
	UpsertTeamSkillBinding(context.Context, string, skill.UpsertBindingRequest) (*kernelapi.SkillBindingMutationResult, error)
	DisableTeamSkillBinding(context.Context, string, skill.DisableBindingRequest) (*kernelapi.SkillBindingMutationResult, error)
	PlanTeamSkillReferenceUpgrade(context.Context, string, runtime.PlanSkillReferenceUpgradeRequest) (*runtime.SkillReferenceUpgradePlan, error)
	ApplyTeamSkillReferenceUpgrade(context.Context, string, runtime.ApplySkillReferenceUpgradeRequest) (*runtime.SkillReferenceUpgradeReceipt, error)
}

// SkillBindingOwner identifies the canonical deployment resource that owns a
// Skill portfolio. The binding representation is identical for both kinds.
type SkillBindingOwner struct {
	Type         runtime.OwnerType
	DeploymentID string
}

// SkillBindingClient is the owner-neutral management boundary used by prompt-
// first clients. Hosts may implement it without exposing unrelated APIs.
type SkillBindingClient interface {
	ListSkillBindings(context.Context, capability.ScopeReference, SkillBindingOwner) (*kernelapi.SkillBindingList, error)
	UpsertSkillBinding(context.Context, SkillBindingOwner, skill.UpsertBindingRequest) (*kernelapi.SkillBindingMutationResult, error)
	DisableSkillBinding(context.Context, SkillBindingOwner, skill.DisableBindingRequest) (*kernelapi.SkillBindingMutationResult, error)
	PlanSkillReferenceUpgrade(context.Context, SkillBindingOwner, runtime.PlanSkillReferenceUpgradeRequest) (*runtime.SkillReferenceUpgradePlan, error)
	ApplySkillReferenceUpgrade(context.Context, SkillBindingOwner, runtime.ApplySkillReferenceUpgradeRequest) (*runtime.SkillReferenceUpgradeReceipt, error)
}

var _ TeamSkillClient = (*KernelHTTPClient)(nil)
var _ SkillBindingClient = (*KernelHTTPClient)(nil)

type KernelHTTPClient struct {
	baseURL        string
	apiRoot        string
	requestHeaders http.Header
	httpClient     *http.Client
}

// KernelHTTPClientOption configures transport concerns without changing the
// canonical kernel protocol. Hosts may use request headers for an authorized
// scope selector or other non-secret routing metadata.
type KernelHTTPClientOption func(*KernelHTTPClient)

// WithRequestHeaders adds headers to every kernel request. The values are
// copied at construction time so callers may safely reuse or mutate input.
// Protocol-owned Content-Type and Idempotency-Key headers cannot be replaced.
func WithRequestHeaders(headers http.Header) KernelHTTPClientOption {
	cloned := headers.Clone()
	return func(client *KernelHTTPClient) {
		for name, values := range cloned {
			canonical := http.CanonicalHeaderKey(strings.TrimSpace(name))
			if canonical == "" || canonical == "Content-Type" || canonical == "Idempotency-Key" {
				continue
			}
			for _, value := range values {
				client.requestHeaders.Add(canonical, value)
			}
		}
	}
}

type APIError struct {
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	if e == nil {
		return "OpenSeal API request failed"
	}
	if e.Message == "" {
		return fmt.Sprintf("OpenSeal API returned HTTP %d", e.StatusCode)
	}
	return e.Message
}

func NewKernelHTTPClient(baseURL string, httpClient *http.Client, options ...KernelHTTPClientOption) *KernelHTTPClient {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = DefaultKernelBaseURL
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	apiRoot := baseURL + "/api/v1"
	if parsed, err := url.Parse(baseURL); err == nil && strings.Trim(parsed.Path, "/") != "" {
		// A URL with a path is an explicit canonical API root. This lets a
		// product-neutral client operate through a host-mounted kernel without
		// teaching OpenSeal any host-specific route names.
		apiRoot = baseURL
	}
	client := &KernelHTTPClient{baseURL: baseURL, apiRoot: apiRoot, requestHeaders: make(http.Header), httpClient: httpClient}
	for _, option := range options {
		if option != nil {
			option(client)
		}
	}
	return client
}

func (c *KernelHTTPClient) Capabilities(ctx context.Context) (kernelapi.CapabilityDocument, error) {
	var document kernelapi.CapabilityDocument
	err := c.do(ctx, http.MethodGet, "/api/v1/capabilities", nil, "", &document)
	return document, err
}

func (c *KernelHTTPClient) AgentDefinitionCapabilities(ctx context.Context, scope capability.ScopeReference, deploymentID string) (kernelapi.CapabilityDocument, error) {
	query := capabilityScopeQuery(scope)
	query.Set("deploymentId", strings.TrimSpace(deploymentID))
	var document kernelapi.CapabilityDocument
	err := c.do(ctx, http.MethodGet, "/api/v1/capabilities?"+query.Encode(), nil, "", &document)
	return document, err
}

func (c *KernelHTTPClient) RegisterSourcePolicyVersion(ctx context.Context, request source.RegisterVersionRequest) (*source.PolicyVersion, error) {
	var result source.PolicyVersionResult
	if err := c.do(ctx, http.MethodPost, "/api/v1/source-policies/versions", request, "", &result); err != nil {
		return nil, err
	}
	if err := requireSourcePolicyVersion(result.APIVersion); err != nil {
		return nil, err
	}
	if result.Version == nil {
		return nil, errors.New("source policy registration returned no policy")
	}
	return result.Version, nil
}

func (c *KernelHTTPClient) GetSourcePolicy(ctx context.Context, scope capability.ScopeReference, policyID string) (*source.LifecycleDetail, error) {
	var result source.LifecycleDetail
	path := "/api/v1/source-policies/" + url.PathEscape(strings.TrimSpace(policyID)) + "?" + capabilityScopeQuery(scope).Encode()
	if err := c.do(ctx, http.MethodGet, path, nil, "", &result); err != nil {
		return nil, err
	}
	if err := requireSourcePolicyVersion(result.APIVersion); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *KernelHTTPClient) ListSourcePolicies(ctx context.Context, scope capability.ScopeReference) (*source.LifecycleList, error) {
	var result source.LifecycleList
	if err := c.do(ctx, http.MethodGet, "/api/v1/source-policies?"+capabilityScopeQuery(scope).Encode(), nil, "", &result); err != nil {
		return nil, err
	}
	if err := requireSourcePolicyVersion(result.APIVersion); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *KernelHTTPClient) GetSourcePolicyVersion(ctx context.Context, scope capability.ScopeReference, policyID, version string) (*source.PolicyVersion, error) {
	var result source.PolicyVersionResult
	path := "/api/v1/source-policies/" + url.PathEscape(strings.TrimSpace(policyID)) + "/versions/" + url.PathEscape(strings.TrimSpace(version)) + "?" + capabilityScopeQuery(scope).Encode()
	if err := c.do(ctx, http.MethodGet, path, nil, "", &result); err != nil {
		return nil, err
	}
	if err := requireSourcePolicyVersion(result.APIVersion); err != nil {
		return nil, err
	}
	if result.Version == nil {
		return nil, errors.New("source policy version response returned no policy")
	}
	return result.Version, nil
}

func (c *KernelHTTPClient) ListSourcePolicyVersions(ctx context.Context, scope capability.ScopeReference, policyID string) (*source.PolicyVersionList, error) {
	var result source.PolicyVersionList
	path := "/api/v1/source-policies/" + url.PathEscape(strings.TrimSpace(policyID)) + "/versions?" + capabilityScopeQuery(scope).Encode()
	if err := c.do(ctx, http.MethodGet, path, nil, "", &result); err != nil {
		return nil, err
	}
	if err := requireSourcePolicyVersion(result.APIVersion); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *KernelHTTPClient) ActivateSourcePolicy(ctx context.Context, policyID string, request source.ActivateRequest) (*source.LifecycleResult, error) {
	var result source.LifecycleResult
	path := "/api/v1/source-policies/" + url.PathEscape(strings.TrimSpace(policyID)) + "/activations"
	if err := c.do(ctx, http.MethodPost, path, request, "", &result); err != nil {
		return nil, err
	}
	if err := requireSourcePolicyVersion(result.APIVersion); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *KernelHTTPClient) RevokeSourcePolicy(ctx context.Context, policyID string, request source.RevokeRequest) (*source.LifecycleResult, error) {
	var result source.LifecycleResult
	path := "/api/v1/source-policies/" + url.PathEscape(strings.TrimSpace(policyID)) + "/revocations"
	if err := c.do(ctx, http.MethodPost, path, request, "", &result); err != nil {
		return nil, err
	}
	if err := requireSourcePolicyVersion(result.APIVersion); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *KernelHTTPClient) ListSourcePolicyActivations(ctx context.Context, scope capability.ScopeReference, policyID string) (*source.LifecycleEventList, error) {
	var result source.LifecycleEventList
	path := "/api/v1/source-policies/" + url.PathEscape(strings.TrimSpace(policyID)) + "/activations?" + capabilityScopeQuery(scope).Encode()
	if err := c.do(ctx, http.MethodGet, path, nil, "", &result); err != nil {
		return nil, err
	}
	if err := requireSourcePolicyVersion(result.APIVersion); err != nil {
		return nil, err
	}
	return &result, nil
}

func requireSourcePolicyVersion(version string) error {
	if version != source.LifecycleAPIVersion {
		return fmt.Errorf("unsupported source policy lifecycle contract %q", version)
	}
	return nil
}

func (c *KernelHTTPClient) ListAgentDefinitionCompilations(ctx context.Context, scope capability.ScopeReference, deploymentID string) ([]*kernelagent.DefinitionCompilation, error) {
	query := capabilityScopeQuery(scope)
	var history kernelapi.AgentDefinitionCompilationHistory
	path := "/api/v1/agent-deployments/" + url.PathEscape(strings.TrimSpace(deploymentID)) + "/compilations?" + query.Encode()
	if err := c.do(ctx, http.MethodGet, path, nil, "", &history); err != nil {
		return nil, err
	}
	if history.APIVersion != kernelapi.APIVersion {
		return nil, fmt.Errorf("unsupported Agent compilation history contract %q", history.APIVersion)
	}
	return history.Items, nil
}

func (c *KernelHTTPClient) InstallAgentManifest(ctx context.Context, request kernelagent.ManifestInstallationRequest, idempotencyKey string) (*kernelagent.ManifestInstallationResult, error) {
	if request.Manifest == nil || request.Deployment == nil {
		return nil, errors.New("agent manifest and deployment are required")
	}
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if idempotencyKey == "" {
		idempotencyKey = strings.TrimSpace(request.IdempotencyKey)
	}
	if idempotencyKey == "" {
		return nil, errors.New("agent manifest installation idempotency key is required")
	}
	request.IdempotencyKey = idempotencyKey
	var result kernelagent.ManifestInstallationResult
	if err := c.do(ctx, http.MethodPost, "/api/v1/agent-installations", request, idempotencyKey, &result); err != nil {
		return nil, err
	}
	if result.Definition == nil || result.Deployment == nil {
		return nil, errors.New("agent manifest installation omitted canonical resources")
	}
	return &result, nil
}

func (c *KernelHTTPClient) GetAgentDeployment(ctx context.Context, scope capability.ScopeReference, deploymentID string) (*kernelapi.AgentDeploymentCatalogEntry, error) {
	query := capabilityScopeQuery(scope)
	var result kernelapi.AgentDeploymentCatalogEntry
	path := "/api/v1/agent-deployments/" + url.PathEscape(strings.TrimSpace(deploymentID)) + "?" + query.Encode()
	if err := c.do(ctx, http.MethodGet, path, nil, "", &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *KernelHTTPClient) ListAgentDeployments(ctx context.Context, scope capability.ScopeReference) (*kernelapi.AgentDeploymentList, error) {
	query := capabilityScopeQuery(scope)
	var result kernelapi.AgentDeploymentList
	if err := c.do(ctx, http.MethodGet, "/api/v1/agent-deployments?"+query.Encode(), nil, "", &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *KernelHTTPClient) UpdateAgentDeployment(ctx context.Context, deploymentID string, request kernelapi.UpdateAgentDeploymentRequest) (*kernelapi.AgentDeploymentUpdateResult, error) {
	var result kernelapi.AgentDeploymentUpdateResult
	path := "/api/v1/agent-deployments/" + url.PathEscape(strings.TrimSpace(deploymentID))
	if err := c.do(ctx, http.MethodPut, path, request, "", &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *KernelHTTPClient) ActivateAgentDefinition(ctx context.Context, deploymentID string, request kernelapi.ActivateAgentDefinitionRequest) (*kernelapi.AgentDefinitionActivationResult, error) {
	var result kernelapi.AgentDefinitionActivationResult
	path := "/api/v1/agent-deployments/" + url.PathEscape(strings.TrimSpace(deploymentID)) + "/activations"
	if err := c.do(ctx, http.MethodPost, path, request, "", &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *KernelHTTPClient) RollbackAgentDefinition(ctx context.Context, deploymentID string, request kernelapi.RollbackAgentDefinitionRequest) (*kernelapi.AgentDefinitionActivationResult, error) {
	var result kernelapi.AgentDefinitionActivationResult
	path := "/api/v1/agent-deployments/" + url.PathEscape(strings.TrimSpace(deploymentID)) + "/rollbacks"
	if err := c.do(ctx, http.MethodPost, path, request, "", &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *KernelHTTPClient) ListAgentDefinitionActivations(ctx context.Context, scope capability.ScopeReference, deploymentID string) ([]workforce.DefinitionActivation, error) {
	var result []workforce.DefinitionActivation
	path := "/api/v1/agent-deployments/" + url.PathEscape(strings.TrimSpace(deploymentID)) + "/activations?" + capabilityScopeQuery(scope).Encode()
	if err := c.do(ctx, http.MethodGet, path, nil, "", &result); err != nil {
		return nil, err
	}
	return result, nil
}

func (c *KernelHTTPClient) ProposeAgentDefinitionAmendment(ctx context.Context, request kernelagent.ProposeAmendmentRequest) (*kernelagent.DefinitionAmendment, error) {
	var result kernelagent.DefinitionAmendment
	path := "/api/v1/agent-deployments/" + url.PathEscape(strings.TrimSpace(request.DeploymentID)) + "/amendments"
	if err := c.do(ctx, http.MethodPost, path, request, "", &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *KernelHTTPClient) GetAgentDefinitionAmendment(ctx context.Context, scope capability.ScopeReference, deploymentID, amendmentID string) (*kernelagent.DefinitionAmendment, error) {
	var result kernelagent.DefinitionAmendment
	path := "/api/v1/agent-deployments/" + url.PathEscape(strings.TrimSpace(deploymentID)) + "/amendments/" + url.PathEscape(strings.TrimSpace(amendmentID)) + "?" + capabilityScopeQuery(scope).Encode()
	if err := c.do(ctx, http.MethodGet, path, nil, "", &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *KernelHTTPClient) ListAgentDefinitionAmendments(ctx context.Context, scope capability.ScopeReference, deploymentID string) (*kernelapi.AgentDefinitionAmendmentList, error) {
	var result kernelapi.AgentDefinitionAmendmentList
	path := "/api/v1/agent-deployments/" + url.PathEscape(strings.TrimSpace(deploymentID)) + "/amendments?" + capabilityScopeQuery(scope).Encode()
	if err := c.do(ctx, http.MethodGet, path, nil, "", &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *KernelHTTPClient) SubmitAgentDefinitionAmendmentEvaluation(ctx context.Context, deploymentID string, request kernelagent.SubmitAmendmentEvaluationRequest) (*kernelagent.DefinitionAmendment, error) {
	var result kernelagent.DefinitionAmendment
	path := "/api/v1/agent-deployments/" + url.PathEscape(strings.TrimSpace(deploymentID)) + "/amendments/" + url.PathEscape(strings.TrimSpace(request.AmendmentID)) + "/evaluations"
	if err := c.do(ctx, http.MethodPost, path, request, "", &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *KernelHTTPClient) ResolveAgentDefinitionAmendment(ctx context.Context, deploymentID string, request kernelagent.ResolveAmendmentRequest) (*kernelagent.DefinitionAmendment, error) {
	var result kernelagent.DefinitionAmendment
	path := "/api/v1/agent-deployments/" + url.PathEscape(strings.TrimSpace(deploymentID)) + "/amendments/" + url.PathEscape(strings.TrimSpace(request.AmendmentID)) + "/decisions"
	if err := c.do(ctx, http.MethodPost, path, request, "", &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *KernelHTTPClient) ActivateAgentDefinitionAmendment(ctx context.Context, deploymentID, amendmentID string, request kernelapi.ActivateAgentDefinitionAmendmentRequest) (*kernelapi.AgentDefinitionAmendmentActivationResult, error) {
	var result kernelapi.AgentDefinitionAmendmentActivationResult
	path := "/api/v1/agent-deployments/" + url.PathEscape(strings.TrimSpace(deploymentID)) + "/amendments/" + url.PathEscape(strings.TrimSpace(amendmentID)) + "/activations"
	if err := c.do(ctx, http.MethodPost, path, request, "", &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// ListAgentSkillActions returns the exact, secret-safe Skill bindings an Agent
// may present to a model or operator. Semantic roles let callers select by
// portable intent instead of guessing action-specific argument names.
func (c *KernelHTTPClient) ListAgentSkillActions(ctx context.Context, scope capability.ScopeReference, deploymentID string, semanticRoles []string, sideEffect capability.SideEffect) (*kernelapi.SkillActionList, error) {
	query := capabilityScopeQuery(scope)
	for _, role := range semanticRoles {
		if role = strings.TrimSpace(role); role != "" {
			query.Add("semanticRole", role)
		}
	}
	if sideEffect != "" {
		query.Set("sideEffect", string(sideEffect))
	}
	var result kernelapi.SkillActionList
	path := "/api/v1/agent-deployments/" + url.PathEscape(strings.TrimSpace(deploymentID)) + "/skill-actions?" + query.Encode()
	if err := c.do(ctx, http.MethodGet, path, nil, "", &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *KernelHTTPClient) WorkforceChangeSetCapabilities(ctx context.Context, scope capability.ScopeReference, id string) (kernelapi.CapabilityDocument, error) {
	query := capabilityScopeQuery(scope)
	query.Set("changeSetId", strings.TrimSpace(id))
	var document kernelapi.CapabilityDocument
	err := c.do(ctx, http.MethodGet, "/api/v1/capabilities?"+query.Encode(), nil, "", &document)
	return document, err
}

func (c *KernelHTTPClient) CompileWorkforce(ctx context.Context, request authoring.GenerateRequest) (*authoring.CompileResult, error) {
	var result authoring.CompileResult
	if err := c.do(ctx, http.MethodPost, "/api/v1/authoring/workforce/compile", request, "", &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *KernelHTTPClient) CreateWorkforceChangeSet(ctx context.Context, request authoring.CreateChangeSetRequest, idempotencyKey string) (*authoring.ChangeSet, error) {
	var result authoring.ChangeSet
	if err := c.do(ctx, http.MethodPost, "/api/v1/authoring/workforce/change-sets", request, idempotencyKey, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *KernelHTTPClient) GetWorkforceChangeSet(ctx context.Context, scope capability.ScopeReference, id string) (*authoring.ChangeSet, error) {
	query := capabilityScopeQuery(scope)
	var result authoring.ChangeSet
	path := "/api/v1/authoring/workforce/change-sets/" + url.PathEscape(strings.TrimSpace(id)) + "?" + query.Encode()
	if err := c.do(ctx, http.MethodGet, path, nil, "", &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *KernelHTTPClient) UpdateWorkforceChangeSetPlacement(ctx context.Context, request authoring.UpdateChangeSetPlacementRequest, idempotencyKey string) (*authoring.ChangeSet, error) {
	var result authoring.ChangeSet
	path := "/api/v1/authoring/workforce/change-sets/" + url.PathEscape(strings.TrimSpace(request.ChangeSetID)) + "/placement"
	if err := c.do(ctx, http.MethodPatch, path, request, idempotencyKey, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *KernelHTTPClient) AnswerWorkforceChangeSetRefinement(ctx context.Context, request authoring.AnswerChangeSetRefinementRequest, idempotencyKey string) (*authoring.ChangeSet, error) {
	return c.mutateWorkforceChangeSet(ctx, request.ChangeSetID, "refinements", request, idempotencyKey)
}

func (c *KernelHTTPClient) SearchWorkforceSkills(ctx context.Context, request authoring.SkillSearchRequest) (*authoring.SkillSearchPage, error) {
	query := url.Values{}
	query.Set("scopeKind", request.Scope.Kind)
	query.Set("scopeId", request.Scope.ID)
	query.Set("query", request.Query)
	for _, action := range request.RequiredActions {
		query.Add("requiredAction", action)
	}
	if request.MaximumRisk != "" {
		query.Set("maximumRisk", string(request.MaximumRisk))
	}
	if request.Cursor != "" {
		query.Set("cursor", request.Cursor)
	}
	if request.Limit > 0 {
		query.Set("limit", strconv.Itoa(request.Limit))
	}
	var page authoring.SkillSearchPage
	if err := c.do(ctx, http.MethodGet, "/api/v1/authoring/workforce/skills?"+query.Encode(), nil, "", &page); err != nil {
		return nil, err
	}
	return &page, nil
}

func (c *KernelHTTPClient) EvaluateWorkforceChangeSet(ctx context.Context, request authoring.SubmitChangeSetEvaluationRequest, idempotencyKey string) (*authoring.ChangeSet, error) {
	return c.mutateWorkforceChangeSet(ctx, request.ChangeSetID, "evaluations", request, idempotencyKey)
}

func (c *KernelHTTPClient) RetryWorkforceChangeSetGeneration(ctx context.Context, request authoring.RetryChangeSetGenerationRequest, idempotencyKey string) (*authoring.ChangeSet, error) {
	return c.mutateWorkforceChangeSet(ctx, request.ChangeSetID, "retry", request, idempotencyKey)
}

func (c *KernelHTTPClient) ResolveWorkforceChangeSetApproval(ctx context.Context, request authoring.ResolveChangeSetApprovalRequest, idempotencyKey string) (*authoring.ChangeSet, error) {
	return c.mutateWorkforceChangeSet(ctx, request.ChangeSetID, "approvals", request, idempotencyKey)
}

func (c *KernelHTTPClient) PrepareWorkforceChangeSetActivation(ctx context.Context, request authoring.PrepareChangeSetActivationRequest, idempotencyKey string) (*authoring.ChangeSet, error) {
	return c.mutateWorkforceChangeSet(ctx, request.ChangeSetID, "activation", request, idempotencyKey)
}

func (c *KernelHTTPClient) ApplyWorkforceChangeSet(ctx context.Context, request authoring.ApplyChangeSetRequest, idempotencyKey string) (*authoring.ChangeSet, error) {
	return c.mutateWorkforceChangeSet(ctx, request.ChangeSetID, "apply", request, idempotencyKey)
}

func (c *KernelHTTPClient) mutateWorkforceChangeSet(ctx context.Context, id, operation string, request interface{}, idempotencyKey string) (*authoring.ChangeSet, error) {
	var result authoring.ChangeSet
	path := "/api/v1/authoring/workforce/change-sets/" + url.PathEscape(strings.TrimSpace(id)) + "/" + operation
	if err := c.do(ctx, http.MethodPost, path, request, strings.TrimSpace(idempotencyKey), &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *KernelHTTPClient) CreateObjective(ctx context.Context, request kernelapi.CreateObjectiveRequest, idempotencyKey string) (*runtime.Objective, error) {
	var objective runtime.Objective
	if err := c.do(ctx, http.MethodPost, "/api/v1/objectives", request, idempotencyKey, &objective); err != nil {
		return nil, err
	}
	return &objective, nil
}

func (c *KernelHTTPClient) ListObjectives(ctx context.Context, filter runtime.ObjectiveFilter) ([]*runtime.Objective, error) {
	query := scopeQuery(filter.Scope)
	if filter.Owner != nil {
		query.Set("ownerType", string(filter.Owner.Type))
		query.Set("ownerId", filter.Owner.ID)
	}
	for _, status := range filter.Statuses {
		query.Add("status", string(status))
	}
	if filter.Limit > 0 {
		query.Set("limit", strconv.Itoa(filter.Limit))
	}
	if filter.Offset > 0 {
		query.Set("offset", strconv.Itoa(filter.Offset))
	}
	var objectives []*runtime.Objective
	if err := c.do(ctx, http.MethodGet, "/api/v1/objectives?"+query.Encode(), nil, "", &objectives); err != nil {
		return nil, err
	}
	return objectives, nil
}

func (c *KernelHTTPClient) GetObjective(ctx context.Context, scope runtime.Scope, objectiveID string) (*kernelapi.ObjectiveDetail, error) {
	query := scopeQuery(scope)
	var detail kernelapi.ObjectiveDetail
	path := "/api/v1/objectives/" + url.PathEscape(strings.TrimSpace(objectiveID)) + "?" + query.Encode()
	if err := c.do(ctx, http.MethodGet, path, nil, "", &detail); err != nil {
		return nil, err
	}
	return &detail, nil
}

func (c *KernelHTTPClient) UpdateObjective(ctx context.Context, scope runtime.Scope, objectiveID string, request kernelapi.UpdateObjectiveRequest) (*runtime.Objective, error) {
	query := scopeQuery(scope)
	var objective runtime.Objective
	path := "/api/v1/objectives/" + url.PathEscape(strings.TrimSpace(objectiveID)) + "?" + query.Encode()
	if err := c.do(ctx, http.MethodPut, path, request, "", &objective); err != nil {
		return nil, err
	}
	return &objective, nil
}

func (c *KernelHTTPClient) ListRunbooks(ctx context.Context, filter runtime.RunbookActivationFilter) ([]*runtime.RunbookActivation, error) {
	query := scopeQuery(filter.Scope)
	if filter.Owner != nil {
		query.Set("ownerType", string(filter.Owner.Type))
		query.Set("ownerId", filter.Owner.ID)
	}
	if strings.TrimSpace(filter.ObjectiveID) != "" {
		query.Set("objectiveId", strings.TrimSpace(filter.ObjectiveID))
	}
	for _, status := range filter.Statuses {
		query.Add("status", string(status))
	}
	for _, kind := range filter.TriggerKinds {
		query.Add("triggerKind", string(kind))
	}
	if filter.Limit > 0 {
		query.Set("limit", strconv.Itoa(filter.Limit))
	}
	if filter.Offset > 0 {
		query.Set("offset", strconv.Itoa(filter.Offset))
	}
	var values []*runtime.RunbookActivation
	if err := c.do(ctx, http.MethodGet, "/api/v1/runbooks?"+query.Encode(), nil, "", &values); err != nil {
		return nil, err
	}
	return values, nil
}

func (c *KernelHTTPClient) GetRunbook(ctx context.Context, scope runtime.Scope, activationID string) (*runtime.RunbookDetail, error) {
	query := scopeQuery(scope)
	var value runtime.RunbookDetail
	path := "/api/v1/runbooks/" + url.PathEscape(strings.TrimSpace(activationID)) + "?" + query.Encode()
	if err := c.do(ctx, http.MethodGet, path, nil, "", &value); err != nil {
		return nil, err
	}
	return &value, nil
}

func (c *KernelHTTPClient) UpdateRunbook(ctx context.Context, scope runtime.Scope, activationID string, request runtime.UpdateRunbookActivationRequest) (*runtime.RunbookActivation, error) {
	query := scopeQuery(scope)
	var value runtime.RunbookActivation
	path := "/api/v1/runbooks/" + url.PathEscape(strings.TrimSpace(activationID)) + "?" + query.Encode()
	if err := c.do(ctx, http.MethodPatch, path, request, "", &value); err != nil {
		return nil, err
	}
	return &value, nil
}

func (c *KernelHTTPClient) StartRunbook(ctx context.Context, scope runtime.Scope, activationID string, request runtime.StartRunbookActivationRequest) (*runtime.AgentRunCommandResult, error) {
	query := scopeQuery(scope)
	var value runtime.AgentRunCommandResult
	path := "/api/v1/runbooks/" + url.PathEscape(strings.TrimSpace(activationID)) + "/runs?" + query.Encode()
	if err := c.do(ctx, http.MethodPost, path, request, "", &value); err != nil {
		return nil, err
	}
	return &value, nil
}

func (c *KernelHTTPClient) ReconcileRunbookSchedules(ctx context.Context, request kernelapi.ReconcileRunbookSchedulesRequest) (*kernelapi.RunbookScheduleReconciliation, error) {
	var reconciliation kernelapi.RunbookScheduleReconciliation
	if err := c.do(ctx, http.MethodPost, "/api/v1/runbooks/schedule-reconciliations", request, "", &reconciliation); err != nil {
		return nil, err
	}
	return &reconciliation, nil
}

func (c *KernelHTTPClient) CreateEventSourceSubscription(ctx context.Context, request kernelapi.CreateEventSourceSubscriptionRequest) (*runtime.EventSourceSubscription, error) {
	var value runtime.EventSourceSubscription
	if err := c.do(ctx, http.MethodPost, "/api/v1/event-source-subscriptions", request, "", &value); err != nil {
		return nil, err
	}
	return &value, nil
}

func (c *KernelHTTPClient) ListEventSourceSubscriptions(ctx context.Context, filter runtime.EventSourceSubscriptionFilter) ([]*runtime.EventSourceSubscription, error) {
	query := scopeQuery(filter.Scope)
	if filter.Owner != nil {
		query.Set("ownerType", string(filter.Owner.Type))
		query.Set("ownerId", filter.Owner.ID)
	}
	for _, status := range filter.Statuses {
		query.Add("status", string(status))
	}
	if filter.ConnectorKind != "" {
		query.Set("connectorKind", string(filter.ConnectorKind))
	}
	if filter.Limit > 0 {
		query.Set("limit", strconv.Itoa(filter.Limit))
	}
	if filter.Offset > 0 {
		query.Set("offset", strconv.Itoa(filter.Offset))
	}
	var values []*runtime.EventSourceSubscription
	if err := c.do(ctx, http.MethodGet, "/api/v1/event-source-subscriptions?"+query.Encode(), nil, "", &values); err != nil {
		return nil, err
	}
	return values, nil
}

func (c *KernelHTTPClient) GetEventSourceSubscription(ctx context.Context, scope runtime.Scope, id string) (*runtime.EventSourceSubscriptionDetail, error) {
	var value runtime.EventSourceSubscriptionDetail
	path := "/api/v1/event-source-subscriptions/" + url.PathEscape(strings.TrimSpace(id)) + "?" + scopeQuery(scope).Encode()
	if err := c.do(ctx, http.MethodGet, path, nil, "", &value); err != nil {
		return nil, err
	}
	return &value, nil
}

func (c *KernelHTTPClient) UpdateEventSourceSubscription(ctx context.Context, scope runtime.Scope, id string, request kernelapi.UpdateEventSourceSubscriptionRequest) (*runtime.EventSourceSubscription, error) {
	var value runtime.EventSourceSubscription
	path := "/api/v1/event-source-subscriptions/" + url.PathEscape(strings.TrimSpace(id)) + "?" + scopeQuery(scope).Encode()
	if err := c.do(ctx, http.MethodPatch, path, request, "", &value); err != nil {
		return nil, err
	}
	return &value, nil
}

func (c *KernelHTTPClient) RetireEventSourceSubscription(ctx context.Context, scope runtime.Scope, id string, request kernelapi.RetireEventSourceSubscriptionRequest) (*runtime.EventSourceSubscription, error) {
	var value runtime.EventSourceSubscription
	path := "/api/v1/event-source-subscriptions/" + url.PathEscape(strings.TrimSpace(id)) + "/retirements?" + scopeQuery(scope).Encode()
	if err := c.do(ctx, http.MethodPost, path, request, "", &value); err != nil {
		return nil, err
	}
	return &value, nil
}

func (c *KernelHTTPClient) ReportEventSourceHealth(ctx context.Context, scope runtime.Scope, id string, request kernelapi.ReportEventSourceHealthRequest) (*runtime.EventSourceHealth, error) {
	var value runtime.EventSourceHealth
	path := "/api/v1/event-source-subscriptions/" + url.PathEscape(strings.TrimSpace(id)) + "/health-reports?" + scopeQuery(scope).Encode()
	if err := c.do(ctx, http.MethodPost, path, request, "", &value); err != nil {
		return nil, err
	}
	return &value, nil
}

func (c *KernelHTTPClient) GetEventSourceCheckpoint(ctx context.Context, scope runtime.Scope, id string) (*runtime.EventSourceCheckpoint, error) {
	var value *runtime.EventSourceCheckpoint
	path := "/api/v1/event-source-subscriptions/" + url.PathEscape(strings.TrimSpace(id)) + "/checkpoint?" + scopeQuery(scope).Encode()
	if err := c.do(ctx, http.MethodGet, path, nil, "", &value); err != nil {
		return nil, err
	}
	return value, nil
}

func (c *KernelHTTPClient) AdvanceEventSourceCheckpoint(ctx context.Context, scope runtime.Scope, id string, request kernelapi.AdvanceEventSourceCheckpointRequest) (*runtime.EventSourceCheckpoint, error) {
	var value runtime.EventSourceCheckpoint
	path := "/api/v1/event-source-subscriptions/" + url.PathEscape(strings.TrimSpace(id)) + "/checkpoint-advancements?" + scopeQuery(scope).Encode()
	if err := c.do(ctx, http.MethodPost, path, request, "", &value); err != nil {
		return nil, err
	}
	return &value, nil
}

func (c *KernelHTTPClient) CreateExternalConversationGateway(
	ctx context.Context,
	request kernelapi.CreateExternalConversationGatewayRequest,
) (*runtime.ExternalConversationGatewayRegistration, error) {
	var value runtime.ExternalConversationGatewayRegistration
	if err := c.do(ctx, http.MethodPost, "/api/v1/conversation-gateways", request, "", &value); err != nil {
		return nil, err
	}
	return &value, nil
}

func (c *KernelHTTPClient) ListExternalConversationGateways(
	ctx context.Context,
	filter runtime.ExternalConversationGatewayFilter,
) ([]*runtime.ExternalConversationGatewayRegistration, error) {
	query := scopeQuery(filter.Scope)
	if filter.Provider != "" {
		query.Set("provider", filter.Provider)
	}
	for _, status := range filter.Statuses {
		query.Add("status", string(status))
	}
	if filter.Limit > 0 {
		query.Set("limit", strconv.Itoa(filter.Limit))
	}
	if filter.Offset > 0 {
		query.Set("offset", strconv.Itoa(filter.Offset))
	}
	var values []*runtime.ExternalConversationGatewayRegistration
	if err := c.do(ctx, http.MethodGet, "/api/v1/conversation-gateways?"+query.Encode(), nil, "", &values); err != nil {
		return nil, err
	}
	return values, nil
}

func (c *KernelHTTPClient) GetExternalConversationGateway(
	ctx context.Context,
	scope runtime.Scope,
	id string,
) (*runtime.ExternalConversationGatewayRegistration, error) {
	var value runtime.ExternalConversationGatewayRegistration
	path := "/api/v1/conversation-gateways/" + url.PathEscape(strings.TrimSpace(id)) + "?" + scopeQuery(scope).Encode()
	if err := c.do(ctx, http.MethodGet, path, nil, "", &value); err != nil {
		return nil, err
	}
	return &value, nil
}

func (c *KernelHTTPClient) UpdateExternalConversationGateway(
	ctx context.Context,
	scope runtime.Scope,
	id string,
	request kernelapi.UpdateExternalConversationGatewayRequest,
) (*runtime.ExternalConversationGatewayRegistration, error) {
	var value runtime.ExternalConversationGatewayRegistration
	path := "/api/v1/conversation-gateways/" + url.PathEscape(strings.TrimSpace(id)) + "?" + scopeQuery(scope).Encode()
	if err := c.do(ctx, http.MethodPatch, path, request, "", &value); err != nil {
		return nil, err
	}
	return &value, nil
}

func (c *KernelHTTPClient) RouteEvent(ctx context.Context, event runtime.EventEnvelope) (*runtime.EventRouteResult, error) {
	var result runtime.EventRouteResult
	if err := c.do(ctx, http.MethodPost, "/api/v1/events", event, "", &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *KernelHTTPClient) CreateProject(ctx context.Context, request kernelapi.CreateProjectRequest, idempotencyKey string) (*runtime.Project, error) {
	var project runtime.Project
	if err := c.do(ctx, http.MethodPost, "/api/v1/projects", request, idempotencyKey, &project); err != nil {
		return nil, err
	}
	return &project, nil
}

func (c *KernelHTTPClient) ListProjects(ctx context.Context, filter runtime.ProjectFilter) ([]*runtime.Project, error) {
	query := scopeQuery(filter.Scope)
	if filter.Owner != nil {
		query.Set("ownerType", string(filter.Owner.Type))
		query.Set("ownerId", filter.Owner.ID)
	}
	setIfPresent(query, "objectiveId", filter.ObjectiveID)
	for _, status := range filter.Statuses {
		query.Add("status", string(status))
	}
	if filter.Limit > 0 {
		query.Set("limit", strconv.Itoa(filter.Limit))
	}
	if filter.Offset > 0 {
		query.Set("offset", strconv.Itoa(filter.Offset))
	}
	var projects []*runtime.Project
	if err := c.do(ctx, http.MethodGet, "/api/v1/projects?"+query.Encode(), nil, "", &projects); err != nil {
		return nil, err
	}
	return projects, nil
}

func (c *KernelHTTPClient) GetProject(ctx context.Context, scope runtime.Scope, projectID string) (*runtime.Project, error) {
	query := scopeQuery(scope)
	var project runtime.Project
	path := "/api/v1/projects/" + url.PathEscape(strings.TrimSpace(projectID)) + "?" + query.Encode()
	if err := c.do(ctx, http.MethodGet, path, nil, "", &project); err != nil {
		return nil, err
	}
	return &project, nil
}

func (c *KernelHTTPClient) PatchProject(ctx context.Context, scope runtime.Scope, projectID string, request kernelapi.UpdateProjectRequest) (*runtime.Project, error) {
	query := scopeQuery(scope)
	var project runtime.Project
	path := "/api/v1/projects/" + url.PathEscape(strings.TrimSpace(projectID)) + "?" + query.Encode()
	if err := c.do(ctx, http.MethodPatch, path, request, "", &project); err != nil {
		return nil, err
	}
	return &project, nil
}

func (c *KernelHTTPClient) CreateOutreachThread(ctx context.Context, request kernelapi.CreateOutreachThreadRequest, idempotencyKey string) (*runtime.OutreachThread, error) {
	var thread runtime.OutreachThread
	path := "/api/v1/projects/" + url.PathEscape(strings.TrimSpace(request.ProjectID)) + "/outreach"
	if err := c.do(ctx, http.MethodPost, path, request, idempotencyKey, &thread); err != nil {
		return nil, err
	}
	return &thread, nil
}

func (c *KernelHTTPClient) ListOutreachThreads(ctx context.Context, filter runtime.OutreachThreadFilter) ([]*runtime.OutreachThread, error) {
	query := scopeQuery(filter.Scope)
	setIfPresent(query, "sourceObservationId", filter.SourceObservationID)
	for _, status := range filter.Statuses {
		query.Add("status", string(status))
	}
	if filter.Limit > 0 {
		query.Set("limit", strconv.Itoa(filter.Limit))
	}
	if filter.Offset > 0 {
		query.Set("offset", strconv.Itoa(filter.Offset))
	}
	var threads []*runtime.OutreachThread
	path := "/api/v1/projects/" + url.PathEscape(strings.TrimSpace(filter.ProjectID)) + "/outreach?" + query.Encode()
	if err := c.do(ctx, http.MethodGet, path, nil, "", &threads); err != nil {
		return nil, err
	}
	return threads, nil
}

func (c *KernelHTTPClient) GetOutreachThread(ctx context.Context, scope runtime.Scope, projectID, threadID string) (*runtime.OutreachThread, error) {
	query := scopeQuery(scope)
	var thread runtime.OutreachThread
	path := "/api/v1/projects/" + url.PathEscape(strings.TrimSpace(projectID)) + "/outreach/" + url.PathEscape(strings.TrimSpace(threadID)) + "?" + query.Encode()
	if err := c.do(ctx, http.MethodGet, path, nil, "", &thread); err != nil {
		return nil, err
	}
	return &thread, nil
}

func (c *KernelHTTPClient) DeliverOutreachMessage(ctx context.Context, projectID, threadID, messageID string, request kernelapi.DeliverOutreachMessageRequest, idempotencyKey string) (*runtime.AgentRun, error) {
	var run runtime.AgentRun
	path := "/api/v1/projects/" + url.PathEscape(strings.TrimSpace(projectID)) + "/outreach/" + url.PathEscape(strings.TrimSpace(threadID)) +
		"/messages/" + url.PathEscape(strings.TrimSpace(messageID)) + "/deliveries"
	if err := c.do(ctx, http.MethodPost, path, request, idempotencyKey, &run); err != nil {
		return nil, err
	}
	return &run, nil
}

func (c *KernelHTTPClient) ListActivity(ctx context.Context, request runtime.ActivityFeedRequest) (*runtime.ActivityFeedPage, error) {
	query := scopeQuery(request.Scope)
	for _, item := range []struct{ key, value string }{
		{"runId", request.RunID}, {"agentId", request.AgentID}, {"objectiveId", request.ObjectiveID}, {"projectId", request.ProjectID}, {"teamId", request.TeamID}, {"cursor", request.Cursor},
	} {
		if value := strings.TrimSpace(item.value); value != "" {
			query.Set(item.key, value)
		}
	}
	for _, eventType := range request.EventTypes {
		if eventType = strings.TrimSpace(eventType); eventType != "" {
			query.Add("eventType", eventType)
		}
	}
	for _, severity := range request.Severities {
		query.Add("severity", string(severity))
	}
	for _, visibility := range request.Visibilities {
		query.Add("visibility", string(visibility))
	}
	if request.Limit > 0 {
		query.Set("limit", strconv.Itoa(request.Limit))
	}
	if request.IncludeDetails {
		query.Set("includeDetails", "true")
	}
	var page runtime.ActivityFeedPage
	if err := c.do(ctx, http.MethodGet, "/api/v1/activity?"+query.Encode(), nil, "", &page); err != nil {
		return nil, err
	}
	return &page, nil
}

func (c *KernelHTTPClient) ListSourceObservations(ctx context.Context, filter runtime.SourceObservationFilter) ([]*runtime.SourceObservation, error) {
	query := scopeQuery(filter.Scope)
	if filter.Limit > 0 {
		query.Set("limit", strconv.Itoa(filter.Limit))
	}
	if filter.Offset > 0 {
		query.Set("offset", strconv.Itoa(filter.Offset))
	}
	var observations []*runtime.SourceObservation
	path := sourceMonitorPath(filter.ProjectID, filter.MonitorID, "observations") + "?" + query.Encode()
	if err := c.do(ctx, http.MethodGet, path, nil, "", &observations); err != nil {
		return nil, err
	}
	return observations, nil
}

func (c *KernelHTTPClient) GetSourceMonitorCheckpoint(ctx context.Context, scope runtime.Scope, projectID, monitorID string) (*runtime.SourceMonitorCheckpoint, error) {
	var checkpoint runtime.SourceMonitorCheckpoint
	path := sourceMonitorPath(projectID, monitorID, "checkpoint") + "?" + scopeQuery(scope).Encode()
	if err := c.do(ctx, http.MethodGet, path, nil, "", &checkpoint); err != nil {
		return nil, err
	}
	return &checkpoint, nil
}

func sourceMonitorPath(projectID, monitorID, suffix string) string {
	return "/api/v1/projects/" + url.PathEscape(strings.TrimSpace(projectID)) + "/source-monitors/" + url.PathEscape(strings.TrimSpace(monitorID)) + "/" + suffix
}

func clawHubPath(kind, reference, action string) string {
	path := "/api/v1/clawhub/" + kind + "/" + url.PathEscape(strings.TrimSpace(reference))
	if action != "" {
		path += "/" + action
	}
	return path
}

func (c *KernelHTTPClient) InspectClawHubSkill(ctx context.Context, reference clawhub.SkillReference) (*clawhub.SkillDetail, error) {
	var result clawhub.SkillDetail
	err := c.do(ctx, http.MethodGet, clawHubPath("catalog", reference.String(), ""), nil, "", &result)
	return &result, err
}
func (c *KernelHTTPClient) ListClawHubSkillVersions(ctx context.Context, reference clawhub.SkillReference, limit int, cursor string) (*clawhub.VersionPage, error) {
	query := url.Values{}
	if limit > 0 {
		query.Set("limit", strconv.Itoa(limit))
	}
	setIfPresent(query, "cursor", cursor)
	var result clawhub.VersionPage
	err := c.do(ctx, http.MethodGet, clawHubPath("catalog", reference.String(), "versions")+"?"+query.Encode(), nil, "", &result)
	return &result, err
}
func (c *KernelHTTPClient) VerifyClawHubSkill(ctx context.Context, reference clawhub.SkillReference, request kernelapi.ClawHubVersionRequest) (*clawhub.Verification, error) {
	var result clawhub.Verification
	err := c.do(ctx, http.MethodPost, clawHubPath("catalog", reference.String(), "verify"), request, "", &result)
	return &result, err
}
func (c *KernelHTTPClient) ListInstalledClawHubSkills(ctx context.Context) ([]clawhub.InstalledState, error) {
	var result []clawhub.InstalledState
	err := c.do(ctx, http.MethodGet, "/api/v1/clawhub/installed", nil, "", &result)
	return result, err
}
func (c *KernelHTTPClient) InstallClawHubSkill(ctx context.Context, reference clawhub.SkillReference, request kernelapi.ClawHubVersionRequest) (*clawhub.LifecycleResult, error) {
	var result clawhub.LifecycleResult
	err := c.do(ctx, http.MethodPost, clawHubPath("catalog", reference.String(), "install"), request, "", &result)
	return &result, err
}
func (c *KernelHTTPClient) VerifyInstalledClawHubSkill(ctx context.Context, reference string) (*clawhub.Verification, error) {
	var result clawhub.Verification
	err := c.do(ctx, http.MethodPost, clawHubPath("installed", reference, "verify"), struct{}{}, "", &result)
	return &result, err
}
func (c *KernelHTTPClient) PinClawHubSkill(ctx context.Context, reference, reason string) (*clawhub.LifecycleResult, error) {
	var result clawhub.LifecycleResult
	err := c.do(ctx, http.MethodPost, clawHubPath("installed", reference, "pin"), kernelapi.ClawHubPinRequest{Reason: reason}, "", &result)
	return &result, err
}
func (c *KernelHTTPClient) UnpinClawHubSkill(ctx context.Context, reference string) (*clawhub.LifecycleResult, error) {
	return c.clawHubMutation(ctx, reference, "unpin")
}
func (c *KernelHTTPClient) UpdateClawHubSkill(ctx context.Context, reference string) (*clawhub.LifecycleResult, error) {
	return c.clawHubMutation(ctx, reference, "update")
}
func (c *KernelHTTPClient) UninstallClawHubSkill(ctx context.Context, reference string) (*clawhub.LifecycleResult, error) {
	var result clawhub.LifecycleResult
	err := c.do(ctx, http.MethodDelete, clawHubPath("installed", reference, ""), nil, "", &result)
	return &result, err
}
func (c *KernelHTTPClient) clawHubMutation(ctx context.Context, reference, action string) (*clawhub.LifecycleResult, error) {
	var result clawhub.LifecycleResult
	err := c.do(ctx, http.MethodPost, clawHubPath("installed", reference, action), struct{}{}, "", &result)
	return &result, err
}
func (c *KernelHTTPClient) UpdateAllClawHubSkills(ctx context.Context) (*clawhub.LifecycleBatchResult, error) {
	var result clawhub.LifecycleBatchResult
	err := c.do(ctx, http.MethodPost, "/api/v1/clawhub/installed/update-all", struct{}{}, "", &result)
	return &result, err
}

func (c *KernelHTTPClient) CreateAgentRun(ctx context.Context, request kernelapi.CreateAgentRunRequest, idempotencyKey string) (*runtime.AgentRunCommandResult, error) {
	var result runtime.AgentRunCommandResult
	if err := c.do(ctx, http.MethodPost, "/api/v1/agent-runs", request, idempotencyKey, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *KernelHTTPClient) ListAgentRuns(ctx context.Context, filter runtime.AgentRunFilter) ([]*runtime.AgentRun, error) {
	query := scopeQuery(filter.Scope)
	setIfPresent(query, "kind", string(filter.Kind))
	setIfPresent(query, "objectiveId", filter.ObjectiveID)
	setIfPresent(query, "parentRunId", filter.ParentRunID)
	setIfPresent(query, "rootRunId", filter.RootRunID)
	setIfPresent(query, "assignedAgentId", filter.AssignedAgentID)
	if filter.Owner != nil {
		query.Set("ownerType", string(filter.Owner.Type))
		query.Set("ownerId", filter.Owner.ID)
	}
	for _, status := range filter.Statuses {
		query.Add("status", string(status))
	}
	if filter.Limit > 0 {
		query.Set("limit", strconv.Itoa(filter.Limit))
	}
	if filter.Offset > 0 {
		query.Set("offset", strconv.Itoa(filter.Offset))
	}
	var runs []*runtime.AgentRun
	if err := c.do(ctx, http.MethodGet, "/api/v1/agent-runs?"+query.Encode(), nil, "", &runs); err != nil {
		return nil, err
	}
	return runs, nil
}

func (c *KernelHTTPClient) GetAgentRun(ctx context.Context, scope runtime.Scope, runID string) (*runtime.AgentRun, error) {
	query := scopeQuery(scope)
	var run runtime.AgentRun
	path := "/api/v1/agent-runs/" + url.PathEscape(strings.TrimSpace(runID)) + "?" + query.Encode()
	if err := c.do(ctx, http.MethodGet, path, nil, "", &run); err != nil {
		return nil, err
	}
	return &run, nil
}

func (c *KernelHTTPClient) GetRunbookExecutionAudit(ctx context.Context, scope runtime.Scope, runID string) (*runtime.RunbookExecutionAudit, error) {
	query := scopeQuery(scope)
	var value runtime.RunbookExecutionAudit
	path := "/api/v1/agent-runs/" + url.PathEscape(strings.TrimSpace(runID)) + "/runbook-audit?" + query.Encode()
	if err := c.do(ctx, http.MethodGet, path, nil, "", &value); err != nil {
		return nil, err
	}
	return &value, nil
}

func (c *KernelHTTPClient) CommandAgentRun(ctx context.Context, scope runtime.Scope, runID string, request kernelapi.AgentRunCommandRequest) (*runtime.AgentRunCommandResult, error) {
	query := scopeQuery(scope)
	var result runtime.AgentRunCommandResult
	path := "/api/v1/agent-runs/" + url.PathEscape(strings.TrimSpace(runID)) + "/commands?" + query.Encode()
	if err := c.do(ctx, http.MethodPost, path, request, "", &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *KernelHTTPClient) CreateAgentRequest(ctx context.Context, request kernelapi.CreateAgentRequestRequest, idempotencyKey string) (*runtime.AgentRequestResult, error) {
	var result runtime.AgentRequestResult
	if err := c.do(ctx, http.MethodPost, "/api/v1/agent-requests", request, idempotencyKey, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *KernelHTTPClient) ListAgentRequests(ctx context.Context, filter runtime.AgentRequestFilter) ([]*runtime.AgentRequest, error) {
	query := scopeQuery(filter.Scope)
	setIfPresent(query, "sourceRunId", filter.SourceRunID)
	if filter.Requester != nil {
		query.Set("requesterType", string(filter.Requester.Type))
		query.Set("requesterId", filter.Requester.ID)
	}
	if filter.Recipient != nil {
		query.Set("recipientType", string(filter.Recipient.Type))
		query.Set("recipientId", filter.Recipient.ID)
	}
	for _, kind := range filter.Kinds {
		query.Add("kind", string(kind))
	}
	for _, status := range filter.Statuses {
		query.Add("status", string(status))
	}
	if filter.Limit > 0 {
		query.Set("limit", strconv.Itoa(filter.Limit))
	}
	if filter.Offset > 0 {
		query.Set("offset", strconv.Itoa(filter.Offset))
	}
	var requests []*runtime.AgentRequest
	if err := c.do(ctx, http.MethodGet, "/api/v1/agent-requests?"+query.Encode(), nil, "", &requests); err != nil {
		return nil, err
	}
	return requests, nil
}

func (c *KernelHTTPClient) GetAgentRequest(ctx context.Context, scope runtime.Scope, requestID string) (*runtime.AgentRequest, error) {
	var request runtime.AgentRequest
	path := "/api/v1/agent-requests/" + url.PathEscape(strings.TrimSpace(requestID)) + "?" + scopeQuery(scope).Encode()
	if err := c.do(ctx, http.MethodGet, path, nil, "", &request); err != nil {
		return nil, err
	}
	return &request, nil
}

func (c *KernelHTTPClient) RespondAgentRequest(ctx context.Context, scope runtime.Scope, requestID string, request kernelapi.RespondAgentRequestRequest) (*runtime.AgentRequestResult, error) {
	var result runtime.AgentRequestResult
	path := "/api/v1/agent-requests/" + url.PathEscape(strings.TrimSpace(requestID)) + "/responses?" + scopeQuery(scope).Encode()
	if err := c.do(ctx, http.MethodPost, path, request, "", &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *KernelHTTPClient) CompleteAgentRequest(ctx context.Context, scope runtime.Scope, requestID string, request kernelapi.CompleteAgentRequestRequest, idempotencyKey string) (*runtime.AgentRequestResult, error) {
	var result runtime.AgentRequestResult
	path := "/api/v1/agent-requests/" + url.PathEscape(strings.TrimSpace(requestID)) + "/completions?" + scopeQuery(scope).Encode()
	if err := c.do(ctx, http.MethodPost, path, request, idempotencyKey, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *KernelHTTPClient) ListActionCalls(ctx context.Context, filter runtime.ActionFilter) ([]*runtime.ActionCall, error) {
	query := scopeQuery(filter.Scope)
	setIfPresent(query, "runId", filter.RunID)
	for _, status := range filter.Status {
		query.Add("status", string(status))
	}
	if filter.Limit > 0 {
		query.Set("limit", strconv.Itoa(filter.Limit))
	}
	if filter.Offset > 0 {
		query.Set("offset", strconv.Itoa(filter.Offset))
	}
	var calls []*runtime.ActionCall
	if err := c.do(ctx, http.MethodGet, "/api/v1/action-calls?"+query.Encode(), nil, "", &calls); err != nil {
		return nil, err
	}
	return calls, nil
}

func (c *KernelHTTPClient) GetActionCall(ctx context.Context, scope runtime.Scope, actionID string) (*runtime.ActionCall, error) {
	var call runtime.ActionCall
	path := "/api/v1/action-calls/" + url.PathEscape(strings.TrimSpace(actionID)) + "?" + scopeQuery(scope).Encode()
	if err := c.do(ctx, http.MethodGet, path, nil, "", &call); err != nil {
		return nil, err
	}
	return &call, nil
}

func (c *KernelHTTPClient) ListAgentTurns(ctx context.Context, filter runtime.AgentTurnFilter) ([]*kernelapi.AgentTurnRecord, error) {
	query := scopeQuery(filter.Scope)
	setIfPresent(query, "runId", filter.RunID)
	for _, status := range filter.Statuses {
		query.Add("status", string(status))
	}
	if filter.AfterSequence > 0 {
		query.Set("afterSequence", strconv.FormatInt(filter.AfterSequence, 10))
	}
	if filter.Limit > 0 {
		query.Set("limit", strconv.Itoa(filter.Limit))
	}
	var turns []*kernelapi.AgentTurnRecord
	if err := c.do(ctx, http.MethodGet, "/api/v1/agent-turns?"+query.Encode(), nil, "", &turns); err != nil {
		return nil, err
	}
	return turns, nil
}

func (c *KernelHTTPClient) GetAgentTurn(ctx context.Context, scope runtime.Scope, turnID string) (*kernelapi.AgentTurnRecord, error) {
	var turn kernelapi.AgentTurnRecord
	path := "/api/v1/agent-turns/" + url.PathEscape(strings.TrimSpace(turnID)) + "?" + scopeQuery(scope).Encode()
	if err := c.do(ctx, http.MethodGet, path, nil, "", &turn); err != nil {
		return nil, err
	}
	return &turn, nil
}

func (c *KernelHTTPClient) ListActionApprovals(ctx context.Context, filter runtime.ApprovalFilter) ([]*runtime.ApprovalCheckpoint, error) {
	query := scopeQuery(filter.Scope)
	setIfPresent(query, "runId", filter.RunID)
	if filter.Owner != nil {
		query.Set("ownerType", string(filter.Owner.Type))
		query.Set("ownerId", filter.Owner.ID)
	}
	for _, status := range filter.Status {
		query.Add("status", string(status))
	}
	if filter.Limit > 0 {
		query.Set("limit", strconv.Itoa(filter.Limit))
	}
	if filter.Offset > 0 {
		query.Set("offset", strconv.Itoa(filter.Offset))
	}
	var approvals []*runtime.ApprovalCheckpoint
	if err := c.do(ctx, http.MethodGet, "/api/v1/action-approvals?"+query.Encode(), nil, "", &approvals); err != nil {
		return nil, err
	}
	return approvals, nil
}

func (c *KernelHTTPClient) GetActionApproval(ctx context.Context, scope runtime.Scope, approvalID string) (*runtime.ApprovalCheckpoint, error) {
	var approval runtime.ApprovalCheckpoint
	path := "/api/v1/action-approvals/" + url.PathEscape(strings.TrimSpace(approvalID)) + "?" + scopeQuery(scope).Encode()
	if err := c.do(ctx, http.MethodGet, path, nil, "", &approval); err != nil {
		return nil, err
	}
	return &approval, nil
}

func (c *KernelHTTPClient) ResolveActionApproval(ctx context.Context, scope runtime.Scope, approvalID string, request kernelapi.ResolveActionApprovalRequest, idempotencyKey string) (*runtime.ApprovalResolutionResult, error) {
	var result runtime.ApprovalResolutionResult
	path := "/api/v1/action-approvals/" + url.PathEscape(strings.TrimSpace(approvalID)) + "/decisions?" + scopeQuery(scope).Encode()
	if err := c.do(ctx, http.MethodPost, path, request, idempotencyKey, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *KernelHTTPClient) RegisterTeamDefinition(ctx context.Context, definition *kernelteam.Definition) (*kernelteam.Definition, error) {
	var result kernelteam.Definition
	if err := c.do(ctx, http.MethodPost, "/api/v1/team-definitions", definition, "", &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *KernelHTTPClient) GetTeamDefinition(ctx context.Context, id, version string) (*kernelteam.Definition, error) {
	query := make(url.Values)
	query.Set("version", strings.TrimSpace(version))
	var result kernelteam.Definition
	path := "/api/v1/team-definitions/" + url.PathEscape(strings.TrimSpace(id)) + "?" + query.Encode()
	if err := c.do(ctx, http.MethodGet, path, nil, "", &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *KernelHTTPClient) ListTeamDefinitionVersions(ctx context.Context, id string) ([]*kernelteam.Definition, error) {
	var result []*kernelteam.Definition
	path := "/api/v1/team-definitions/" + url.PathEscape(strings.TrimSpace(id))
	if err := c.do(ctx, http.MethodGet, path, nil, "", &result); err != nil {
		return nil, err
	}
	return result, nil
}

func (c *KernelHTTPClient) CreateTeamDeployment(ctx context.Context, request kernelapi.CreateTeamDeploymentRequest) (*kernelapi.TeamDeploymentResult, error) {
	var result kernelapi.TeamDeploymentResult
	if err := c.do(ctx, http.MethodPost, "/api/v1/team-deployments", request, "", &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *KernelHTTPClient) GetTeamDeployment(ctx context.Context, scope capability.ScopeReference, id string) (*kernelteam.Deployment, error) {
	query := capabilityScopeQuery(scope)
	var result kernelteam.Deployment
	path := "/api/v1/team-deployments/" + url.PathEscape(strings.TrimSpace(id)) + "?" + query.Encode()
	if err := c.do(ctx, http.MethodGet, path, nil, "", &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *KernelHTTPClient) ListTeamDeployments(ctx context.Context, scope capability.ScopeReference) (*kernelapi.TeamDeploymentList, error) {
	query := url.Values{"scopeKind": []string{scope.Kind}, "scopeId": []string{scope.ID}}
	var result kernelapi.TeamDeploymentList
	if err := c.do(ctx, http.MethodGet, "/api/v1/team-deployments?"+query.Encode(), nil, "", &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *KernelHTTPClient) ListTeamSkillBindings(ctx context.Context, scope capability.ScopeReference, deploymentID string) (*kernelapi.SkillBindingList, error) {
	return c.ListSkillBindings(ctx, scope, SkillBindingOwner{Type: runtime.OwnerTypeTeam, DeploymentID: deploymentID})
}

func skillBindingOwnerPath(owner SkillBindingOwner) (string, error) {
	id := strings.TrimSpace(owner.DeploymentID)
	if id == "" {
		return "", errors.New("deployment id is required")
	}
	switch owner.Type {
	case runtime.OwnerTypeAgent:
		return "/api/v1/agent-deployments/" + url.PathEscape(id), nil
	case runtime.OwnerTypeTeam:
		return "/api/v1/team-deployments/" + url.PathEscape(id), nil
	default:
		return "", fmt.Errorf("Skill bindings are not supported for owner type %q", owner.Type)
	}
}

func (c *KernelHTTPClient) ListSkillBindings(ctx context.Context, scope capability.ScopeReference, owner SkillBindingOwner) (*kernelapi.SkillBindingList, error) {
	var result kernelapi.SkillBindingList
	root, err := skillBindingOwnerPath(owner)
	if err != nil {
		return nil, err
	}
	path := root + "/skill-bindings?" + capabilityScopeQuery(scope).Encode()
	if err := c.do(ctx, http.MethodGet, path, nil, "", &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *KernelHTTPClient) UpsertTeamSkillBinding(ctx context.Context, deploymentID string, request skill.UpsertBindingRequest) (*kernelapi.SkillBindingMutationResult, error) {
	return c.UpsertSkillBinding(ctx, SkillBindingOwner{Type: runtime.OwnerTypeTeam, DeploymentID: deploymentID}, request)
}

func (c *KernelHTTPClient) UpsertSkillBinding(ctx context.Context, owner SkillBindingOwner, request skill.UpsertBindingRequest) (*kernelapi.SkillBindingMutationResult, error) {
	var result kernelapi.SkillBindingMutationResult
	if request.Binding == nil {
		return nil, errors.New("binding is required")
	}
	root, err := skillBindingOwnerPath(owner)
	if err != nil {
		return nil, err
	}
	path := root + "/skill-bindings/" + url.PathEscape(strings.TrimSpace(request.Binding.ID)) + "?" + capabilityScopeQuery(request.Binding.Scope).Encode()
	if err := c.do(ctx, http.MethodPut, path, request, "", &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *KernelHTTPClient) DisableTeamSkillBinding(ctx context.Context, deploymentID string, request skill.DisableBindingRequest) (*kernelapi.SkillBindingMutationResult, error) {
	return c.DisableSkillBinding(ctx, SkillBindingOwner{Type: runtime.OwnerTypeTeam, DeploymentID: deploymentID}, request)
}

func (c *KernelHTTPClient) DisableSkillBinding(ctx context.Context, owner SkillBindingOwner, request skill.DisableBindingRequest) (*kernelapi.SkillBindingMutationResult, error) {
	var result kernelapi.SkillBindingMutationResult
	root, err := skillBindingOwnerPath(owner)
	if err != nil {
		return nil, err
	}
	path := root + "/skill-bindings/" + url.PathEscape(strings.TrimSpace(request.BindingID)) + "/disable?" + capabilityScopeQuery(request.Scope).Encode()
	if err := c.do(ctx, http.MethodPost, path, request, "", &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *KernelHTTPClient) PlanTeamSkillReferenceUpgrade(ctx context.Context, deploymentID string, request runtime.PlanSkillReferenceUpgradeRequest) (*runtime.SkillReferenceUpgradePlan, error) {
	return c.PlanSkillReferenceUpgrade(ctx, SkillBindingOwner{Type: runtime.OwnerTypeTeam, DeploymentID: deploymentID}, request)
}

func (c *KernelHTTPClient) PlanSkillReferenceUpgrade(ctx context.Context, owner SkillBindingOwner, request runtime.PlanSkillReferenceUpgradeRequest) (*runtime.SkillReferenceUpgradePlan, error) {
	var result runtime.SkillReferenceUpgradePlan
	root, err := skillBindingOwnerPath(owner)
	if err != nil {
		return nil, err
	}
	scope := capability.ScopeReference{Kind: request.Scope.Kind, ID: request.Scope.ID}
	path := root + "/skill-bindings/" + url.PathEscape(strings.TrimSpace(request.BindingID)) + "/upgrade-plan?" + capabilityScopeQuery(scope).Encode()
	if err := c.do(ctx, http.MethodPost, path, request, "", &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *KernelHTTPClient) ApplyTeamSkillReferenceUpgrade(ctx context.Context, deploymentID string, request runtime.ApplySkillReferenceUpgradeRequest) (*runtime.SkillReferenceUpgradeReceipt, error) {
	return c.ApplySkillReferenceUpgrade(ctx, SkillBindingOwner{Type: runtime.OwnerTypeTeam, DeploymentID: deploymentID}, request)
}

func (c *KernelHTTPClient) ApplySkillReferenceUpgrade(ctx context.Context, owner SkillBindingOwner, request runtime.ApplySkillReferenceUpgradeRequest) (*runtime.SkillReferenceUpgradeReceipt, error) {
	if request.Plan == nil {
		return nil, errors.New("reviewed Skill reference upgrade plan is required")
	}
	var result runtime.SkillReferenceUpgradeReceipt
	root, err := skillBindingOwnerPath(owner)
	if err != nil {
		return nil, err
	}
	scope := capability.ScopeReference{Kind: request.Plan.Scope.Kind, ID: request.Plan.Scope.ID}
	path := root + "/skill-bindings/" + url.PathEscape(strings.TrimSpace(request.Plan.BindingID)) + "/upgrade?" + capabilityScopeQuery(scope).Encode()
	if err := c.do(ctx, http.MethodPost, path, request, "", &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *KernelHTTPClient) UpdateTeamDeployment(ctx context.Context, deploymentID string, request kernelapi.UpdateTeamDeploymentRequest) (*kernelapi.TeamDeploymentResult, error) {
	var result kernelapi.TeamDeploymentResult
	path := "/api/v1/team-deployments/" + url.PathEscape(strings.TrimSpace(deploymentID))
	if err := c.do(ctx, http.MethodPut, path, request, "", &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *KernelHTTPClient) ActivateTeamDefinition(ctx context.Context, deploymentID string, request kernelapi.ActivateTeamDefinitionRequest) (*kernelapi.TeamDeploymentResult, error) {
	var result kernelapi.TeamDeploymentResult
	path := "/api/v1/team-deployments/" + url.PathEscape(strings.TrimSpace(deploymentID)) + "/activations"
	if err := c.do(ctx, http.MethodPost, path, request, "", &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *KernelHTTPClient) ListTeamDefinitionActivations(ctx context.Context, scope capability.ScopeReference, deploymentID string) ([]workforce.DefinitionActivation, error) {
	query := capabilityScopeQuery(scope)
	var result []workforce.DefinitionActivation
	path := "/api/v1/team-deployments/" + url.PathEscape(strings.TrimSpace(deploymentID)) + "/activations?" + query.Encode()
	if err := c.do(ctx, http.MethodGet, path, nil, "", &result); err != nil {
		return nil, err
	}
	return result, nil
}

func (c *KernelHTTPClient) ProposeTeamDefinitionAmendment(ctx context.Context, request kernelteam.ProposeAmendmentRequest) (*kernelteam.DefinitionAmendment, error) {
	var result kernelteam.DefinitionAmendment
	path := "/api/v1/team-deployments/" + url.PathEscape(strings.TrimSpace(request.DeploymentID)) + "/amendments"
	if err := c.do(ctx, http.MethodPost, path, request, "", &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *KernelHTTPClient) GetTeamDefinitionAmendment(ctx context.Context, scope capability.ScopeReference, deploymentID, amendmentID string) (*kernelteam.DefinitionAmendment, error) {
	var result kernelteam.DefinitionAmendment
	path := "/api/v1/team-deployments/" + url.PathEscape(strings.TrimSpace(deploymentID)) + "/amendments/" + url.PathEscape(strings.TrimSpace(amendmentID)) + "?" + capabilityScopeQuery(scope).Encode()
	if err := c.do(ctx, http.MethodGet, path, nil, "", &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *KernelHTTPClient) ListTeamDefinitionAmendments(ctx context.Context, scope capability.ScopeReference, deploymentID string) (*kernelapi.TeamDefinitionAmendmentList, error) {
	var result kernelapi.TeamDefinitionAmendmentList
	path := "/api/v1/team-deployments/" + url.PathEscape(strings.TrimSpace(deploymentID)) + "/amendments?" + capabilityScopeQuery(scope).Encode()
	if err := c.do(ctx, http.MethodGet, path, nil, "", &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *KernelHTTPClient) SubmitTeamDefinitionAmendmentEvaluation(ctx context.Context, deploymentID string, request kernelteam.SubmitAmendmentEvaluationRequest) (*kernelteam.DefinitionAmendment, error) {
	var result kernelteam.DefinitionAmendment
	path := "/api/v1/team-deployments/" + url.PathEscape(strings.TrimSpace(deploymentID)) + "/amendments/" + url.PathEscape(strings.TrimSpace(request.AmendmentID)) + "/evaluations"
	if err := c.do(ctx, http.MethodPost, path, request, "", &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *KernelHTTPClient) ResolveTeamDefinitionAmendment(ctx context.Context, deploymentID string, request kernelteam.ResolveAmendmentRequest) (*kernelteam.DefinitionAmendment, error) {
	var result kernelteam.DefinitionAmendment
	path := "/api/v1/team-deployments/" + url.PathEscape(strings.TrimSpace(deploymentID)) + "/amendments/" + url.PathEscape(strings.TrimSpace(request.AmendmentID)) + "/decisions"
	if err := c.do(ctx, http.MethodPost, path, request, "", &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *KernelHTTPClient) ActivateTeamDefinitionAmendment(ctx context.Context, deploymentID, amendmentID string, request kernelapi.ActivateTeamDefinitionAmendmentRequest) (*kernelapi.TeamDefinitionAmendmentActivationResult, error) {
	var result kernelapi.TeamDefinitionAmendmentActivationResult
	path := "/api/v1/team-deployments/" + url.PathEscape(strings.TrimSpace(deploymentID)) + "/amendments/" + url.PathEscape(strings.TrimSpace(amendmentID)) + "/activations"
	if err := c.do(ctx, http.MethodPost, path, request, "", &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *KernelHTTPClient) do(ctx context.Context, method, path string, body interface{}, idempotencyKey string, result interface{}) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode OpenSeal request: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}
	requestPath := strings.TrimPrefix(path, "/api/v1")
	req, err := http.NewRequestWithContext(ctx, method, c.apiRoot+requestPath, reader)
	if err != nil {
		return fmt.Errorf("build OpenSeal request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for name, values := range c.requestHeaders {
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}
	if idempotencyKey = strings.TrimSpace(idempotencyKey); idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("connect to OpenSeal at %s: %w", c.baseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		decoder := json.NewDecoder(io.LimitReader(resp.Body, 8<<20))
		return decodeAPIError(resp.StatusCode, decoder)
	}
	if result == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	payload, err := io.ReadAll(io.LimitReader(resp.Body, (8<<20)+1))
	if err != nil {
		return fmt.Errorf("read OpenSeal response: %w", err)
	}
	if len(payload) > 8<<20 {
		return fmt.Errorf("decode OpenSeal response: payload exceeds 8 MiB")
	}
	if err := decodeKernelResult(payload, result); err != nil {
		return fmt.Errorf("decode OpenSeal response: %w", err)
	}
	return nil
}

func decodeKernelResult(payload []byte, result interface{}) error {
	var envelope struct {
		Code   *int            `json:"code"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(payload, &envelope); err == nil && envelope.Code != nil && len(envelope.Result) > 0 && string(envelope.Result) != "null" {
		return json.Unmarshal(envelope.Result, result)
	}
	return json.Unmarshal(payload, result)
}

func decodeAPIError(statusCode int, decoder *json.Decoder) error {
	var payload struct {
		Error string `json:"error"`
	}
	if decodeErr := decoder.Decode(&payload); decodeErr != nil && !errors.Is(decodeErr, io.EOF) {
		payload.Error = http.StatusText(statusCode)
	}
	return &APIError{StatusCode: statusCode, Message: strings.TrimSpace(payload.Error)}
}

func scopeQuery(scope runtime.Scope) url.Values {
	query := make(url.Values)
	query.Set("scopeKind", strings.TrimSpace(scope.Kind))
	query.Set("scopeId", strings.TrimSpace(scope.ID))
	return query
}

func capabilityScopeQuery(scope capability.ScopeReference) url.Values {
	query := make(url.Values)
	query.Set("scopeKind", strings.TrimSpace(scope.Kind))
	query.Set("scopeId", strings.TrimSpace(scope.ID))
	return query
}

func setIfPresent(query url.Values, key, value string) {
	if value = strings.TrimSpace(value); value != "" {
		query.Set(key, value)
	}
}

var _ KernelClient = (*KernelHTTPClient)(nil)
var _ AgentDefinitionCapabilityClient = (*KernelHTTPClient)(nil)
