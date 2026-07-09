package trigger

import (
	"os"

	envRepository "github.com/axiom-studio/openseal/pkg/environment"
	"github.com/axiom-studio/openseal/pkg/repository"
	"go.uber.org/zap"
)

// NewDefaultRegistry creates a registry with default triggers (without K8s triggers)
func NewDefaultRegistry(baseURL string) (*Registry, error) {
	r := NewRegistry()

	triggers := []Trigger{
		NewWebhookTrigger(baseURL),
		NewCronTrigger(),
		NewManualTrigger(),
	}

	for _, t := range triggers {
		if err := r.Register(t); err != nil {
			return nil, err
		}
	}

	return r, nil
}

// NewDefaultRegistryWithRepository creates a registry with database-backed triggers
func NewDefaultRegistryWithRepository(baseURL string, triggerRepo repository.AgentTriggerRepository) (*Registry, error) {
	r := NewRegistry()

	triggers := []Trigger{
		NewWebhookTriggerWithRepository(baseURL, triggerRepo),
		NewCronTrigger(),
		NewManualTrigger(),
	}

	for _, t := range triggers {
		if err := r.Register(t); err != nil {
			return nil, err
		}
	}

	return r, nil
}

func NewDefaultRegistryWithHTTP(
	baseURL string,
	resourceAPIBaseURL string,
	triggerRepo repository.AgentTriggerRepository,
	instanceRepo repository.AgentInstanceRepository,
	environmentRepo envRepository.EnvironmentRepository,
	logger *zap.SugaredLogger,
) (*Registry, error) {
	r := NewRegistry()

	k8sClient := NewK8sEventClient(resourceAPIBaseURL, logger)

	triggers := []Trigger{
		NewWebhookTriggerWithRepository(baseURL, triggerRepo),
		NewCronTrigger(),
		NewManualTrigger(),
		NewK8sEventTriggerHTTP(k8sClient, instanceRepo, environmentRepo, logger),
		NewK8sWatchTriggerHTTP(k8sClient, instanceRepo, environmentRepo, logger),
	}

	for _, t := range triggers {
		if err := r.Register(t); err != nil {
			return nil, err
		}
	}

	return r, nil
}

// NewDefaultRegistryFromEnv creates a registry using environment variables
func NewDefaultRegistryFromEnv() (*Registry, error) {
	baseURL := os.Getenv("AGENT_BASE_URL")
	if baseURL == "" {
		baseURL = "http://localhost:8085"
	}
	return NewDefaultRegistry(baseURL)
}

// NewDefaultRegistryFromEnvWithRepository creates a registry with repo support using env
func NewDefaultRegistryFromEnvWithRepository(triggerRepo repository.AgentTriggerRepository) (*Registry, error) {
	baseURL := os.Getenv("AGENT_BASE_URL")
	if baseURL == "" {
		baseURL = "http://localhost:8085"
	}
	return NewDefaultRegistryWithRepository(baseURL, triggerRepo)
}
