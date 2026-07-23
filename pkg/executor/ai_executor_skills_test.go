package executor

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/axiom-studio/openseal/pkg/config"
)

func setupTestSkillsDir(t *testing.T, skills map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for slug, content := range skills {
		skillDir := filepath.Join(dir, slug)
		if err := os.MkdirAll(skillDir, 0755); err != nil {
			t.Fatalf("failed to create skill dir %s: %v", skillDir, err)
		}
		skillFile := filepath.Join(skillDir, "SKILL.md")
		if err := os.WriteFile(skillFile, []byte(content), 0644); err != nil {
			t.Fatalf("failed to write SKILL.md for %s: %v", slug, err)
		}
	}
	return dir
}

func TestBuildSystemPromptWithSkills_NoSkills(t *testing.T) {
	executor := NewAIExecutor()
	originalPrompt := "You are a helpful assistant."

	result, warnings := executor.buildSystemPromptWithSkills(originalPrompt, nil, nil)

	if result != originalPrompt {
		t.Errorf("expected original prompt unchanged, got:\n%s", result)
	}
	if len(warnings) != 0 {
		t.Errorf("expected no warnings, got: %v", warnings)
	}
}

func TestBuildSystemPromptWithSkills_EmptySkills(t *testing.T) {
	executor := NewAIExecutor()
	originalPrompt := "You are a helpful assistant."

	result, warnings := executor.buildSystemPromptWithSkills(originalPrompt, []string{}, nil)

	if result != originalPrompt {
		t.Errorf("expected original prompt unchanged, got:\n%s", result)
	}
	if len(warnings) != 0 {
		t.Errorf("expected no warnings, got: %v", warnings)
	}
}

func TestBuildSystemPromptWithSkills_SingleEnabledSkill(t *testing.T) {
	skillsDir := setupTestSkillsDir(t, map[string]string{
		"code-review": `---
name: code-review
description: Code review skill
version: "1.0"
metadata:
  openclaw:
    always: true
---

You are an expert code reviewer. Follow these guidelines:
- Review for correctness, performance, and style
- Provide constructive feedback
`,
	})
	t.Setenv(config.OpenClawSkillsDirectoryEnvironment, skillsDir)

	executor := NewAIExecutor()
	originalPrompt := "Summarize the following code."

	result, warnings := executor.buildSystemPromptWithSkills(originalPrompt, []string{"code-review"}, nil)

	expectedPrefix := "--- SKILL: code-review ---\n"
	expectedSuffix := "\n--- END SKILL ---\n\n" + originalPrompt

	if len(warnings) != 0 {
		t.Errorf("expected no warnings, got: %v", warnings)
	}
	if result[:len(expectedPrefix)] != expectedPrefix {
		t.Errorf("expected prefix %q, got:\n%s", expectedPrefix, result[:50])
	}
	if result[len(result)-len(originalPrompt):] != originalPrompt {
		t.Errorf("expected original prompt at end, got:\n%s", result[len(result)-len(originalPrompt):])
	}
	expectedBody := "\nYou are an expert code reviewer. Follow these guidelines:\n- Review for correctness, performance, and style\n- Provide constructive feedback\n"
	if result != expectedPrefix+expectedBody+expectedSuffix {
		t.Errorf("unexpected result:\n%s", result)
	}
}

func TestBuildSystemPromptWithSkills_TwoSkillsAlphabetical(t *testing.T) {
	skillsDir := setupTestSkillsDir(t, map[string]string{
		"zebra-skill": `---
name: zebra-skill
description: Zebra skill
version: "1.0"
metadata:
  openclaw:
    always: true
---

Zebra content here.
`,
		"alpha-skill": `---
name: alpha-skill
description: Alpha skill
version: "1.0"
metadata:
  openclaw:
    always: true
---

Alpha content here.
`,
	})
	t.Setenv(config.OpenClawSkillsDirectoryEnvironment, skillsDir)

	executor := NewAIExecutor()
	originalPrompt := "Do something."

	result, warnings := executor.buildSystemPromptWithSkills(originalPrompt, []string{"zebra-skill", "alpha-skill"}, nil)

	if len(warnings) != 0 {
		t.Errorf("expected no warnings, got: %v", warnings)
	}

	alphaIdx := indexOf(result, "--- SKILL: alpha-skill ---")
	zebraIdx := indexOf(result, "--- SKILL: zebra-skill ---")

	if alphaIdx == -1 {
		t.Error("expected alpha-skill to be injected")
	}
	if zebraIdx == -1 {
		t.Error("expected zebra-skill to be injected")
	}
	if alphaIdx > zebraIdx {
		t.Errorf("expected alpha-skill before zebra-skill, but alpha at %d, zebra at %d", alphaIdx, zebraIdx)
	}
}

