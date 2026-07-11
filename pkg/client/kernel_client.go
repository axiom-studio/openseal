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

	"github.com/axiom-studio/openseal/pkg/authoring"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/kernelapi"
	"github.com/axiom-studio/openseal/pkg/runtime"
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
	CreateInitiative(context.Context, kernelapi.CreateInitiativeRequest, string) (*runtime.Initiative, error)
	ListInitiatives(context.Context, runtime.InitiativeFilter) ([]*runtime.Initiative, error)
	GetInitiative(context.Context, runtime.Scope, string) (*runtime.Initiative, error)
	PatchInitiative(context.Context, runtime.Scope, string, kernelapi.UpdateInitiativeRequest) (*runtime.Initiative, error)
	CreateAgentRun(context.Context, kernelapi.CreateAgentRunRequest, string) (*runtime.AgentRunCommandResult, error)
	ListAgentRuns(context.Context, runtime.AgentRunFilter) ([]*runtime.AgentRun, error)
	GetAgentRun(context.Context, runtime.Scope, string) (*runtime.AgentRun, error)
	CommandAgentRun(context.Context, runtime.Scope, string, kernelapi.AgentRunCommandRequest) (*runtime.AgentRunCommandResult, error)
	CompileWorkforce(context.Context, authoring.GenerateRequest) (*authoring.CompileResult, error)
	CreateWorkforceChangeSet(context.Context, authoring.CreateChangeSetRequest, string) (*authoring.ChangeSet, error)
	GetWorkforceChangeSet(context.Context, capability.ScopeReference, string) (*authoring.ChangeSet, error)
	RetryWorkforceChangeSetGeneration(context.Context, authoring.RetryChangeSetGenerationRequest, string) (*authoring.ChangeSet, error)
	EvaluateWorkforceChangeSet(context.Context, authoring.SubmitChangeSetEvaluationRequest, string) (*authoring.ChangeSet, error)
	ResolveWorkforceChangeSetApproval(context.Context, authoring.ResolveChangeSetApprovalRequest, string) (*authoring.ChangeSet, error)
	ApplyWorkforceChangeSet(context.Context, authoring.ApplyChangeSetRequest, string) (*authoring.ChangeSet, error)
}

type TeamClient interface {
	RegisterTeamDefinition(context.Context, *kernelteam.Definition) (*kernelteam.Definition, error)
	GetTeamDefinition(context.Context, string, string) (*kernelteam.Definition, error)
	ListTeamDefinitionVersions(context.Context, string) ([]*kernelteam.Definition, error)
	CreateTeamDeployment(context.Context, kernelapi.CreateTeamDeploymentRequest) (*kernelapi.TeamDeploymentResult, error)
	GetTeamDeployment(context.Context, capability.ScopeReference, string) (*kernelteam.Deployment, error)
	UpdateTeamDeployment(context.Context, string, kernelapi.UpdateTeamDeploymentRequest) (*kernelapi.TeamDeploymentResult, error)
	ActivateTeamDefinition(context.Context, string, kernelapi.ActivateTeamDefinitionRequest) (*kernelapi.TeamDeploymentResult, error)
	ListTeamDefinitionActivations(context.Context, capability.ScopeReference, string) ([]workforce.DefinitionActivation, error)
}

