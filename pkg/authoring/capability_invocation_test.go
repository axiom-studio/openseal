package authoring

import (
	"errors"
	"testing"

	"github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/axiom-studio/openseal/pkg/workforce"
)

func browserActionCandidate() WorkforceCandidate {
	return WorkforceCandidate{Agents: []*agent.AgentDefinition{{
		ID:                 "reddit-researcher",
		ObjectiveTemplates: []workforce.ObjectiveTemplate{{ID: "daily-scan", Title: "Daily scan", Goal: "Inspect permitted Reddit sources", Priority: 1}},
		Runbook: &runbook.Definition{
			APIVersion: runbook.APIVersion, ID: "scan", Version: "1.0.0", Name: "Scan",
			Entrypoints: map[string]string{"scan": "open"},
			Triggers:    map[string]runbook.Trigger{"daily": {Kind: runbook.TriggerSchedule, Schedule: &runbook.Schedule{Cron: "0 0 0 * * *", Timezone: "UTC"}, Entrypoint: "scan", ObjectiveID: WorkforceObjectiveKey("agent", "reddit-researcher", "daily-scan")}},
			Steps: map[string]runbook.Step{
				"open": {Kind: runbook.StepAction, Action: &runbook.ActionStep{SkillID: "skill-browser", SkillVersion: "2.0.1", Action: "browser-open", Arguments: map[string]runbook.Value{"url": literalActionValue("https://www.reddit.com/r/openseal"), "maxItems": literalActionValue(10)}, ResultPath: "/results/open", Next: "done"}},
				"done": {Kind: runbook.StepEnd, End: &runbook.EndStep{}},
			},
		},
	}}}
}

func browserActionCatalog() CapabilityCatalog {
	return CapabilityCatalog{Skills: map[string]SkillCapability{"skill-browser": {
		ID: "skill-browser", Version: "2.0.1", Actions: []string{"browser-open"},
		ActionContracts: map[string]SkillActionContract{"browser-open": {InputSchema: map[string]interface{}{
			"type": "object", "additionalProperties": false,
			"properties": map[string]interface{}{"sessionId": map[string]interface{}{"type": "string"}, "url": map[string]interface{}{"type": "string"}},
			"required":   []interface{}{"sessionId", "url"},
		}}},
	}}}
}

func TestRunbookCapabilityInputsUseExactAuthorizedActionContract(t *testing.T) {
	candidate, catalog := browserActionCandidate(), browserActionCatalog()
	issues := validateObjectiveCapabilityInputs(&candidate, catalog, false)
	if len(issues) != 1 || issues[0].Code != "capability_action_input_invalid" || issues[0].Path == "" {
		t.Fatalf("invalid invocation issues = %#v", issues)
	}
	action := candidate.Agents[0].Runbook.Steps["open"].Action
	action.Arguments = map[string]runbook.Value{"sessionId": literalActionValue("reddit-daily-scan"), "url": literalActionValue("https://www.reddit.com/r/openseal")}
	if issues = validateObjectiveCapabilityInputs(&candidate, catalog, false); len(issues) != 0 {
		t.Fatalf("valid invocation issues = %#v", issues)
	}
}

func TestApplyPlacementRevalidatesRunbookCapabilityInputs(t *testing.T) {
	changeSet := &ChangeSet{Result: CompileResult{Candidate: browserActionCandidate(), Valid: true}, Catalog: browserActionCatalog()}
	err := validateApplyPlacement(changeSet)
	var readiness *ChangeSetReadinessError
	if !errors.As(err, &readiness) || len(readiness.Issues) != 1 || readiness.Issues[0].Code != "skill_binding_capability_action_input_invalid" {
		t.Fatalf("apply validation error = %#v", err)
	}
}
