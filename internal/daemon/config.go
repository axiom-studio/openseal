package daemon

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/axiom-studio/openseal/pkg/runtime"
	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/axiom-studio/openseal/pkg/source"
	"gopkg.in/yaml.v3"
)

// DefaultDaemonConfig returns a sensible default configuration.
func DefaultDaemonConfig() *DaemonConfig {
	return &DaemonConfig{
		LogLevel: "info",
		Storage: StorageConfig{
			Driver:        "sqlite",
			Path:          "data/openseal.db",
			ArtifactsPath: "data/artifacts",
		},
		API: APIConfig{
			ListenAddr: "127.0.0.1:8080",
		},
	}
}

// DaemonConfig is the top-level configuration for the daemon,
// loaded from a YAML file (typically daemon.yaml).
type DaemonConfig struct {
	// API server settings for kernel clients.
	API APIConfig `yaml:"api"`

	// LogLevel controls verbosity: "debug", "info", "warn", "error".
	LogLevel string `yaml:"logLevel"`

	// Storage configures the durable canonical kernel store.
	Storage StorageConfig `yaml:"storage"`

	// SourcePolicies are credential-free, scope-bound network authorities.
	// No source access or outreach delivery is exposed for an unlisted scope.
	SourcePolicies []ScopedSourcePolicy `yaml:"sourcePolicies,omitempty"`
}

type ScopedSourcePolicy struct {
	Scope  runtime.Scope `yaml:"scope"`
	Policy source.Policy `yaml:"policy"`
}

// StorageConfig configures standalone OpenSeal persistence. SQLite is the
// portable durable store; paths are resolved relative to the daemon config.
type StorageConfig struct {
	Driver        string `yaml:"driver"`
	Path          string `yaml:"path"`
	ArtifactsPath string `yaml:"artifactsPath"`
}

// APIConfig configures the HTTP API server used by the TUI and integrations.
type APIConfig struct {
	// ListenAddr is the host:port the API server binds to.
	ListenAddr string `yaml:"listenAddr"`
}

