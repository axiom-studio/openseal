package authoring

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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
	issues := validateCandidate(&candidate, nil)
	if !hasValidationCode(issues, "runbook_trigger_objective_unknown") {
		t.Fatalf("unknown Objective issues = %#v", issues)
	}
	if encoded := fmt.Sprint(issues); !strings.Contains(encoded, "agent:operator:operate") {
		t.Fatalf("unknown Objective repair options = %#v", issues)
	}
}

func TestCandidateAcceptsRunbookTriggerForAgentObjective(t *testing.T) {
	candidate := objectiveRunbookCandidate(WorkforceObjectiveKey("agent", "operator", "operate"))
	if issues := validateCandidate(&candidate, nil); len(issues) != 0 {
		t.Fatalf("valid Objective-owned Runbook rejected = %#v", issues)
	}
}

func TestCompilerCanonicalizesUniqueLocalRunbookObjectiveReference(t *testing.T) {
	candidate := objectiveRunbookCandidate("operate")
	payload, err := json.Marshal(GenerationResponse{Candidate: candidate})
	if err != nil {
		t.Fatal(err)
	}
	compiler, _ := NewCompiler(staticGenerator{payload: payload})
	result, err := compiler.Compile(t.Context(), GenerateRequest{Mode: ModeCreate, Prompt: "Create one Agent with one Objective and an hourly operation."})
	if err != nil || result == nil {
		t.Fatalf("compile result = %#v, err = %v", result, err)
	}
	trigger := result.Candidate.Agents[0].Runbook.Triggers["hourly"]
	want := WorkforceObjectiveKey("agent", "operator", "operate")
	if trigger.ObjectiveID != want {
		t.Fatalf("canonical Objective reference = %q, want %q", trigger.ObjectiveID, want)
	}
	if issues := validateCandidate(&result.Candidate, nil); len(issues) != 0 {
		t.Fatalf("canonicalized candidate rejected = %#v", issues)
	}
}

func TestCompilerLeavesAmbiguousLocalRunbookObjectiveReferenceForRepair(t *testing.T) {
	candidate := objectiveRunbookCandidate("operate")
	candidate.Agents = append(candidate.Agents, &agent.AgentDefinition{
		ID: "reviewer", Version: "1.0.0", DisplayName: "Reviewer", Purpose: "Review operations", SystemPrompt: "Review operations.",
		Authority:          agent.AuthorityPolicy{MaximumRisk: capability.RiskLevelRead, MaxConcurrentRuns: 1},
		ObjectiveTemplates: []workforce.ObjectiveTemplate{{ID: "operate", Title: "Operate together", Goal: "Coordinate the operation", Priority: 1}},
	})
	canonicalizeGeneratedRunbookObjectiveReferences(&candidate)
	if got := candidate.Agents[0].Runbook.Triggers["hourly"].ObjectiveID; got != "operate" {
		t.Fatalf("ambiguous Objective reference was rewritten to %q", got)
	}
	issues := validateCandidate(&candidate, nil)
	if !hasValidationCode(issues, "runbook_trigger_objective_unknown") {
		t.Fatalf("ambiguous Objective reference issues = %#v", issues)
	}
}

func TestCompilerNeverPersistsUnknownRunbookObjectiveAsUserRepairPlan(t *testing.T) {
	candidate := objectiveRunbookCandidate(WorkforceObjectiveKey("agent", "operator", "missing"))
	payload, err := json.Marshal(GenerationResponse{Candidate: candidate})
	if err != nil {
		t.Fatal(err)
	}
	compiler, _ := NewCompiler(staticGenerator{payload: payload})
	result, err := compiler.Compile(t.Context(), GenerateRequest{Mode: ModeCreate, Prompt: "Create one Agent with one Objective and an hourly operation."})
	var contractError *ContractGenerationError
	if result != nil || !errors.As(err, &contractError) || !strings.Contains(contractError.Diagnostic, "runbook_trigger_objective_unknown") {
		t.Fatalf("result=%#v contractError=%#v err=%v", result, contractError, err)
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
