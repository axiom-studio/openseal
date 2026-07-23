package executor

import (
	"os"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/config"
)

func TestTokenBudget_UnderSoftLimit_NoWarning(t *testing.T) {
	smallBody := strings.Repeat("x ", 500)

	skillsDir := setupTestSkillsDir(t, map[string]string{
		"small-skill": `---
name: small-skill
description: Small skill
version: "1.0"
metadata:
  openclaw:
    always: true
---

` + smallBody,
	})
	t.Setenv(config.OpenClawSkillsDirectoryEnvironment, skillsDir)

	executor := NewAIExecutor()
	result, warnings := executor.buildSystemPromptWithSkills("Original prompt.", []string{"small-skill"}, nil)

	if len(warnings) != 0 {
		t.Errorf("expected no warnings for skill under soft limit, got: %v", warnings)
	}
	if !strings.Contains(result, "Original prompt.") {
		t.Error("result should contain original prompt")
	}
	if !strings.Contains(result, "--- SKILL: small-skill ---") {
		t.Error("result should contain small-skill injection")
	}
}

func TestTokenBudget_OverSoftLimit_WarningReturned(t *testing.T) {
	bodyOverSoftLimit := strings.Repeat("line of padding content to exceed soft limit\n", 500)

	skillsDir := setupTestSkillsDir(t, map[string]string{
		"large-skill": `---
name: large-skill
description: Large skill
version: "1.0"
metadata:
  openclaw:
    always: true
---

` + bodyOverSoftLimit,
	})
	t.Setenv(config.OpenClawSkillsDirectoryEnvironment, skillsDir)
	t.Setenv("SKILL_PROMPT_MAX_SIZE_KB", "1")

	executor := NewAIExecutor()
	_, warnings := executor.buildSystemPromptWithSkills("Original.", []string{"large-skill"}, nil)

	hasBudgetWarning := false
	for _, w := range warnings {
		if strings.Contains(w, "approaching limit") || strings.Contains(w, "exceed") || strings.Contains(w, "truncated") {
			hasBudgetWarning = true
			break
		}
	}
	if !hasBudgetWarning {
		t.Errorf("expected budget warning when exceeding soft limit, got: %v", warnings)
	}

	t.Setenv("SKILL_PROMPT_MAX_SIZE_KB", "")
}

func TestTokenBudget_OverHardLimit_SkillNotInjected(t *testing.T) {
	veryLargeBody := strings.Repeat("This line pads the skill content to exceed the hard limit threshold.\n", 5000)

	skillsDir := setupTestSkillsDir(t, map[string]string{
		"massive-skill": `---
name: massive-skill
description: Massive skill
version: "1.0"
metadata:
  openclaw:
    always: true
---

` + veryLargeBody,
	})
	t.Setenv(config.OpenClawSkillsDirectoryEnvironment, skillsDir)
	t.Setenv("SKILL_PROMPT_MAX_SIZE_KB", "1")

	executor := NewAIExecutor()
	result, warnings := executor.buildSystemPromptWithSkills("Original.", []string{"massive-skill"}, nil)

	hasHardLimitWarning := false
	for _, w := range warnings {
		if strings.Contains(w, "truncated") || strings.Contains(w, "exceed") {
			hasHardLimitWarning = true
			break
		}
	}
	if !hasHardLimitWarning {
		t.Errorf("expected hard limit warning, got: %v", warnings)
	}

	if !strings.Contains(result, "Original.") {
		t.Error("result should still contain original prompt even when skill is truncated")
	}

	injectionSize := CalculateSkillInjectionSize("massive-skill", veryLargeBody)
	_, hardLimitBytes := GetSkillPromptBudget()
	if injectionSize > hardLimitBytes && strings.Contains(result, "--- SKILL: massive-skill ---") {
		t.Error("massive-skill should not be fully injected when it exceeds hard limit")
	}

	t.Setenv("SKILL_PROMPT_MAX_SIZE_KB", "")
}