func TestBuildSystemPromptWithSkills_UnavailableSkillGating(t *testing.T) {
	skillsDir := setupTestSkillsDir(t, map[string]string{
		"missing-bin-skill": `---
name: missing-bin-skill
description: Requires a binary that doesn't exist
version: "1.0"
metadata:
  openclaw:
    requires_bins: ["nonexistent-binary-xyz-123"]
---

This skill should not be injected.
`,
		"always-skill": `---
name: always-skill
description: Always available
version: "1.0"
metadata:
  openclaw:
    always: true
---

Always available content.
`,
	})
	t.Setenv(config.OpenClawSkillsDirectoryEnvironment, skillsDir)

	executor := NewAIExecutor()
	originalPrompt := "Hello."

	result, warnings := executor.buildSystemPromptWithSkills(originalPrompt, []string{"missing-bin-skill", "always-skill"}, nil)

	if contains(result, "--- SKILL: missing-bin-skill ---") {
		t.Error("missing-bin-skill should NOT be injected due to gating")
	}
	if !contains(result, "--- SKILL: always-skill ---") {
		t.Error("always-skill should be injected")
	}
	if len(warnings) == 0 {
		t.Error("expected warning for unavailable skill")
	}
}

func TestBuildSystemPromptWithSkills_ExceedsSizeLimit(t *testing.T) {
	largeBody := ""
	for i := 0; i < 2500; i++ {
		largeBody += "This is a line of skill content to pad the size.\n"
	}

	skillsDir := setupTestSkillsDir(t, map[string]string{
		"large-skill": `---
name: large-skill
description: A very large skill
version: "1.0"
metadata:
  openclaw:
    always: true
---

` + largeBody,
	})
	t.Setenv(config.OpenClawSkillsDirectoryEnvironment, skillsDir)

	executor := NewAIExecutor()
	originalPrompt := "Original prompt."

	result, warnings := executor.buildSystemPromptWithSkills(originalPrompt, []string{"large-skill"}, nil)

	hasTruncationWarning := false
	for _, w := range warnings {
		if contains(w, "truncat") || contains(w, "exceed") || contains(w, "limit") {
			hasTruncationWarning = true
			break
		}
	}
	if !hasTruncationWarning {
		t.Errorf("expected truncation warning, got: %v", warnings)
	}

	if !contains(result, originalPrompt) {
		t.Error("result should still contain original prompt")
	}
}

func TestBuildSystemPromptWithSkills_AlphabeticalOrderByName(t *testing.T) {
	skillsDir := setupTestSkillsDir(t, map[string]string{
		"charlie": `---
name: charlie
description: Charlie skill
version: "1.0"
metadata:
  openclaw:
    always: true
---

Charlie body.
`,
		"alpha": `---
name: alpha
description: Alpha skill
version: "1.0"
metadata:
  openclaw:
    always: true
---

Alpha body.
`,
		"bravo": `---
name: bravo
description: Bravo skill
version: "1.0"
metadata:
  openclaw:
    always: true
---

Bravo body.
`,
	})
	t.Setenv(config.OpenClawSkillsDirectoryEnvironment, skillsDir)

	executor := NewAIExecutor()
	originalPrompt := "Test."

	result, _ := executor.buildSystemPromptWithSkills(originalPrompt, []string{"charlie", "alpha", "bravo"}, nil)

	alphaIdx := indexOf(result, "--- SKILL: alpha ---")
	bravoIdx := indexOf(result, "--- SKILL: bravo ---")
	charlieIdx := indexOf(result, "--- SKILL: charlie ---")

	if alphaIdx == -1 || bravoIdx == -1 || charlieIdx == -1 {
		t.Fatalf("missing skill injection: alpha=%d, bravo=%d, charlie=%d", alphaIdx, bravoIdx, charlieIdx)
	}

	if !(alphaIdx < bravoIdx && bravoIdx < charlieIdx) {
		t.Errorf("expected alphabetical order: alpha(%d) < bravo(%d) < charlie(%d)", alphaIdx, bravoIdx, charlieIdx)
	}
}

func TestBuildSystemPromptWithSkills_MissingSkillFile(t *testing.T) {
	skillsDir := setupTestSkillsDir(t, map[string]string{})
	t.Setenv(config.OpenClawSkillsDirectoryEnvironment, skillsDir)

	executor := NewAIExecutor()
	originalPrompt := "Hello."

	result, warnings := executor.buildSystemPromptWithSkills(originalPrompt, []string{"nonexistent-skill"}, nil)

	if result != originalPrompt {
		t.Errorf("expected original prompt when skill file missing, got:\n%s", result)
	}
	if len(warnings) == 0 {
		t.Error("expected warning for missing skill file")
	}
}

func TestBuildSystemPromptWithSkills_CustomMaxSize(t *testing.T) {
	mediumBody := ""
	for i := 0; i < 80; i++ {
		mediumBody += "Medium skill content line.\n"
	}

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

	executor := NewAIExecutor()
	originalPrompt := "Original."

	result, warnings := executor.buildSystemPromptWithSkills(originalPrompt, []string{"medium-skill"}, nil)

	hasSizeWarning := false
	for _, w := range warnings {
		if contains(w, "truncat") || contains(w, "exceed") || contains(w, "limit") || contains(w, "size") {
			hasSizeWarning = true
			break
		}
	}
	if !hasSizeWarning {
		t.Errorf("expected size warning with 1KB limit, got: %v", warnings)
	}

	if !contains(result, originalPrompt) {
		t.Error("result should contain original prompt")
	}

	t.Setenv("SKILL_PROMPT_MAX_SIZE_KB", "")
}

func indexOf(s, substr string) int {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}

func contains(s, substr string) bool {
	return indexOf(s, substr) != -1
}
