package executor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/axiom-studio/openseal/pkg/skill/skillmd"
	"go.uber.org/zap"
)

type SkillRepository interface {
	FindBySlug(slug string) (*OpenClawSkill, error)
	FindAll() ([]*OpenClawSkill, error)
}

type OpenClawSkill struct {
	ID          string
	Slug        string
	Name        string
	IsEnabled   bool
	SkillMdPath string
}

func CheckOrphanedSkillReferences(slug string, skillsDir string, repo SkillRepository, logger *zap.SugaredLogger) []string {
	var warnings []string

	allSkills, err := repo.FindAll()
	if err != nil {
		logger.Warnw("cannot check for orphaned skill references", "slug", slug, "error", err)
		return warnings
	}

	for _, skill := range allSkills {
		if skill.Slug == slug || !skill.IsEnabled {
			continue
		}
		if skill.SkillMdPath == "" {
			continue
		}

		content, err := os.ReadFile(skill.SkillMdPath)
		if err != nil {
			continue
		}

		if referencesSkill(content, slug) {
			warnings = append(warnings, fmt.Sprintf("skill %q references uninstalled skill %q", skill.Slug, slug))
			logger.Warnw("orphaned skill reference detected",
				"referencing_skill", skill.Slug,
				"missing_skill", slug)
		}
	}

	return warnings
}

func referencesSkill(content []byte, slug string) bool {
	text := string(content)
	patterns := []string{
		fmt.Sprintf("@%s", slug),
		fmt.Sprintf("skill:%s", slug),
		fmt.Sprintf("use %s", slug),
		fmt.Sprintf("load %s", slug),
	}
	for _, pat := range patterns {
		if strings.Contains(text, pat) {
			return true
		}
	}
	return false
}

func CleanupOrphanedSkillFiles(slug string, skillsDir string, logger *zap.SugaredLogger) {
	skillDir := filepath.Join(skillsDir, slug)
	if _, err := os.Stat(skillDir); os.IsNotExist(err) {
		return
	}

	leftoverFiles, err := filepath.Glob(filepath.Join(skillDir, "*"))
	if err != nil {
		logger.Warnw("cannot check for leftover skill files", "slug", slug, "error", err)
		return
	}

	if len(leftoverFiles) > 0 {
		logger.Warnw("leftover skill files detected after uninstall",
			"slug", slug,
			"leftover_count", len(leftoverFiles))
	}
}

func ScanForOrphanedSkills(skillsDir string, repo SkillRepository, logger *zap.SugaredLogger) []string {
	var orphans []string

	installedSkills, err := repo.FindAll()
	if err != nil {
		logger.Warnw("cannot scan for orphaned skills", "error", err)
		return orphans
	}

	installedSlugs := make(map[string]bool)
	for _, skill := range installedSkills {
		installedSlugs[skill.Slug] = true
	}

	entries, err := os.ReadDir(skillsDir)
	if err != nil {
		logger.Warnw("cannot read skills directory", "dir", skillsDir, "error", err)
		return orphans
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if installedSlugs[entry.Name()] {
			continue
		}

		skillMdPath := filepath.Join(skillsDir, entry.Name(), "SKILL.md")
		content, err := os.ReadFile(skillMdPath)
		if err != nil {
			orphans = append(orphans, entry.Name())
			continue
		}

		parsed, err := skillmd.ParseSkillMD(content)
		if err != nil {
			orphans = append(orphans, entry.Name())
			continue
		}

		if parsed.Name != entry.Name() {
			orphans = append(orphans, entry.Name())
			logger.Warnw("orphaned skill directory found",
				"directory", entry.Name(),
				"declared_name", parsed.Name)
		}
	}

	return orphans
}
