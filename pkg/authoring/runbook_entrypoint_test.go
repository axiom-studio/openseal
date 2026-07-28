package authoring

import (
	"testing"

	"github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/axiom-studio/openseal/pkg/workforce"
)

func TestCandidateRejectsRunbookTriggerWithoutObjective(t *testing.T) {
	candidate := objectiveRunbookCandidate("")
	if issues := validateCandidate(&candidate, nil); !hasValidationCode(issues, "runbook_trigger_objective_required") {
		t.Fatalf("missing Objective issues = %#v", issues)
	}
}

func TestCandidateRejectsRunbookTriggerForUnknownObjective(t *testing.T) {
	candidate := objectiveRunbookCandidate(WorkforceObjectiveKey("agent", "operator", "missing"))
	if issues := validateCandidate(&candidate, nil); !hasValidationCode(issues, "runbook_trigger_objective_unknown") {
		t.Fatalf("unknown Objective issues = %#v", issues)
	}
}

func TestCandidateAcceptsRunbookTriggerForAgentObjective(t *testing.T) {
	candidate := objectiveRunbookCandidate(WorkforceObjectiveKey("agent", "operator", "operate"))
	if issues := validateCandidate(&candidate, nil); len(issues) != 0 {
		t.Fatalf("valid Objective-owned Runbook rejected = %#v", issues)
	}
}

func objectiveRunbookCandidate(objectiveID string) WorkforceCandidate {
	return WorkforceCandidate{Agents: []*agent.AgentDefinition{{
		ID: "operator", Version: "1.0.0", DisplayName: "Operator", Purpose: "Operate deterministically",
		SystemPrompt: "Use the reviewed operation.", Authority: agent.AuthorityPolicy{MaximumRisk: capability.RiskLevelRead, MaxConcurrentRuns: 1},
		ObjectiveTemplates: []workforce.ObjectiveTemplate{{ID: "operate", Title: "Operate", Goal: "Run the reviewed operation", Priority: 1}},
		Runbook: &runbook.Definition{
			APIVersion: runbook.APIVersion, ID: "operator-operations", Version: "1.0.0", Name: "Operator operations",
			Entrypoints: map[string]string{"operate": "done"},
			Triggers: map[string]runbook.Trigger{"hourly": {
				Kind: runbook.TriggerSchedule, Schedule: &runbook.Schedule{Cron: "0 0 * * * *", Timezone: "UTC"},
				Entrypoint: "operate", ObjectiveID: objectiveID,
			}},
			Steps: map[string]runbook.Step{"done": {Kind: runbook.StepEnd, End: &runbook.EndStep{}}},
		},
	}}}
}
