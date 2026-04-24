package main

import (
	"embed"
	"fmt"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

//go:embed embedded_nodes/*.yaml
var embeddedNodesFS embed.FS

func loadEmbeddedNodeSchemas() ([]map[string]interface{}, error) {
	schemas := make([]map[string]interface{}, 0)

	entries, err := embeddedNodesFS.ReadDir("embedded_nodes")
	if err != nil {
		return nil, fmt.Errorf("failed to read embedded nodes: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		path := filepath.Join("embedded_nodes", entry.Name())
		data, err := embeddedNodesFS.ReadFile(path)
		if err != nil {
			continue
		}

		var schema map[string]interface{}
		if err := yaml.Unmarshal(data, &schema); err != nil {
			continue
		}

		schemas = append(schemas, schema)
	}

	return schemas, nil
}
