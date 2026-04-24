package client

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/caarlos0/env"
	"go.uber.org/zap"
)

// RemoteClient provides access to OpenSeal APIs from OpenSeal
type RemoteClient interface {
	GetBlueprintInstanceNodeOutputs(instanceId int) (map[int]map[string]interface{}, error)
}

type RemoteClientConfig struct {
	BaseUrl string `env:"OPENSEAL_BASE_URL" envDefault:"http://openseal-service:80"`
	Timeout int    `env:"OPENSEAL_CLIENT_TIMEOUT" envDefault:"30"`
}

func GetRemoteClientConfig() (*RemoteClientConfig, error) {
	cfg := &RemoteClientConfig{}
	err := env.Parse(cfg)
	return cfg, err
}

type RemoteClientImpl struct {
	logger     *zap.SugaredLogger
	config     *RemoteClientConfig
	httpClient *http.Client
}

func NewRemoteClientImpl(
	logger *zap.SugaredLogger,
	config *RemoteClientConfig,
) *RemoteClientImpl {
	return &RemoteClientImpl{
		logger: logger,
		config: config,
		httpClient: &http.Client{
			Timeout: time.Duration(config.Timeout) * time.Second,
		},
	}
}

// apiResponse wraps the standard OpenSeal API response format
type apiResponse struct {
	Code   int             `json:"code"`
	Status string          `json:"status"`
	Result json.RawMessage `json:"result"`
	Error  string          `json:"error,omitempty"`
}

// GetBlueprintInstanceNodeOutputs fetches node outputs for a blueprint instance from OpenSeal
func (impl *RemoteClientImpl) GetBlueprintInstanceNodeOutputs(instanceId int) (map[int]map[string]interface{}, error) {
	url := fmt.Sprintf("%s/orchestrator/blueprint/instance/%d/node-outputs", impl.config.BaseUrl, instanceId)

	impl.logger.Debugw("fetching blueprint instance node outputs", "url", url, "instanceId", instanceId)

	resp, err := impl.httpClient.Get(url)
	if err != nil {
		return nil, fmt.Errorf("error calling openseal: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("error reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("openseal returned status %d: %s", resp.StatusCode, string(body))
	}

	var response apiResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("error parsing response: %w", err)
	}

	if response.Error != "" {
		return nil, fmt.Errorf("openseal error: %s", response.Error)
	}

	// Parse the result map - keys are string representations of int IDs
	var rawResult map[string]map[string]interface{}
	if err := json.Unmarshal(response.Result, &rawResult); err != nil {
		return nil, fmt.Errorf("error parsing result: %w", err)
	}

	// Convert string keys to int keys
	result := make(map[int]map[string]interface{})
	for key, value := range rawResult {
		var intKey int
		if _, err := fmt.Sscanf(key, "%d", &intKey); err == nil {
			result[intKey] = value
		}
	}

	return result, nil
}