func TestTokenBudget_ConfigurableLimitViaEnvVar(t *testing.T) {
	mediumBody := strings.Repeat("Medium skill content line for budget test.\n", 100)

	skillsDir := setupTestSkillsDir(t, map[string]string{
		"medium-skill": `---
name: medium-skill
description: Medium skill
version: "1.0"
metadata:
  openclaw:
    always: true
---

` + mediumBody,
	})
	t.Setenv(config.OpenClawSkillsDirectoryEnvironment, skillsDir)

	t.Setenv("SKILL_PROMPT_MAX_SIZE_KB", "1")
	softLimit, hardLimit := GetSkillPromptBudget()
	if softLimit != 1024 {
		t.Errorf("expected soft limit 1024 bytes with 1KB env, got %d", softLimit)
	}
	if hardLimit != 1536 {
		t.Errorf("expected hard limit 1536 bytes with 1KB env, got %d", hardLimit)
	}

	executor := NewAIExecutor()
	_, warnings := executor.buildSystemPromptWithSkills("Test.", []string{"medium-skill"}, nil)

	if len(warnings) == 0 {
		t.Error("expected warning with 1KB limit and medium skill")
	}

	t.Setenv("SKILL_PROMPT_MAX_SIZE_KB", "100")
	softLimit, hardLimit = GetSkillPromptBudget()
	if softLimit != 102400 {
		t.Errorf("expected soft limit 102400 bytes with 100KB env, got %d", softLimit)
	}
	if hardLimit != 153600 {
		t.Errorf("expected hard limit 153600 bytes with 100KB env, got %d", hardLimit)
	}

	t.Setenv("SKILL_PROMPT_MAX_SIZE_KB", "")
}

func TestTokenBudget_CumulativeMultipleSkills(t *testing.T) {
	mediumBody := strings.Repeat("Cumulative skill content line.\n", 200)

	skillsDir := setupTestSkillsDir(t, map[string]string{
		"skill-a": `---
name: skill-a
description: Skill A
version: "1.0"
metadata:
  openclaw:
    always: true
---

` + mediumBody,
		"skill-b": `---
name: skill-b
description: Skill B
version: "1.0"
metadata:
  openclaw:
    always: true
---

` + mediumBody,
		"skill-c": `---
name: skill-c
description: Skill C
version: "1.0"
metadata:
  openclaw:
    always: true
---

` + mediumBody,
	})
	t.Setenv(config.OpenClawSkillsDirectoryEnvironment, skillsDir)
	t.Setenv("SKILL_PROMPT_MAX_SIZE_KB", "2")

	executor := NewAIExecutor()
	result, warnings := executor.buildSystemPromptWithSkills("Original.", []string{"skill-a", "skill-b", "skill-c"}, nil)

	hasBudgetWarning := false
	for _, w := range warnings {
		if strings.Contains(w, "approaching") || strings.Contains(w, "exceed") || strings.Contains(w, "truncated") {
			hasBudgetWarning = true
			break
		}
	}
	if !hasBudgetWarning {
		t.Errorf("expected budget warning with cumulative skills exceeding limit, got: %v", warnings)
	}

	if !strings.Contains(result, "Original.") {
		t.Error("result should contain original prompt")
	}

	t.Setenv("SKILL_PROMPT_MAX_SIZE_KB", "")
}

func TestCalculateSkillInjectionSize(t *testing.T) {
	size := CalculateSkillInjectionSize("test-skill", "body content")
	expected := len("--- SKILL: test-skill ---\nbody content\n--- END SKILL ---\n")
	if size != expected {
		t.Errorf("expected size %d, got %d", expected, size)
	}
}

func TestGetSkillPromptBudget_DefaultValues(t *testing.T) {
	os.Unsetenv("SKILL_PROMPT_MAX_SIZE_KB")

	softLimit, hardLimit := GetSkillPromptBudget()

	if softLimit != 20*1024 {
		t.Errorf("expected default soft limit %d, got %d", 20*1024, softLimit)
	}
	if hardLimit != 30*1024 {
		t.Errorf("expected default hard limit %d, got %d", 30*1024, hardLimit)
	}
}
