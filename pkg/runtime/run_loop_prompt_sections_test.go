package runtime

import (
	"regexp"
	"strings"
	"testing"
)

func loopKernelSectionTexts() map[string]string {
	return map[string]string{
		"skill_access":            loopSkillAccessText,
		"progress_rules":          loopProgressRulesText,
		"progress_rules/taskwork": loopProgressRulesTaskWorkText,
		"conversation_tasks":      loopConversationTasksText,
		"workspace":               loopWorkspaceText,
		"media":                   loopMediaText,
		"review_summaries":        loopReviewSummariesText,
	}
}

func TestPromptSectionsHaveNoFormWords(t *testing.T) {
	forbidden := []string{"runoutput", "outputsummary", "continuationcheckpoint", "this turn", "proposal", "hosted-turn", "offeredactions", "_atlas"}
	for name, text := range loopKernelSectionTexts() {
		lower := strings.ToLower(text)
		for _, word := range forbidden {
			if strings.Contains(lower, word) {
				t.Errorf("section %s contains form wording %q", name, word)
			}
		}
		if text == "" || strings.TrimSpace(text) != text {
			t.Errorf("section %s text is empty or has surrounding whitespace", name)
		}
	}
}

func TestPromptSectionOrderAndTitles(t *testing.T) {
	want := []string{"progress_rules", "conversation_tasks", "workspace", "media", "routine_script"}
	if strings.Join(loopKernelSectionOrder, ",") != strings.Join(want, ",") {
		t.Fatalf("order = %v, want %v", loopKernelSectionOrder, want)
	}
	id := regexp.MustCompile(`^[a-z0-9_.-]{1,64}$`)
	seen := map[string]bool{}
	for _, section := range loopKernelSectionOrder {
		if !id.MatchString(section) || seen[section] {
			t.Errorf("section id %q is invalid or repeated", section)
		}
		seen[section] = true
		if strings.TrimSpace(loopKernelSectionTitles[section]) == "" {
			t.Errorf("section %s has no title", section)
		}
	}
	if len(loopKernelSectionTitles) != len(loopKernelSectionOrder) {
		t.Errorf("titles cover %d sections, order has %d", len(loopKernelSectionTitles), len(loopKernelSectionOrder))
	}
}
