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
	kernelbundle "github.com/axiom-studio/openseal/pkg/bundle"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/kernelapi"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/activity"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/collaboration"
	"github.com/axiom-studio/openseal/pkg/runtime/routine"
	"github.com/axiom-studio/openseal/pkg/runtime/skillbinding"
	"github.com/axiom-studio/openseal/pkg/skill"
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
	CreateObjective(context.Context, kernelapi.CreateObjectiveRequest, string) (*models.Objective, error)
	ListObjectives(context.Context, models.ObjectiveFilter) ([]*models.Objective, error)
	GetObjective(context.Context, models.Scope, string) (*kernelapi.ObjectiveDetail, error)
	UpdateObjective(context.Context, models.Scope, string, kernelapi.UpdateObjectiveRequest) (*models.Objective, error)
	ListRunbooks(context.Context, models.RunbookActivationFilter) ([]*models.RunbookActivation, error)
	GetRunbook(context.Context, models.Scope, string) (*routine.RunbookDetail, error)
	UpdateRunbook(context.Context, models.Scope, string, routine.UpdateRunbookActivationRequest) (*models.RunbookActivation, error)
	StartRunbook(context.Context, models.Scope, string, routine.StartRunbookActivationRequest) (*agentrun.CommandResult, error)
	ReconcileRunbookSchedules(context.Context, kernelapi.ReconcileRunbookSchedulesRequest) (*kernelapi.RunbookScheduleReconciliation, error)
	ListActivity(context.Context, activity.FeedRequest) (*activity.FeedPage, error)
	CreateAgentRun(context.Context, kernelapi.CreateAgentRunRequest, string) (*agentrun.CommandResult, error)
	ListAgentRuns(context.Context, models.AgentRunFilter) ([]*models.AgentRun, error)
	GetAgentRun(context.Context, models.Scope, string) (*models.AgentRun, error)
	CommandAgentRun(context.Context, models.Scope, string, kernelapi.AgentRunCommandRequest) (*agentrun.CommandResult, error)
	ListAgentTurns(context.Context, models.AgentTurnFilter) ([]*kernelapi.AgentTurnRecord, error)
	GetAgentTurn(context.Context, models.Scope, string) (*kernelapi.AgentTurnRecord, error)
	CreateAgentRequest(context.Context, kernelapi.CreateAgentRequestRequest, string) (*collaboration.AgentRequestResult, error)
	ListAgentRequests(context.Context, models.AgentRequestFilter) ([]*models.AgentRequest, error)
	GetAgentRequest(context.Context, models.Scope, string) (*models.AgentRequest, error)
	RespondAgentRequest(context.Context, models.Scope, string, kernelapi.RespondAgentRequestRequest) (*collaboration.AgentRequestResult, error)
	CompleteAgentRequest(context.Context, models.Scope, string, kernelapi.CompleteAgentRequestRequest, string) (*collaboration.AgentRequestResult, error)
	ReviewAgentRequestCompletion(context.Context, models.Scope, string, kernelapi.ReviewAgentRequestCompletionRequest, string) (*collaboration.AgentRequestResult, error)
	ListActionCalls(context.Context, models.ActionFilter) ([]*models.ActionCall, error)
	GetActionCall(context.Context, models.Scope, string) (*models.ActionCall, error)
	ListActionApprovals(context.Context, models.ApprovalFilter) ([]*models.ApprovalCheckpoint, error)
	GetActionApproval(context.Context, models.Scope, string) (*models.ApprovalCheckpoint, error)
	ResolveActionApproval(context.Context, models.Scope, string, kernelapi.ResolveActionApprovalRequest, string) (*models.ApprovalResolutionResult, error)
	ListAgentDefinitionCompilations(context.Context, capability.ScopeReference, string) ([]*kernelagent.DefinitionCompilation, error)
	InstallAgentManifest(context.Context, kernelagent.ManifestInstallationRequest, string) (*kernelagent.ManifestInstallationResult, error)
	GetAgentDeployment(context.Context, capability.ScopeReference, string) (*kernelapi.AgentDeploymentCatalogEntry, error)
	ListAgentDeployments(context.Context, kernelagent.AgentDeploymentFilter) (*kernelapi.AgentDeploymentList, error)
	UpdateAgentDeployment(context.Context, string, kernelapi.UpdateAgentDeploymentRequest) (*kernelapi.AgentDeploymentUpdateResult, error)
	ListAgentSkillActions(context.Context, capability.ScopeReference, string, []string, capability.SideEffect) (*kernelapi.SkillActionList, error)
}

