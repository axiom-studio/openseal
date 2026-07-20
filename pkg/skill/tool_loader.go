package skill

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/axiom-studio/openseal/pkg/executor"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

type ToolLoader struct {
	logger    *zap.SugaredLogger
	skillsDir string
	registry  *executor.ToolRegistry
}

func NewToolLoader(logger *zap.SugaredLogger, skillsDir string) *ToolLoader {
	return &ToolLoader{
		logger:    logger,
		skillsDir: skillsDir,
		registry:  executor.GetGlobalToolRegistry(),
	}
}

func (l *ToolLoader) LoadAllSkills(ctx context.Context) error {
	if _, err := os.Stat(l.skillsDir); os.IsNotExist(err) {
		l.logger.Infow("skills directory does not exist, skipping", "path", l.skillsDir)
		return nil
	}

	entries, err := os.ReadDir(l.skillsDir)
	if err != nil {
		return fmt.Errorf("failed to read skills directory: %w", err)
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		skillDir := filepath.Join(l.skillsDir, entry.Name())
		if err := l.loadSkill(ctx, skillDir); err != nil {
			l.logger.Errorw("failed to load skill", "dir", entry.Name(), "error", err)
			continue
		}
	}

	return nil
}

func (l *ToolLoader) loadSkill(ctx context.Context, skillDir string) error {
	manifestPath := filepath.Join(skillDir, "skill.yaml")
	if _, err := os.Stat(manifestPath); os.IsNotExist(err) {
		return fmt.Errorf("skill.yaml not found in %s", skillDir)
	}

	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return fmt.Errorf("failed to read skill.yaml: %w", err)
	}

	var manifest SkillManifest
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		return fmt.Errorf("failed to parse skill.yaml: %w", err)
	}

	if len(manifest.Spec.Tools) == 0 {
		l.logger.Debugw("skill has no tools, skipping", "id", manifest.Metadata.ID)
		return nil
	}

	for _, toolDef := range manifest.Spec.Tools {
		toolConfig := toolDef.Config
		if toolConfig == nil {
			toolConfig = map[string]interface{}{
				"type":    "mcp",
				"command": manifest.Spec.MCP.Command,
				"args":    manifest.Spec.MCP.Args,
				"env":     manifest.Spec.MCP.Env,
			}
		}

		tool := &executor.ToolDefinition{
			Name:        toolDef.Name,
			Description: toolDef.Description,
			Parameters:  toolDef.Parameters,
			Config:      toolConfig,
		}

		l.registry.RegisterTool(tool)
		l.logger.Infow("registered tool from skill", "tool", toolDef.Name, "skill", manifest.Metadata.ID)
	}

	l.logger.Infow("loaded skill tools", "skill", manifest.Metadata.ID, "tools", len(manifest.Spec.Tools))
	return nil
}

func (l *ToolLoader) ReloadSkill(ctx context.Context, skillID string) error {
	entries, err := os.ReadDir(l.skillsDir)
	if err != nil {
		return fmt.Errorf("failed to read skills directory: %w", err)
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		skillPath := filepath.Join(l.skillsDir, entry.Name())
		manifestPath := filepath.Join(skillPath, "skill.yaml")

		data, err := os.ReadFile(manifestPath)
		if err != nil {
			continue
		}

		var manifest SkillManifest
		if err := yaml.Unmarshal(data, &manifest); err != nil {
			continue
		}

		if manifest.Metadata.ID == skillID {
			return l.loadSkill(ctx, skillPath)
		}
	}

	return fmt.Errorf("skill not found: %s", skillID)
}
