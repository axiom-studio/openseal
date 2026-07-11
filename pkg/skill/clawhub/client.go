package clawhub

import (
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	RegistryURL    = "https://clawhub.ai"
	defaultBaseURL = RegistryURL + "/api/v1"
	defaultTimeout = 30 * time.Second
	maxRetries     = 2
	envRegistryURL = "CLAWHUB_REGISTRY"
)

type ClawHubClient struct {
	baseURL    string
	httpClient *http.Client
}

func NewClawHubClient(baseURL string) *ClawHubClient {
	return NewClawHubClientWithHTTP(baseURL, &http.Client{Timeout: defaultTimeout})
}

func NewClawHubClientWithHTTP(baseURL string, httpClient *http.Client) *ClawHubClient {
	if baseURL == "" {
		baseURL = os.Getenv(envRegistryURL)
		if baseURL == "" {
			baseURL = defaultBaseURL
		}
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultTimeout}
	}
	return &ClawHubClient{baseURL: strings.TrimRight(baseURL, "/"), httpClient: httpClient}
}
