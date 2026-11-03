package runtime

import (
	"context"
	"fmt"
	"strings"

	"github.com/axiom-studio/openseal/pkg/skill"
)

// maximumConnectedAppToolsNamed bounds the tool names a setup refusal lists.
const maximumConnectedAppToolsNamed = 8

// connectedAppSetupRefusal returns the model-facing reason to refuse setting
// up a Skill for a service the run already reaches through a host binding
// (a connected app), naming that binding's tools, or "" when none covers it.
// A connected app is the user's own choice for that service, so the Agent
// uses its tools instead of asking for a second, provider-specific account.
func (d *SkillBindingActionDispatcher) connectedAppSetupRefusal(ctx context.Context, run *AgentRun, deploymentID string, skillID string, candidate *skill.DiscoveryCandidate) (string, error) {
	if run == nil || run.AssignedAgentID != deploymentID {
		return "", nil
	}
	scope := skill.ScopeReference{Kind: run.Scope.Kind, ID: run.Scope.ID}
	set, err := d.catalog.HostBindings(ctx, scope, deploymentID, skill.RunActor{RunID: run.ID, AgentID: deploymentID})
	if err != nil || set == nil || len(set.Bindings) == 0 {
		return "", err
	}
	wanted := map[string]bool{}
	name := skillID
	for _, value := range []string{skillID, candidateName(candidate)} {
		if key := serviceKey(value); key != "" {
			wanted[key] = true
		}
	}
	if candidate != nil && strings.TrimSpace(candidate.Name) != "" {
		name = strings.TrimSpace(candidate.Name)
	}
	for _, binding := range set.Bindings {
		covers := false
		for _, service := range set.Services[binding.ID] {
			covers = covers || wanted[serviceKey(service)]
		}
		if !covers {
			continue
		}
		tools, err := ConnectedAppToolNames(ctx, d.catalog, binding)
		if err != nil {
			return "", err
		}
		listed := tools
		if len(listed) > maximumConnectedAppToolsNamed {
			listed = listed[:maximumConnectedAppToolsNamed]
		}
		message := fmt.Sprintf("%s is already connected as a connected app, so it needs no Skill setup and no other account. Use its tools for this request", name)
		if len(listed) > 0 {
			message += ": " + strings.Join(listed, ", ")
			if len(tools) > len(listed) {
				message += fmt.Sprintf(" and %d more", len(tools)-len(listed))
			}
		}
		return message + ". Do not ask the user to set up the " + name + " Skill.", nil
	}
	return "", nil
}

func candidateName(candidate *skill.DiscoveryCandidate) string {
	if candidate == nil {
		return ""
	}
	return candidate.Name
}

// serviceNoise are words that name the kind of integration, not the service.
var serviceNoise = map[string]bool{
	"mcp": true, "server": true, "skill": true, "skills": true, "api": true, "app": true, "apps": true,
	"official": true, "remote": true, "connector": true, "integration": true, "openseal": true,
}

// serviceKey reduces a Skill ID, catalog slug or display name to the
// service it names: lowercase words without integration noise, so "Linear",
// "linear-mcp" and "skill-linear" all name "linear".
func serviceKey(value string) string {
	words := strings.FieldsFunc(strings.ToLower(value), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	})
	kept := words[:0]
	for _, word := range words {
		if !serviceNoise[word] {
			kept = append(kept, word)
		}
	}
	return strings.Join(kept, "-")
}
