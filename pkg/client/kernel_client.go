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

	"github.com/axiom-studio/openseal/pkg/kernelapi"
	"github.com/axiom-studio/openseal/pkg/runtime"
)

const DefaultKernelBaseURL = "http://127.0.0.1:8080"

// KernelClient is the thin HTTP boundary used by OpenSeal interactive
// surfaces. It deliberately exposes only versioned public kernel operations.
type KernelClient interface {
	ArtifactClient
	Capabilities(context.Context) (kernelapi.CapabilityDocument, error)
	CreateAgentRun(context.Context, kernelapi.CreateAgentRunRequest, string) (*runtime.AgentRunCommandResult, error)
	ListAgentRuns(context.Context, runtime.AgentRunFilter) ([]*runtime.AgentRun, error)
	GetAgentRun(context.Context, runtime.Scope, string) (*runtime.AgentRun, error)
	CommandAgentRun(context.Context, runtime.Scope, string, kernelapi.AgentRunCommandRequest) (*runtime.AgentRunCommandResult, error)
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

func setIfPresent(query url.Values, key, value string) {
	if value = strings.TrimSpace(value); value != "" {
		query.Set(key, value)
	}
}

var _ KernelClient = (*KernelHTTPClient)(nil)