type KernelHTTPClient struct {
	baseURL    string
	httpClient *http.Client
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

func NewKernelHTTPClient(baseURL string, httpClient *http.Client) *KernelHTTPClient {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = DefaultKernelBaseURL
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &KernelHTTPClient{baseURL: baseURL, httpClient: httpClient}
}

func (c *KernelHTTPClient) Capabilities(ctx context.Context) (kernelapi.CapabilityDocument, error) {
	var document kernelapi.CapabilityDocument
	err := c.do(ctx, http.MethodGet, "/api/v1/capabilities", nil, "", &document)
	return document, err
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

func (c *KernelHTTPClient) EvaluateWorkforceChangeSet(ctx context.Context, request authoring.SubmitChangeSetEvaluationRequest, idempotencyKey string) (*authoring.ChangeSet, error) {
	return c.mutateWorkforceChangeSet(ctx, request.ChangeSetID, "evaluations", request, idempotencyKey)
}

func (c *KernelHTTPClient) RetryWorkforceChangeSetGeneration(ctx context.Context, request authoring.RetryChangeSetGenerationRequest, idempotencyKey string) (*authoring.ChangeSet, error) {
	return c.mutateWorkforceChangeSet(ctx, request.ChangeSetID, "retry", request, idempotencyKey)
}

func (c *KernelHTTPClient) ResolveWorkforceChangeSetApproval(ctx context.Context, request authoring.ResolveChangeSetApprovalRequest, idempotencyKey string) (*authoring.ChangeSet, error) {
	return c.mutateWorkforceChangeSet(ctx, request.ChangeSetID, "approvals", request, idempotencyKey)
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

func (c *KernelHTTPClient) CreateInitiative(ctx context.Context, request kernelapi.CreateInitiativeRequest, idempotencyKey string) (*runtime.Initiative, error) {
	var initiative runtime.Initiative
	if err := c.do(ctx, http.MethodPost, "/api/v1/initiatives", request, idempotencyKey, &initiative); err != nil {
		return nil, err
	}
	return &initiative, nil
}

func (c *KernelHTTPClient) ListInitiatives(ctx context.Context, filter runtime.InitiativeFilter) ([]*runtime.Initiative, error) {
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
	var initiatives []*runtime.Initiative
	if err := c.do(ctx, http.MethodGet, "/api/v1/initiatives?"+query.Encode(), nil, "", &initiatives); err != nil {
		return nil, err
	}
	return initiatives, nil
}

func (c *KernelHTTPClient) GetInitiative(ctx context.Context, scope runtime.Scope, initiativeID string) (*runtime.Initiative, error) {
	query := scopeQuery(scope)
	var initiative runtime.Initiative
	path := "/api/v1/initiatives/" + url.PathEscape(strings.TrimSpace(initiativeID)) + "?" + query.Encode()
	if err := c.do(ctx, http.MethodGet, path, nil, "", &initiative); err != nil {
		return nil, err
	}
	return &initiative, nil
}

func (c *KernelHTTPClient) PatchInitiative(ctx context.Context, scope runtime.Scope, initiativeID string, request kernelapi.UpdateInitiativeRequest) (*runtime.Initiative, error) {
	query := scopeQuery(scope)
	var initiative runtime.Initiative
	path := "/api/v1/initiatives/" + url.PathEscape(strings.TrimSpace(initiativeID)) + "?" + query.Encode()
	if err := c.do(ctx, http.MethodPatch, path, request, "", &initiative); err != nil {
		return nil, err
	}
	return &initiative, nil
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

func (c *KernelHTTPClient) CommandAgentRun(ctx context.Context, scope runtime.Scope, runID string, request kernelapi.AgentRunCommandRequest) (*runtime.AgentRunCommandResult, error) {
	query := scopeQuery(scope)
	var result runtime.AgentRunCommandResult
	path := "/api/v1/agent-runs/" + url.PathEscape(strings.TrimSpace(runID)) + "/commands?" + query.Encode()
	if err := c.do(ctx, http.MethodPost, path, request, "", &result); err != nil {
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

func (c *KernelHTTPClient) do(ctx context.Context, method, path string, body interface{}, idempotencyKey string, result interface{}) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode OpenSeal request: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return fmt.Errorf("build OpenSeal request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if idempotencyKey = strings.TrimSpace(idempotencyKey); idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("connect to OpenSeal at %s: %w", c.baseURL, err)
	}
	defer resp.Body.Close()
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return decodeAPIError(resp.StatusCode, decoder)
	}
	if result == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if err := decoder.Decode(result); err != nil {
		return fmt.Errorf("decode OpenSeal response: %w", err)
	}
	return nil
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
