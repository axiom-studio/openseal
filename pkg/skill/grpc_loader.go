package skill

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	skillgrpc "github.com/axiom-studio/openseal/pkg/skillgrpc"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

type GRPCSkillLoader struct {
	logger       *zap.SugaredLogger
	skillsDir    string
	registry     *skillgrpc.Registry
	loadedSkills map[string]*LoadedGRPCSkill
}

type LoadedGRPCSkill struct {
	SkillID    string
	Manifest   *SkillManifest
	Address    string
	ProcessPID int
}

func NewGRPCSkillLoader(logger *zap.SugaredLogger, skillsDir string) *GRPCSkillLoader {
	return &GRPCSkillLoader{
		logger:       logger,
		skillsDir:    skillsDir,
		registry:     skillgrpc.NewRegistry(),
		loadedSkills: make(map[string]*LoadedGRPCSkill),
	}
}

func (l *GRPCSkillLoader) LoadAllSkills(ctx context.Context) ([]*LoadedGRPCSkill, error) {
	if l.skillsDir == "" {
		return nil, nil
	}

	l.logger.Infow("Walking skills directory", "dir", l.skillsDir)

	var skills []*LoadedGRPCSkill

	err := filepath.WalkDir(l.skillsDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			if strings.HasPrefix(d.Name(), ".") && d.Name() != "." {
				return fs.SkipDir
			}
			return nil
		}

		if d.Name() == "skill.yaml" {
			skillPath := filepath.Dir(path)
			skillID := filepath.Base(skillPath)

			l.logger.Infow("Found skill.yaml", "path", path, "skillPath", skillPath, "skillID", skillID)

			loaded, err := l.loadSkill(ctx, skillID, skillPath)
			if err != nil {
				l.logger.Errorw("Failed to load gRPC skill",
					"skill", skillID, "path", skillPath, "error", err)
				return nil
			}

			skills = append(skills, loaded)
		}

		return nil
	})

	if err != nil && !os.IsNotExist(err) {
		l.logger.Warnw("Error walking skills dir", "error", err)
	}

	l.logger.Infow("Loaded gRPC skills", "count", len(skills))
	return skills, nil
}

func (l *GRPCSkillLoader) loadSkill(ctx context.Context, skillID, skillPath string) (*LoadedGRPCSkill, error) {
	manifestPath := filepath.Join(skillPath, "skill.yaml")
	manifestData, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read manifest: %w", err)
	}

	var manifest SkillManifest
	if err := yaml.Unmarshal(manifestData, &manifest); err != nil {
		return nil, fmt.Errorf("failed to parse manifest: %w", err)
	}

	if manifest.Spec.ExecutorType != "grpc" {
		return nil, fmt.Errorf("unsupported executor type: %s (expected grpc)", manifest.Spec.ExecutorType)
	}

	address := manifest.Spec.GRPC.Address
	if address == "" && manifest.Spec.GRPC.Port > 0 {
		address = fmt.Sprintf("localhost:%d", manifest.Spec.GRPC.Port)
	}
	if address == "" {
		address = fmt.Sprintf("localhost:%d", 50051+len(l.loadedSkills))
	}

	if len(manifest.Spec.GRPC.Binary) > 0 {
		platform := fmt.Sprintf("%s-%s", runtime.GOOS, runtime.GOARCH)
		binaryRelPath, ok := manifest.Spec.GRPC.Binary[platform]
		if !ok {
			return nil, fmt.Errorf("no binary for platform: %s", platform)
		}

		binaryPath := filepath.Join(skillPath, binaryRelPath)
		if _, err := os.Stat(binaryPath); os.IsNotExist(err) {
			return nil, fmt.Errorf("binary not found: %s", binaryPath)
		}
	}

	_, err = l.registry.Register(ctx, address)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to gRPC skill at %s: %w", address, err)
	}

	loaded := &LoadedGRPCSkill{
		SkillID:  skillID,
		Manifest: &manifest,
		Address:  address,
	}

	l.loadedSkills[skillID] = loaded

	l.logger.Infow("Loaded gRPC skill",
		"skill", skillID,
		"version", manifest.Metadata.Version,
		"address", address,
		"nodeTypes", len(l.registry.ListTypes()))

	return loaded, nil
}

func (l *GRPCSkillLoader) UnloadSkill(skillID string) error {
	loaded, ok := l.loadedSkills[skillID]
	if !ok {
		return fmt.Errorf("skill not loaded: %s", skillID)
	}

	if err := l.registry.Unregister(skillID); err != nil {
		l.logger.Warnw("Error disconnecting from gRPC skill",
			"skill", skillID, "error", err)
	}

	delete(l.loadedSkills, skillID)
	l.logger.Infow("Unloaded gRPC skill", "skill", skillID, "address", loaded.Address)
	return nil
}

func (l *GRPCSkillLoader) GetRegistry() *skillgrpc.Registry {
	return l.registry
}

func (l *GRPCSkillLoader) GetLoadedSkill(skillID string) (*LoadedGRPCSkill, bool) {
	skill, ok := l.loadedSkills[skillID]
	return skill, ok
}

func (l *GRPCSkillLoader) ListLoadedSkills() []string {
	skills := make([]string, 0, len(l.loadedSkills))
	for id := range l.loadedSkills {
		skills = append(skills, id)
	}
	return skills
}

func (l *GRPCSkillLoader) Close() error {
	return l.registry.Close()
}

func (l *GRPCSkillLoader) StartHealthChecks(ctx context.Context, interval time.Duration) {
	go l.registry.HealthCheck(ctx, interval)
}