// LoadDaemonConfig reads and validates a daemon config file.
// If the file does not exist, a default config is written and returned.
func LoadDaemonConfig(path string) (*DaemonConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			cfg := DefaultDaemonConfig()
			if err := WriteDaemonConfig(path, cfg); err != nil {
				return nil, fmt.Errorf("write default config: %w", err)
			}
			return cfg, nil
		}
		return nil, fmt.Errorf("read config: %w", err)
	}

	var cfg DaemonConfig
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	if cfg.LogLevel == "" {
		cfg.LogLevel = "info"
	}
	if cfg.API.ListenAddr == "" {
		cfg.API.ListenAddr = "127.0.0.1:8080"
	}
	if cfg.Storage.Driver == "" {
		cfg.Storage.Driver = "sqlite"
	}
	if cfg.Storage.Path == "" {
		cfg.Storage.Path = "data/openseal.db"
	}
	if cfg.Storage.ArtifactsPath == "" {
		cfg.Storage.ArtifactsPath = "data/artifacts"
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// WriteDaemonConfig serializes a config to YAML and writes it to disk.
func WriteDaemonConfig(path string, cfg *DaemonConfig) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

// Validate checks that required durable-kernel fields are present.
func (c *DaemonConfig) Validate() error {
	if c.Storage.Driver != "sqlite" {
		return fmt.Errorf("storage.driver must be sqlite")
	}
	if c.Storage.Path == "" {
		return fmt.Errorf("storage.path is required")
	}
	if c.Storage.ArtifactsPath == "" {
		return fmt.Errorf("storage.artifactsPath is required")
	}

	seenPolicies := make(map[string]bool, len(c.SourcePolicies))
	for index := range c.SourcePolicies {
		configured := &c.SourcePolicies[index]
		if err := configured.Scope.Validate(); err != nil {
			return fmt.Errorf("source policy %d scope: %w", index, err)
		}
		if err := configured.Policy.Validate(); err != nil {
			return fmt.Errorf("source policy %d: %w", index, err)
		}
		key := scopedSourcePolicyKey(configured.Scope, configured.Policy.ID+"@"+configured.Policy.Version)
		if seenPolicies[key] {
			return fmt.Errorf("source policy %s is duplicated in scope %s:%s", configured.Policy.ID+"@"+configured.Policy.Version, configured.Scope.Kind, configured.Scope.ID)
		}
		seenPolicies[key] = true
	}

	return nil
}

// SourcePolicyCatalog is immutable after construction, so worker scope
// reconciliation and authorization can read it concurrently without copying
// secrets or consulting model-visible state.
type SourcePolicyCatalog struct {
	mu       sync.RWMutex
	policies map[string]source.Policy
	scopes   map[string]runtime.Scope
}

func NewSourcePolicyCatalog(configured []ScopedSourcePolicy) (*SourcePolicyCatalog, error) {
	catalog := &SourcePolicyCatalog{policies: make(map[string]source.Policy, len(configured)), scopes: make(map[string]runtime.Scope)}
	for index := range configured {
		entry := configured[index]
		if err := entry.Scope.Validate(); err != nil {
			return nil, fmt.Errorf("source policy %d scope: %w", index, err)
		}
		if err := entry.Policy.Validate(); err != nil {
			return nil, fmt.Errorf("source policy %d: %w", index, err)
		}
		reference := entry.Policy.ID + "@" + entry.Policy.Version
		key := scopedSourcePolicyKey(entry.Scope, reference)
		if _, exists := catalog.policies[key]; exists {
			return nil, fmt.Errorf("source policy %s is duplicated in scope %s:%s", reference, entry.Scope.Kind, entry.Scope.ID)
		}
		catalog.policies[key] = entry.Policy
		if entry.Policy.Enabled && entry.Policy.Outreach != nil && entry.Policy.Outreach.Enabled {
			catalog.scopes[workerScopeKey(entry.Scope)] = entry.Scope
		}
	}
	return catalog, nil
}

func (c *SourcePolicyCatalog) ResolveOutreachPolicy(_ context.Context, scope skill.ScopeReference, reference string) (*source.Policy, error) {
	if c == nil {
		return nil, fmt.Errorf("source policy catalog is unavailable")
	}
	c.mu.RLock()
	policy, ok := c.policies[scopedSourcePolicyKey(runtime.Scope{Kind: scope.Kind, ID: scope.ID}, reference)]
	c.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("source policy %q is not configured in scope %s:%s", strings.TrimSpace(reference), scope.Kind, scope.ID)
	}
	copy := policy
	copy.Sources = append([]source.PolicySource(nil), policy.Sources...)
	for index := range copy.Sources {
		copy.Sources[index].PathPrefixes = append([]string(nil), policy.Sources[index].PathPrefixes...)
	}
	if policy.Outreach != nil {
		outreachCopy := *policy.Outreach
		copy.Outreach = &outreachCopy
	}
	return &copy, nil
}

func (c *SourcePolicyCatalog) ListWorkerScopes(context.Context) ([]runtime.Scope, error) {
	if c == nil {
		return nil, fmt.Errorf("source policy catalog is unavailable")
	}
	c.mu.RLock()
	result := make([]runtime.Scope, 0, len(c.scopes))
	for _, scope := range c.scopes {
		result = append(result, scope)
	}
	c.mu.RUnlock()
	sort.Slice(result, func(left, right int) bool {
		if result[left].Kind == result[right].Kind {
			return result[left].ID < result[right].ID
		}
		return result[left].Kind < result[right].Kind
	})
	return result, nil
}

func (c *SourcePolicyCatalog) OutreachEnabled(scope runtime.Scope) bool {
	if c == nil {
		return false
	}
	c.mu.RLock()
	_, ok := c.scopes[workerScopeKey(scope)]
	c.mu.RUnlock()
	return ok
}

func scopedSourcePolicyKey(scope runtime.Scope, reference string) string {
	return workerScopeKey(scope) + "\x00" + strings.TrimSpace(reference)
}

func workerScopeKey(scope runtime.Scope) string {
	return strings.TrimSpace(scope.Kind) + "\x00" + strings.TrimSpace(scope.ID)
}
