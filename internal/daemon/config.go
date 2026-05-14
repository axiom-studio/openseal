package daemon

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// DaemonConfig is the top-level configuration for the daemon,
// loaded from a YAML file (typically daemon.yaml).
type DaemonConfig struct {
	// WorkflowsDir is the directory containing .hcl/.wf workflow files.
	WorkflowsDir string `yaml:"workflowsDir"`

	// Triggers holds all trigger configurations keyed by trigger type.
	Triggers TriggerConfigs `yaml:"triggers"`

	// Webhook server settings (used when a webhook trigger is present).
	Webhook WebhookConfig `yaml:"webhook"`

	// API server settings for the web GUI.
	API APIConfig `yaml:"api"`

	// LogLevel controls verbosity: "debug", "info", "warn", "error".
	LogLevel string `yaml:"logLevel"`
}

// APIConfig configures the HTTP API server for the web GUI.
type APIConfig struct {
	// ListenAddr is the host:port the API server binds to.
	ListenAddr string `yaml:"listenAddr"`
}

// TriggerConfigs groups trigger definitions by type.
type TriggerConfigs struct {
	// Cron triggers, keyed by an arbitrary name.
	Cron map[string]CronTriggerConfig `yaml:"cron,omitempty"`

	// Webhook triggers, keyed by an arbitrary name.
	Webhook map[string]WebhookTriggerConfig `yaml:"webhook,omitempty"`
}

// CronTriggerConfig defines a single cron trigger.
type CronTriggerConfig struct {
	// Expression is the cron expression (with seconds field).
	Expression string `yaml:"expression"`

	// Workflow is the filename of the workflow to execute (within WorkflowsDir).
	Workflow string `yaml:"workflow"`
}

// WebhookTriggerConfig defines a single webhook trigger.
type WebhookTriggerConfig struct {
	// Path is the URL path segment for this webhook (auto-generated if empty).
	Path string `yaml:"path,omitempty"`

	// Workflow is the filename of the workflow to execute (within WorkflowsDir).
	Workflow string `yaml:"workflow"`

	// Secret is an optional shared-secret for validating webhook payloads.
	Secret string `yaml:"secret,omitempty"`
}

// WebhookConfig configures the HTTP server for receiving webhooks.
type WebhookConfig struct {
	// ListenAddr is the host:port the webhook server binds to.
	ListenAddr string `yaml:"listenAddr"`

	// BaseURL is the externally-reachable URL used to build webhook URLs.
	BaseURL string `yaml:"baseURL"`
}

// LoadDaemonConfig reads and validates a daemon config file.
func LoadDaemonConfig(filepath string) (*DaemonConfig, error) {
	data, err := os.ReadFile(filepath)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	var cfg DaemonConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	if cfg.LogLevel == "" {
		cfg.LogLevel = "info"
	}
	if cfg.Webhook.ListenAddr == "" {
		cfg.Webhook.ListenAddr = ":9090"
	}
	if cfg.Webhook.BaseURL == "" {
		cfg.Webhook.BaseURL = "http://localhost:9090"
	}
	if cfg.API.ListenAddr == "" {
		cfg.API.ListenAddr = ":8080"
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// Validate checks that required fields are present and trigger configs are valid.
func (c *DaemonConfig) Validate() error {
	if c.WorkflowsDir == "" {
		return fmt.Errorf("workflowsDir is required")
	}

	for name, ct := range c.Triggers.Cron {
		if ct.Expression == "" {
			return fmt.Errorf("cron trigger %q: expression is required", name)
		}
		if ct.Workflow == "" {
			return fmt.Errorf("cron trigger %q: workflow is required", name)
		}
	}

	for name, wt := range c.Triggers.Webhook {
		if wt.Workflow == "" {
			return fmt.Errorf("webhook trigger %q: workflow is required", name)
		}
	}

	return nil
}