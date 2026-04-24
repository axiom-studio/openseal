package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"go.uber.org/zap"
)

type SentinelAPIClient struct {
	baseURL    string
	httpClient *http.Client
	logger     *zap.SugaredLogger
}

func NewSentinelAPIClient(baseURL string, logger *zap.SugaredLogger) *SentinelAPIClient {
	return &SentinelAPIClient{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		logger: logger,
	}
}

func (c *SentinelAPIClient) GetInstance(instanceId int) (*InstanceConfig, error) {
	url := fmt.Sprintf("%s/agent/internal/instance/%d", c.baseURL, instanceId)
	resp, err := c.httpClient.Get(url)
	if err != nil {
		return nil, fmt.Errorf("failed to get instance: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("API error (status %d): %s", resp.StatusCode, string(body))
	}

	var instance InstanceConfig
	if err := json.NewDecoder(resp.Body).Decode(&instance); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &instance, nil
}

func (c *SentinelAPIClient) GetVersion(versionId int) (*VersionConfig, error) {
	url := fmt.Sprintf("%s/agent/internal/version/%d", c.baseURL, versionId)
	resp, err := c.httpClient.Get(url)
	if err != nil {
		return nil, fmt.Errorf("failed to get version: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("API error (status %d): %s", resp.StatusCode, string(body))
	}

	var version VersionConfig
	if err := json.NewDecoder(resp.Body).Decode(&version); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &version, nil
}

func (c *SentinelAPIClient) GetEnvironment(envId int) (*EnvironmentConfig, error) {
	url := fmt.Sprintf("%s/agent/internal/environment/%d", c.baseURL, envId)
	resp, err := c.httpClient.Get(url)
	if err != nil {
		return nil, fmt.Errorf("failed to get environment: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("API error (status %d): %s", resp.StatusCode, string(body))
	}

	var env EnvironmentConfig
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &env, nil
}

func (c *SentinelAPIClient) GetBlueprint(name string, environmentId int) (*BlueprintConfig, error) {
	url := fmt.Sprintf("%s/agent/internal/blueprint/%s?environmentId=%d", c.baseURL, name, environmentId)
	resp, err := c.httpClient.Get(url)
	if err != nil {
		return nil, fmt.Errorf("failed to get blueprint: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("API error (status %d): %s", resp.StatusCode, string(body))
	}

	var blueprint BlueprintConfig
	if err := json.NewDecoder(resp.Body).Decode(&blueprint); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &blueprint, nil
}

type InstanceConfig struct {
	Id                    int    `json:"id"`
	AgentLibraryVersionId int    `json:"agentLibraryVersionId"`
	Name                  string `json:"name"`
	EnvironmentId         int    `json:"environmentId"`
	BlueprintInstanceId   int    `json:"blueprintInstanceId"`
	Bindings              string `json:"bindings"`
	Enabled               bool   `json:"enabled"`
	TimeoutSeconds        int    `json:"timeoutSeconds,omitempty"`
}

type VersionConfig struct {
	Id          int    `json:"id"`
	Version     string `json:"version"`
	Nodes       string `json:"nodes"`
	Connections string `json:"connections"`
}

type EnvironmentConfig struct {
	Id        int    `json:"id"`
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	ClusterId int    `json:"clusterId"`
}

type BlueprintConfig struct {
	Id    int                           `json:"id"`
	Name  string                        `json:"name"`
	Nodes map[string]*BlueprintNodeInfo `json:"nodes"`
}

type BlueprintNodeInfo struct {
	Name        string                 `json:"name"`
	ServiceName string                 `json:"serviceName"`
	Outputs     map[string]interface{} `json:"outputs"`
}

type StatusUpdate struct {
	RunId     int       `json:"runId"`
	Status    string    `json:"status"`
	Error     string    `json:"error,omitempty"`
	Timestamp time.Time `json:"timestamp"`
}

type Heartbeat struct {
	ClusterId   int       `json:"clusterId"`
	WorkerCount int       `json:"workerCount"`
	Version     string    `json:"version"`
	Timestamp   time.Time `json:"timestamp"`
}

func (c *SentinelAPIClient) PostStatusUpdate(update *StatusUpdate) error {
	url := fmt.Sprintf("%s/agent/internal/status", c.baseURL)
	data, err := json.Marshal(update)
	if err != nil {
		return fmt.Errorf("failed to marshal status update: %w", err)
	}

	resp, err := c.httpClient.Post(url, "application/json", bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("failed to post status update: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("status update failed (status %d): %s", resp.StatusCode, string(body))
	}

	return nil
}

func (c *SentinelAPIClient) PostHeartbeat(heartbeat *Heartbeat) error {
	url := fmt.Sprintf("%s/agent/internal/heartbeat", c.baseURL)
	data, err := json.Marshal(heartbeat)
	if err != nil {
		return fmt.Errorf("failed to marshal heartbeat: %w", err)
	}

	resp, err := c.httpClient.Post(url, "application/json", bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("failed to post heartbeat: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("heartbeat failed (status %d): %s", resp.StatusCode, string(body))
	}

	return nil
}

type StepRunData struct {
	AgentRunId  int       `json:"agentRunId"`
	StepName    string    `json:"stepName"`
	StepType    string    `json:"stepType"`
	StepIndex   int       `json:"stepIndex"`
	Status      string    `json:"status"`
	Input       string    `json:"input"`
	Output      string    `json:"output"`
	Error       string    `json:"error,omitempty"`
	StartedAt   time.Time `json:"startedAt"`
	CompletedAt time.Time `json:"completedAt"`
}

func (c *SentinelAPIClient) PostStepRuns(stepRuns []*StepRunData) error {
	if len(stepRuns) == 0 {
		return nil
	}

	url := fmt.Sprintf("%s/agent/internal/step-runs", c.baseURL)
	data, err := json.Marshal(stepRuns)
	if err != nil {
		return fmt.Errorf("failed to marshal step runs: %w", err)
	}

	resp, err := c.httpClient.Post(url, "application/json", bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("failed to post step runs: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("step runs save failed (status %d): %s", resp.StatusCode, string(body))
	}

	return nil
}

type ClusterConfig struct {
	Id                int    `json:"id"`
	ClusterName       string `json:"clusterName"`
	ServerUrl         string `json:"serverUrl"`
	BearerToken       string `json:"bearerToken"`
	Active            bool   `json:"active"`
	InsecureSkipTLS   bool   `json:"insecureSkipTLS"`
	CertificateData   string `json:"certificateData"`
	KeyData           string `json:"keyData"`
	CertAuthorityData string `json:"certAuthorityData"`
}

func (c *SentinelAPIClient) GetClusterConfig(clusterId int) (*ClusterConfig, error) {
	url := fmt.Sprintf("%s/agent/internal/cluster/%d", c.baseURL, clusterId)
	resp, err := c.httpClient.Get(url)
	if err != nil {
		return nil, fmt.Errorf("failed to get cluster config: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("API error (status %d): %s", resp.StatusCode, string(body))
	}

	var cluster ClusterConfig
	if err := json.NewDecoder(resp.Body).Decode(&cluster); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &cluster, nil
}

type ResolveVaultCredentialFieldRequest struct {
	CredentialID int    `json:"credentialId"`
	Field        string `json:"field"`
}

type ResolveVaultCredentialFieldResponse struct {
	Value string `json:"value"`
}

func (c *SentinelAPIClient) ResolveVaultCredentialField(ctx context.Context, credentialID int, field string) (string, error) {
	url := fmt.Sprintf("%s/vault/internal/resolve-field", c.baseURL)

	reqBody := ResolveVaultCredentialFieldRequest{
		CredentialID: credentialID,
		Field:        field,
	}

	data, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("failed to marshal request: %w", err)
	}

	resp, err := c.httpClient.Post(url, "application/json", bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("failed to resolve vault field: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("vault resolution failed (status %d): %s", resp.StatusCode, string(body))
	}

	var response ResolveVaultCredentialFieldResponse
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		return "", fmt.Errorf("failed to decode response: %w", err)
	}

	return response.Value, nil
}

type AgentSkillRepository struct {
	Id          int    `json:"Id"`
	Name        string `json:"Name"`
	Url         string `json:"Url"`
	Description string `json:"Description"`
	AuthType    string `json:"AuthType"`
	IsEnabled   bool   `json:"IsEnabled"`
	IsDefault   bool   `json:"IsDefault"`
}

func (c *SentinelAPIClient) GetSkillRepos() ([]*AgentSkillRepository, error) {
	url := fmt.Sprintf("%s/agent/skills/list", c.baseURL)
	resp, err := c.httpClient.Get(url)
	if err != nil {
		return nil, fmt.Errorf("failed to get skill repos: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("API error (status %d): %s", resp.StatusCode, string(body))
	}

	var response struct {
		Code   int                    `json:"code"`
		Status string                 `json:"status"`
		Result []*AgentSkillRepository `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return response.Result, nil
}
