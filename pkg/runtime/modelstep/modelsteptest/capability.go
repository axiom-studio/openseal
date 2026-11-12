package modelsteptest

import (
	"fmt"
	"strings"

	"github.com/axiom-studio/openseal/pkg/builtin"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/modelstep"
	"github.com/axiom-studio/openseal/pkg/workspace"
)

// CapabilityCatalogFixture is a deferred catalog: eight Notion-like actions
// bound to one prompt each, four research prompts, and the built-in Skill.
func CapabilityCatalogFixture() modelstep.CapabilityCatalog {
	catalog := modelstep.CapabilityCatalog{Scope: models.Scope{Kind: "tenant", ID: "tenant-1"}, AgentID: "agent-1"}
	for i := 0; i < 4; i++ {
		catalog.SkillPrompts = append(catalog.SkillPrompts, models.HostedSkillPrompt{
			SkillID: fmt.Sprintf("research.%d", i), Version: "1.0.0", Reference: fmt.Sprintf("skill-research-%d@1", i),
			Name: fmt.Sprintf("Research %d", i), Description: "Research guidance", Instructions: fmt.Sprintf("Research instructions %d.", i),
		})
	}
	catalog.SkillPrompts = append(catalog.SkillPrompts, models.HostedSkillPrompt{
		SkillID: "notion", Version: "3", Reference: "skill-notion@3", Name: "Notion", Description: "Notion workspace", Instructions: "Use Notion carefully.",
	})
	for i := 0; i < 8; i++ {
		catalog.Actions = append(catalog.Actions, capability.ModelAction{
			Name: fmt.Sprintf("notion.op%d", i), SkillID: "notion", Version: "3", Action: fmt.Sprintf("op%d", i),
			Description: fmt.Sprintf("Notion operation %d", i), InputSchema: map[string]interface{}{"type": "object"},
		})
	}
	catalog.SkillPrompts = append(catalog.SkillPrompts, models.HostedSkillPrompt{
		SkillID: builtin.SkillID, Version: "1", Reference: "builtin-z@1", Name: "Built-in", Instructions: "Built-in instructions.",
	}, models.HostedSkillPrompt{
		SkillID: builtin.SkillID, Version: "1", Reference: "builtin-a@1", Name: "Built-in A", Instructions: "Built-in A instructions.",
	})
	catalog.Actions = append(catalog.Actions, capability.ModelAction{
		Name: "openseal.builtin.web_search", SkillID: builtin.SkillID, Version: "1", Action: "web_search", Description: "Search the web",
	}, capability.ModelAction{
		Name: "openseal.builtin.generate_image", SkillID: builtin.SkillID, Version: "1", Action: "generate_image", Description: "Make an image",
	})
	catalog.HostTools = modelstep.HostToolSpecs(workspace.ChatAuthority("chat-1"))
	return catalog
}

// CapabilityNoteLines is the text of a capability listing, one line per item.
func CapabilityNoteLines(items []modelstep.CapabilityNoteItem) string {
	var b strings.Builder
	for _, item := range items {
		b.WriteString("\n" + item.Text)
	}
	return b.String()
}