// RunbookExecutionAuditClient exposes the optional Runbook-first audit read.
// Interactive surfaces capability-gate it with agent-runs.audit so clients
// connected to an older or deliberately minimal kernel remain valid.
type RunbookExecutionAuditClient interface {
	GetRunbookExecutionAudit(context.Context, models.Scope, string) (*routine.RunbookExecutionAudit, error)
	GetRunExecutionAudit(context.Context, models.Scope, string) (*routine.RunExecutionAudit, error)
}

var _ RunbookExecutionAuditClient = (*KernelHTTPClient)(nil)

// WorkforceBundleClient is capability-gated by workforce-bundles/v1. Keeping
// it additive lets older kernels and deliberately minimal hosts remain valid.
type WorkforceBundleClient interface {
	ValidateWorkforceBundle(context.Context, *kernelbundle.Bundle) error
	InspectWorkforceBundle(context.Context, *kernelbundle.Bundle) (*kernelbundle.Inspection, error)
	CompareWorkforceBundles(context.Context, *kernelbundle.Bundle, *kernelbundle.Bundle) (*kernelbundle.Diff, error)
	PreviewWorkforceBundleInstallation(context.Context, *kernelbundle.Bundle, kernelbundle.Placement) (*kernelbundle.InstallationPreview, error)
	PlanWorkforceBundleUpgrade(context.Context, *kernelbundle.Bundle, *kernelbundle.Bundle) (*kernelbundle.UpgradePlan, error)
	InstallWorkforceBundle(context.Context, *kernelbundle.Bundle, capability.ScopeReference, kernelbundle.Placement, string, string) (*kernelbundle.InstallationReceipt, error)
}

var _ WorkforceBundleClient = (*KernelHTTPClient)(nil)

// ExternalConversationGatewayClient is the narrow lifecycle surface used by
// hosts that advertise conversation-gateways/v2. Keeping it separate from the
// base KernelClient lets older or read-only embedding surfaces remain honest.
type ExternalConversationGatewayClient interface {
	CreateExternalConversationGateway(context.Context, kernelapi.CreateExternalConversationGatewayRequest) (*models.ExternalConversationGatewayRegistration, error)
	ListExternalConversationGateways(context.Context, models.ExternalConversationGatewayFilter) ([]*models.ExternalConversationGatewayRegistration, error)
	GetExternalConversationGateway(context.Context, models.Scope, string) (*models.ExternalConversationGatewayRegistration, error)
	UpdateExternalConversationGateway(context.Context, models.Scope, string, kernelapi.UpdateExternalConversationGatewayRequest) (*models.ExternalConversationGatewayRegistration, error)
}

// AgentDefinitionCapabilityClient discovers the operations authorized for one
// exact Agent deployment. Hosts may expose read-only Agent definition
// capabilities globally and add governed mutations only after applying the
// deployment's scope, revision, and caller policy.
type AgentDefinitionCapabilityClient interface {
	AgentDefinitionCapabilities(context.Context, capability.ScopeReference, string) (kernelapi.CapabilityDocument, error)
}

func (c *KernelHTTPClient) ValidateWorkforceBundle(ctx context.Context, bundle *kernelbundle.Bundle) error {
	var result struct {
		Valid bool `json:"valid"`
	}
	if err := c.do(ctx, http.MethodPost, "/api/v1/workforce-bundles/validate", bundle, "", &result); err != nil {
		return err
	}
	if !result.Valid {
		return errors.New("kernel did not validate workforce bundle")
	}
	return nil
}

