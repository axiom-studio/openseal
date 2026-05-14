package executor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// NodeMetadata holds metadata from embedded_nodes/*.yaml files.
type NodeMetadata struct {
	Name        string                 `yaml:"name" json:"name"`
	DisplayName string                 `yaml:"displayName" json:"displayName"`
	Category    string                 `yaml:"category" json:"category"`
	Description string                 `yaml:"description" json:"description"`
	Icon        string                 `yaml:"icon" json:"icon"`
	InputSchema map[string]interface{} `yaml:"inputSchema" json:"inputSchema"`
	OutputSchema map[string]interface{} `yaml:"outputSchema" json:"outputSchema"`
}

// ExecutorInfo combines registry type with metadata.
type ExecutorInfo struct {
	Type        string                 `json:"type"`
	Name        string                 `json:"name"`
	Category    string                 `json:"category"`
	Description string                 `json:"description"`
	Icon        string                 `json:"icon"`
	InputSchema map[string]interface{} `json:"inputSchema,omitempty"`
	OutputSchema map[string]interface{} `json:"outputSchema,omitempty"`
}

// embeddedNodesDir is the path to embedded node schema YAMLs.
// Override at build time if needed.
var embeddedNodesDir = "embedded_nodes"

// LoadNodeMetadata reads all YAML files in embedded_nodes/ and returns a map name->metadata.
func LoadNodeMetadata() (map[string]*NodeMetadata, error) {
	entries, err := os.ReadDir(embeddedNodesDir)
	if err != nil {
		return nil, fmt.Errorf("read embedded_nodes dir: %w", err)
	}

	meta := make(map[string]*NodeMetadata)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}

		path := filepath.Join(embeddedNodesDir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}

		var m NodeMetadata
		if err := yaml.Unmarshal(data, &m); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		if m.Name != "" {
			meta[m.Name] = &m
		}
	}
	return meta, nil
}

// List returns all registered executors enriched with metadata from embedded_nodes YAMLs.
func (r *Registry) List() []ExecutorInfo {
	meta, _ := LoadNodeMetadata()

	infos := make([]ExecutorInfo, 0, len(r.executors))
	for typ := range r.executors {
		info := ExecutorInfo{Type: typ}
		if m, ok := meta[typ]; ok {
			info.Name = m.DisplayName
			if info.Name == "" {
				info.Name = m.Name
			}
			info.Category = m.Category
			info.Description = m.Description
			info.Icon = m.Icon
			info.InputSchema = m.InputSchema
			info.OutputSchema = m.OutputSchema
		} else {
			info.Name = typ
		}
		infos = append(infos, info)
	}
	return infos
}
