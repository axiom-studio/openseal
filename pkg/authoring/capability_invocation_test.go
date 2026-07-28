package authoring

import (
	"errors"
	"testing"

	"github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/workforce"
)

func TestObjectiveCapabilityInputsUseExactAuthorizedActionContract(t *testing.T) {
	candidate := WorkforceCandidate{Agents: []*agent.AgentDefinition{{
		ID: "reddit-researcher",
		ObjectiveTemplates: []workforce.ObjectiveTemplate{{
			ID: "daily-scan", Title: "Daily scan", Goal: "Inspect permitted Reddit sources",
			Cadence: map[string]interface{}{
				"runTemplate": map[string]interface{}{"capability": map[string]interface{}{
					"skillId": "skill-browser", "skillVersion": "2.0.1", "action": "browser-open",
					"inputs": map[string]interface{}{"url": "https://www.reddit.com/r/openseal", "maxItems": float64(10)},
				}},
			},
		}},
	}}}
	catalog := CapabilityCatalog{Skills: map[string]SkillCapability{
		"skill-browser": {
			ID: "skill-browser", Version: "2.0.1", Actions: []string{"browser-open"},
			ActionContracts: map[string]SkillActionContract{"browser-open": {InputSchema: map[string]interface{}{
				"type": "object", "additionalProperties": false,
				"properties": map[string]interface{}{
					"sessionId": map[string]interface{}{"type": "string"},
					"url":       map[string]interface{}{"type": "string"},
				},
				"required": []interface{}{"sessionId", "url"},
			}}},
		},
	}}

	issues := validateObjectiveCapabilityInputs(&candidate, catalog, false)
	if len(issues) != 1 || issues[0].Code != "capability_action_input_invalid" || issues[0].Path == "" {
		t.Fatalf("invalid invocation issues = %#v", issues)
	}

	invocation := candidate.Agents[0].ObjectiveTemplates[0].Cadence["runTemplate"].(map[string]interface{})["capability"].(map[string]interface{})
	invocation["inputs"] = map[string]interface{}{
		"sessionId": "reddit-daily-scan", "url": "https://www.reddit.com/r/openseal",
	}
	if issues = validateObjectiveCapabilityInputs(&candidate, catalog, false); len(issues) != 0 {
		t.Fatalf("valid invocation issues = %#v", issues)
	}
}

func TestApplyPlacementRevalidatesObjectiveCapabilityInputs(t *testing.T) {
	candidate := WorkforceCandidate{Agents: []*agent.AgentDefinition{{
		ID: "reddit-researcher",
		ObjectiveTemplates: []workforce.ObjectiveTemplate{{
			ID: "daily-scan", Title: "Daily scan", Goal: "Inspect permitted Reddit sources",
			Cadence: map[string]interface{}{"runTemplate": map[string]interface{}{"capability": map[string]interface{}{
				"skillId": "skill-browser", "skillVersion": "2.0.1", "action": "browser-open",
				"inputs": map[string]interface{}{"url": "https://www.reddit.com/r/openseal"},
			}}},
		}},
	}}}
	changeSet := &ChangeSet{
		Result: CompileResult{Candidate: candidate, Valid: true},
		Catalog: CapabilityCatalog{Skills: map[string]SkillCapability{"skill-browser": {
			ID: "skill-browser", Version: "2.0.1", Actions: []string{"browser-open"},
			ActionContracts: map[string]SkillActionContract{"browser-open": {InputSchema: map[string]interface{}{
				"type": "object", "additionalProperties": false,
				"properties": map[string]interface{}{
					"sessionId": map[string]interface{}{"type": "string"},
					"url":       map[string]interface{}{"type": "string"},
				},
				"required": []interface{}{"sessionId", "url"},
			}}},
		}}},
	}

	err := validateApplyPlacement(changeSet)
	var readiness *ChangeSetReadinessError
	if !errors.As(err, &readiness) || len(readiness.Issues) != 1 || readiness.Issues[0].Code != "skill_binding_capability_action_input_invalid" {
		t.Fatalf("apply validation error = %#v", err)
	}
}
