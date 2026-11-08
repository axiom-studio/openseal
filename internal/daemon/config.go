package daemon

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

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
	return nil
}