func (c *KernelHTTPClient) InspectWorkforceBundle(ctx context.Context, bundle *kernelbundle.Bundle) (*kernelbundle.Inspection, error) {
	var result kernelbundle.Inspection
	if err := c.do(ctx, http.MethodPost, "/api/v1/workforce-bundles/inspect", bundle, "", &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *KernelHTTPClient) CompareWorkforceBundles(ctx context.Context, from, to *kernelbundle.Bundle) (*kernelbundle.Diff, error) {
	var result kernelbundle.Diff
	payload := struct {
		From *kernelbundle.Bundle `json:"from"`
		To   *kernelbundle.Bundle `json:"to"`
	}{From: from, To: to}
	if err := c.do(ctx, http.MethodPost, "/api/v1/workforce-bundles/compare", payload, "", &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *KernelHTTPClient) PreviewWorkforceBundleInstallation(ctx context.Context, bundle *kernelbundle.Bundle, placement kernelbundle.Placement) (*kernelbundle.InstallationPreview, error) {
	var result kernelbundle.InstallationPreview
	payload := struct {
		Bundle    *kernelbundle.Bundle   `json:"bundle"`
		Placement kernelbundle.Placement `json:"placement"`
	}{Bundle: bundle, Placement: placement}
	if err := c.do(ctx, http.MethodPost, "/api/v1/workforce-bundles/installation-preview", payload, "", &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *KernelHTTPClient) PlanWorkforceBundleUpgrade(ctx context.Context, current, target *kernelbundle.Bundle) (*kernelbundle.UpgradePlan, error) {
	var result kernelbundle.UpgradePlan
	payload := struct {
		Current *kernelbundle.Bundle `json:"current"`
		Target  *kernelbundle.Bundle `json:"target"`
	}{Current: current, Target: target}
	if err := c.do(ctx, http.MethodPost, "/api/v1/workforce-bundles/upgrade-plan", payload, "", &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *KernelHTTPClient) InstallWorkforceBundle(ctx context.Context, bundle *kernelbundle.Bundle, scope capability.ScopeReference, placement kernelbundle.Placement, reason, idempotencyKey string) (*kernelbundle.InstallationReceipt, error) {
	var result kernelbundle.InstallationReceipt
	payload := struct {
		Bundle    *kernelbundle.Bundle      `json:"bundle"`
		Scope     capability.ScopeReference `json:"scope"`
		Placement kernelbundle.Placement    `json:"placement"`
		Reason    string                    `json:"reason"`
	}{Bundle: bundle, Scope: scope, Placement: placement, Reason: reason}
	if err := c.do(ctx, http.MethodPost, "/api/v1/workforce-bundles/install", payload, idempotencyKey, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

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
	PlanTeamSkillReferenceUpgrade(context.Context, string, skillbinding.PlanSkillReferenceUpgradeRequest) (*models.SkillReferenceUpgradePlan, error)
	ApplyTeamSkillReferenceUpgrade(context.Context, string, skillbinding.ApplySkillReferenceUpgradeRequest) (*models.SkillReferenceUpgradeReceipt, error)
}

// SkillBindingOwner identifies the canonical deployment resource that owns a
// Skill portfolio. The binding representation is identical for both kinds.
type SkillBindingOwner struct {
	Type         models.OwnerType
	DeploymentID string
}

// SkillBindingClient is the owner-neutral management boundary used by prompt-
// first clients. Hosts may implement it without exposing unrelated APIs.
type SkillBindingClient interface {
	ListSkillBindings(context.Context, capability.ScopeReference, SkillBindingOwner) (*kernelapi.SkillBindingList, error)
	UpsertSkillBinding(context.Context, SkillBindingOwner, skill.UpsertBindingRequest) (*kernelapi.SkillBindingMutationResult, error)
	DisableSkillBinding(context.Context, SkillBindingOwner, skill.DisableBindingRequest) (*kernelapi.SkillBindingMutationResult, error)
	PlanSkillReferenceUpgrade(context.Context, SkillBindingOwner, skillbinding.PlanSkillReferenceUpgradeRequest) (*models.SkillReferenceUpgradePlan, error)
	ApplySkillReferenceUpgrade(context.Context, SkillBindingOwner, skillbinding.ApplySkillReferenceUpgradeRequest) (*models.SkillReferenceUpgradeReceipt, error)
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

func (c *KernelHTTPClient) ListAgentDeployments(ctx context.Context, filter kernelagent.AgentDeploymentFilter) (*kernelapi.AgentDeploymentList, error) {
	query := capabilityScopeQuery(filter.Scope)
	for _, status := range filter.ExcludeStatuses {
		query.Add("excludeStatuses", string(status))
	}
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

// StartAgentBuilderConversation opens a builder conversation for the
// authenticated local operator and posts the request as its first message.
func (c *KernelHTTPClient) StartAgentBuilderConversation(ctx context.Context, request kernelapi.StartAgentBuilderConversationRequest) (*kernelapi.StartAgentBuilderConversationResponse, error) {
	var result kernelapi.StartAgentBuilderConversationResponse
	if err := c.do(ctx, http.MethodPost, "/api/v1/agent-builder/conversations", request, strings.TrimSpace(request.IdempotencyKey), &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// ListAgentBuilderDrafts lists the local operator's builder conversations that
// have not produced their Agent or Team yet.
func (c *KernelHTTPClient) ListAgentBuilderDrafts(ctx context.Context, scope models.Scope, limit, offset int) (*kernelapi.ListAgentBuilderDraftsResponse, error) {
	query := scopeQuery(scope)
	if limit > 0 {
		query.Set("limit", strconv.Itoa(limit))
	}
	if offset > 0 {
		query.Set("offset", strconv.Itoa(offset))
	}
	var result kernelapi.ListAgentBuilderDraftsResponse
	if err := c.do(ctx, http.MethodGet, "/api/v1/agent-builder/drafts?"+query.Encode(), nil, "", &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *KernelHTTPClient) CreateObjective(ctx context.Context, request kernelapi.CreateObjectiveRequest, idempotencyKey string) (*models.Objective, error) {
	var objective models.Objective
	if err := c.do(ctx, http.MethodPost, "/api/v1/objectives", request, idempotencyKey, &objective); err != nil {
		return nil, err
	}
	return &objective, nil
}

func (c *KernelHTTPClient) ListObjectives(ctx context.Context, filter models.ObjectiveFilter) ([]*models.Objective, error) {
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
	var objectives []*models.Objective
	if err := c.do(ctx, http.MethodGet, "/api/v1/objectives?"+query.Encode(), nil, "", &objectives); err != nil {
		return nil, err
	}
	return objectives, nil
}

func (c *KernelHTTPClient) GetObjective(ctx context.Context, scope models.Scope, objectiveID string) (*kernelapi.ObjectiveDetail, error) {
	query := scopeQuery(scope)
	var detail kernelapi.ObjectiveDetail
	path := "/api/v1/objectives/" + url.PathEscape(strings.TrimSpace(objectiveID)) + "?" + query.Encode()
	if err := c.do(ctx, http.MethodGet, path, nil, "", &detail); err != nil {
		return nil, err
	}
	return &detail, nil
}

func (c *KernelHTTPClient) UpdateObjective(ctx context.Context, scope models.Scope, objectiveID string, request kernelapi.UpdateObjectiveRequest) (*models.Objective, error) {
	query := scopeQuery(scope)
	var objective models.Objective
	path := "/api/v1/objectives/" + url.PathEscape(strings.TrimSpace(objectiveID)) + "?" + query.Encode()
	if err := c.do(ctx, http.MethodPut, path, request, "", &objective); err != nil {
		return nil, err
	}
	return &objective, nil
}

func (c *KernelHTTPClient) ListRunbooks(ctx context.Context, filter models.RunbookActivationFilter) ([]*models.RunbookActivation, error) {
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
	var values []*models.RunbookActivation
	if err := c.do(ctx, http.MethodGet, "/api/v1/runbooks?"+query.Encode(), nil, "", &values); err != nil {
		return nil, err
	}
	return values, nil
}

func (c *KernelHTTPClient) GetRunbook(ctx context.Context, scope models.Scope, activationID string) (*routine.RunbookDetail, error) {
	query := scopeQuery(scope)
	var value routine.RunbookDetail
	path := "/api/v1/runbooks/" + url.PathEscape(strings.TrimSpace(activationID)) + "?" + query.Encode()
	if err := c.do(ctx, http.MethodGet, path, nil, "", &value); err != nil {
		return nil, err
	}
	return &value, nil
}

func (c *KernelHTTPClient) UpdateRunbook(ctx context.Context, scope models.Scope, activationID string, request routine.UpdateRunbookActivationRequest) (*models.RunbookActivation, error) {
	query := scopeQuery(scope)
	var value models.RunbookActivation
	path := "/api/v1/runbooks/" + url.PathEscape(strings.TrimSpace(activationID)) + "?" + query.Encode()
	if err := c.do(ctx, http.MethodPatch, path, request, "", &value); err != nil {
		return nil, err
	}
	return &value, nil
}

func (c *KernelHTTPClient) StartRunbook(ctx context.Context, scope models.Scope, activationID string, request routine.StartRunbookActivationRequest) (*agentrun.CommandResult, error) {
	query := scopeQuery(scope)
	var value agentrun.CommandResult
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

func (c *KernelHTTPClient) CreateExternalConversationGateway(
	ctx context.Context,
	request kernelapi.CreateExternalConversationGatewayRequest,
) (*models.ExternalConversationGatewayRegistration, error) {
	var value models.ExternalConversationGatewayRegistration
	if err := c.do(ctx, http.MethodPost, "/api/v1/conversation-gateways", request, "", &value); err != nil {
		return nil, err
	}
	return &value, nil
}

func (c *KernelHTTPClient) ListExternalConversationGateways(
	ctx context.Context,
	filter models.ExternalConversationGatewayFilter,
) ([]*models.ExternalConversationGatewayRegistration, error) {
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
	var values []*models.ExternalConversationGatewayRegistration
	if err := c.do(ctx, http.MethodGet, "/api/v1/conversation-gateways?"+query.Encode(), nil, "", &values); err != nil {
		return nil, err
	}
	return values, nil
}

func (c *KernelHTTPClient) GetExternalConversationGateway(
	ctx context.Context,
	scope models.Scope,
	id string,
) (*models.ExternalConversationGatewayRegistration, error) {
	var value models.ExternalConversationGatewayRegistration
	path := "/api/v1/conversation-gateways/" + url.PathEscape(strings.TrimSpace(id)) + "?" + scopeQuery(scope).Encode()
	if err := c.do(ctx, http.MethodGet, path, nil, "", &value); err != nil {
		return nil, err
	}
	return &value, nil
}

func (c *KernelHTTPClient) UpdateExternalConversationGateway(
	ctx context.Context,
	scope models.Scope,
	id string,
	request kernelapi.UpdateExternalConversationGatewayRequest,
) (*models.ExternalConversationGatewayRegistration, error) {
	var value models.ExternalConversationGatewayRegistration
	path := "/api/v1/conversation-gateways/" + url.PathEscape(strings.TrimSpace(id)) + "?" + scopeQuery(scope).Encode()
	if err := c.do(ctx, http.MethodPatch, path, request, "", &value); err != nil {
		return nil, err
	}
	return &value, nil
}

func (c *KernelHTTPClient) ListActivity(ctx context.Context, request activity.FeedRequest) (*activity.FeedPage, error) {
	query := scopeQuery(request.Scope)
	for _, item := range []struct{ key, value string }{
		{"runId", request.RunID}, {"agentId", request.AgentID}, {"objectiveId", request.ObjectiveID}, {"teamId", request.TeamID}, {"cursor", request.Cursor},
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
	var page activity.FeedPage
	if err := c.do(ctx, http.MethodGet, "/api/v1/activity?"+query.Encode(), nil, "", &page); err != nil {
		return nil, err
	}
	return &page, nil
}

func (c *KernelHTTPClient) CreateAgentRun(ctx context.Context, request kernelapi.CreateAgentRunRequest, idempotencyKey string) (*agentrun.CommandResult, error) {
	var result agentrun.CommandResult
	if err := c.do(ctx, http.MethodPost, "/api/v1/agent-runs", request, idempotencyKey, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *KernelHTTPClient) ListAgentRuns(ctx context.Context, filter models.AgentRunFilter) ([]*models.AgentRun, error) {
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
	var runs []*models.AgentRun
	if err := c.do(ctx, http.MethodGet, "/api/v1/agent-runs?"+query.Encode(), nil, "", &runs); err != nil {
		return nil, err
	}
	return runs, nil
}

func (c *KernelHTTPClient) InspectAgentRunAdmission(ctx context.Context, request agentrun.ClaimRequest) (*models.AgentRunAdmissionDecision, error) {
	query := scopeQuery(request.Scope)
	setIfPresent(query, "kind", string(request.Kind))
	setIfPresent(query, "assignedAgentId", request.AssignedAgentID)
	for key, value := range map[string]int{
		"maxActiveForAgent": request.MaxActiveForAgent, "maxActiveForOwner": request.MaxActiveForOwner,
		"maxActiveForObjective": request.MaxActiveForObjective, "maxActiveForConcurrencyKey": request.MaxActiveForConcurrencyKey,
	} {
		if value > 0 {
			query.Set(key, strconv.Itoa(value))
		}
	}
	var decision models.AgentRunAdmissionDecision
	if err := c.do(ctx, http.MethodGet, "/api/v1/agent-runs/admission?"+query.Encode(), nil, "", &decision); err != nil {
		return nil, err
	}
	return &decision, nil
}

func (c *KernelHTTPClient) GetAgentRun(ctx context.Context, scope models.Scope, runID string) (*models.AgentRun, error) {
	query := scopeQuery(scope)
	var run models.AgentRun
	path := "/api/v1/agent-runs/" + url.PathEscape(strings.TrimSpace(runID)) + "?" + query.Encode()
	if err := c.do(ctx, http.MethodGet, path, nil, "", &run); err != nil {
		return nil, err
	}
	return &run, nil
}

func (c *KernelHTTPClient) GetRunbookExecutionAudit(ctx context.Context, scope models.Scope, runID string) (*routine.RunbookExecutionAudit, error) {
	query := scopeQuery(scope)
	var value routine.RunbookExecutionAudit
	path := "/api/v1/agent-runs/" + url.PathEscape(strings.TrimSpace(runID)) + "/runbook-audit?" + query.Encode()
	if err := c.do(ctx, http.MethodGet, path, nil, "", &value); err != nil {
		return nil, err
	}
	return &value, nil
}

func (c *KernelHTTPClient) GetRunExecutionAudit(ctx context.Context, scope models.Scope, runID string) (*routine.RunExecutionAudit, error) {
	query := scopeQuery(scope)
	var value routine.RunExecutionAudit
	path := "/api/v1/agent-runs/" + url.PathEscape(strings.TrimSpace(runID)) + "/execution-audit?" + query.Encode()
	if err := c.do(ctx, http.MethodGet, path, nil, "", &value); err != nil {
		return nil, err
	}
	return &value, nil
}

func (c *KernelHTTPClient) CommandAgentRun(ctx context.Context, scope models.Scope, runID string, request kernelapi.AgentRunCommandRequest) (*agentrun.CommandResult, error) {
	query := scopeQuery(scope)
	var result agentrun.CommandResult
	path := "/api/v1/agent-runs/" + url.PathEscape(strings.TrimSpace(runID)) + "/commands?" + query.Encode()
	if err := c.do(ctx, http.MethodPost, path, request, "", &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *KernelHTTPClient) CreateAgentRequest(ctx context.Context, request kernelapi.CreateAgentRequestRequest, idempotencyKey string) (*collaboration.AgentRequestResult, error) {
	var result collaboration.AgentRequestResult
	if err := c.do(ctx, http.MethodPost, "/api/v1/agent-requests", request, idempotencyKey, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *KernelHTTPClient) ListAgentRequests(ctx context.Context, filter models.AgentRequestFilter) ([]*models.AgentRequest, error) {
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
	var requests []*models.AgentRequest
	if err := c.do(ctx, http.MethodGet, "/api/v1/agent-requests?"+query.Encode(), nil, "", &requests); err != nil {
		return nil, err
	}
	return requests, nil
}

func (c *KernelHTTPClient) GetAgentRequest(ctx context.Context, scope models.Scope, requestID string) (*models.AgentRequest, error) {
	var request models.AgentRequest
	path := "/api/v1/agent-requests/" + url.PathEscape(strings.TrimSpace(requestID)) + "?" + scopeQuery(scope).Encode()
	if err := c.do(ctx, http.MethodGet, path, nil, "", &request); err != nil {
		return nil, err
	}
	return &request, nil
}

func (c *KernelHTTPClient) RespondAgentRequest(ctx context.Context, scope models.Scope, requestID string, request kernelapi.RespondAgentRequestRequest) (*collaboration.AgentRequestResult, error) {
	var result collaboration.AgentRequestResult
	path := "/api/v1/agent-requests/" + url.PathEscape(strings.TrimSpace(requestID)) + "/responses?" + scopeQuery(scope).Encode()
	if err := c.do(ctx, http.MethodPost, path, request, "", &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *KernelHTTPClient) CompleteAgentRequest(ctx context.Context, scope models.Scope, requestID string, request kernelapi.CompleteAgentRequestRequest, idempotencyKey string) (*collaboration.AgentRequestResult, error) {
	var result collaboration.AgentRequestResult
	path := "/api/v1/agent-requests/" + url.PathEscape(strings.TrimSpace(requestID)) + "/completions?" + scopeQuery(scope).Encode()
	if err := c.do(ctx, http.MethodPost, path, request, idempotencyKey, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *KernelHTTPClient) ReviewAgentRequestCompletion(ctx context.Context, scope models.Scope, requestID string, request kernelapi.ReviewAgentRequestCompletionRequest, idempotencyKey string) (*collaboration.AgentRequestResult, error) {
	var result collaboration.AgentRequestResult
	path := "/api/v1/agent-requests/" + url.PathEscape(strings.TrimSpace(requestID)) + "/completion-review?" + scopeQuery(scope).Encode()
	if err := c.do(ctx, http.MethodPost, path, request, idempotencyKey, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *KernelHTTPClient) ListActionCalls(ctx context.Context, filter models.ActionFilter) ([]*models.ActionCall, error) {
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
	var calls []*models.ActionCall
	if err := c.do(ctx, http.MethodGet, "/api/v1/action-calls?"+query.Encode(), nil, "", &calls); err != nil {
		return nil, err
	}
	return calls, nil
}

func (c *KernelHTTPClient) GetActionCall(ctx context.Context, scope models.Scope, actionID string) (*models.ActionCall, error) {
	var call models.ActionCall
	path := "/api/v1/action-calls/" + url.PathEscape(strings.TrimSpace(actionID)) + "?" + scopeQuery(scope).Encode()
	if err := c.do(ctx, http.MethodGet, path, nil, "", &call); err != nil {
		return nil, err
	}
	return &call, nil
}

// ListAgentTurns lists the model steps of one Run, agent-turns v2.
func (c *KernelHTTPClient) ListAgentTurns(ctx context.Context, filter models.AgentTurnFilter) ([]*kernelapi.AgentTurnRecord, error) {
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

// GetAgentTurn reads one model step, agent-turns v2.
func (c *KernelHTTPClient) GetAgentTurn(ctx context.Context, scope models.Scope, turnID string) (*kernelapi.AgentTurnRecord, error) {
	var turn kernelapi.AgentTurnRecord
	path := "/api/v1/agent-turns/" + url.PathEscape(strings.TrimSpace(turnID)) + "?" + scopeQuery(scope).Encode()
	if err := c.do(ctx, http.MethodGet, path, nil, "", &turn); err != nil {
		return nil, err
	}
	return &turn, nil
}

func (c *KernelHTTPClient) ListActionApprovals(ctx context.Context, filter models.ApprovalFilter) ([]*models.ApprovalCheckpoint, error) {
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
	var approvals []*models.ApprovalCheckpoint
	if err := c.do(ctx, http.MethodGet, "/api/v1/action-approvals?"+query.Encode(), nil, "", &approvals); err != nil {
		return nil, err
	}
	return approvals, nil
}

func (c *KernelHTTPClient) GetActionApproval(ctx context.Context, scope models.Scope, approvalID string) (*models.ApprovalCheckpoint, error) {
	var approval models.ApprovalCheckpoint
	path := "/api/v1/action-approvals/" + url.PathEscape(strings.TrimSpace(approvalID)) + "?" + scopeQuery(scope).Encode()
	if err := c.do(ctx, http.MethodGet, path, nil, "", &approval); err != nil {
		return nil, err
	}
	return &approval, nil
}

func (c *KernelHTTPClient) ResolveActionApproval(ctx context.Context, scope models.Scope, approvalID string, request kernelapi.ResolveActionApprovalRequest, idempotencyKey string) (*models.ApprovalResolutionResult, error) {
	var result models.ApprovalResolutionResult
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
	return c.ListSkillBindings(ctx, scope, SkillBindingOwner{Type: models.OwnerTypeTeam, DeploymentID: deploymentID})
}

func skillBindingOwnerPath(owner SkillBindingOwner) (string, error) {
	id := strings.TrimSpace(owner.DeploymentID)
	if id == "" {
		return "", errors.New("deployment id is required")
	}
	switch owner.Type {
	case models.OwnerTypeAgent:
		return "/api/v1/agent-deployments/" + url.PathEscape(id), nil
	case models.OwnerTypeTeam:
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
	return c.UpsertSkillBinding(ctx, SkillBindingOwner{Type: models.OwnerTypeTeam, DeploymentID: deploymentID}, request)
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
	return c.DisableSkillBinding(ctx, SkillBindingOwner{Type: models.OwnerTypeTeam, DeploymentID: deploymentID}, request)
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

func (c *KernelHTTPClient) PlanTeamSkillReferenceUpgrade(ctx context.Context, deploymentID string, request skillbinding.PlanSkillReferenceUpgradeRequest) (*models.SkillReferenceUpgradePlan, error) {
	return c.PlanSkillReferenceUpgrade(ctx, SkillBindingOwner{Type: models.OwnerTypeTeam, DeploymentID: deploymentID}, request)
}

func (c *KernelHTTPClient) PlanSkillReferenceUpgrade(ctx context.Context, owner SkillBindingOwner, request skillbinding.PlanSkillReferenceUpgradeRequest) (*models.SkillReferenceUpgradePlan, error) {
	var result models.SkillReferenceUpgradePlan
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

func (c *KernelHTTPClient) ApplyTeamSkillReferenceUpgrade(ctx context.Context, deploymentID string, request skillbinding.ApplySkillReferenceUpgradeRequest) (*models.SkillReferenceUpgradeReceipt, error) {
	return c.ApplySkillReferenceUpgrade(ctx, SkillBindingOwner{Type: models.OwnerTypeTeam, DeploymentID: deploymentID}, request)
}

func (c *KernelHTTPClient) ApplySkillReferenceUpgrade(ctx context.Context, owner SkillBindingOwner, request skillbinding.ApplySkillReferenceUpgradeRequest) (*models.SkillReferenceUpgradeReceipt, error) {
	if request.Plan == nil {
		return nil, errors.New("reviewed Skill reference upgrade plan is required")
	}
	var result models.SkillReferenceUpgradeReceipt
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

func scopeQuery(scope models.Scope) url.Values {
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
