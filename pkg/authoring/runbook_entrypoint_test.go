package authoring

import (
	"testing"

	"github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/axiom-studio/openseal/pkg/workforce"
)

func TestCandidateRejectsObjectiveEntrypointWithoutEmbeddedAgentRunbook(t *testing.T) {
	candidate := objectiveRunbookCandidate()
	issues := validateCandidate(&candidate, nil)
	if !hasValidationCode(issues, "runbook_entrypoint_unavailable") {
		t.Fatalf("dangling Runbook entrypoint issues = %#v", issues)
	}

	candidate.Agents[0].Runbook = objectiveTestRunbook()
	issues = validateCandidate(&candidate, nil)
	if hasValidationCode(issues, "runbook_entrypoint_unavailable") {
		t.Fatalf("embedded Runbook entrypoint was rejected: %#v", issues)
	}
}

func TestCandidateRejectsAmbiguousCapabilityAndRunbookEntrypoint(t *testing.T) {
	candidate := objectiveRunbookCandidate()
	candidate.Agents[0].Runbook = objectiveTestRunbook()
	runTemplate := candidate.Agents[0].ObjectiveTemplates[0].Cadence["runTemplate"].(map[string]interface{})
	runTemplate["capability"] = map[string]interface{}{
		"skillId": "reader", "skillVersion": "1.0.0", "action": "read",
	}
	issues := validateCandidate(&candidate, nil)
	if !hasValidationCode(issues, "runbook_entrypoint_conflicts_with_capability") {
		t.Fatalf("ambiguous operation issues = %#v", issues)
	}
}

func TestCandidateValidatesEventEntrypointAgainstAssignedAgentRunbook(t *testing.T) {
	candidate := objectiveRunbookCandidate()
	candidate.Agents[0].ObjectiveTemplates[0].Cadence = nil
	candidate.Agents[0].ObjectiveTemplates[0].EventRules = map[string]interface{}{
		"version": "1",
		"rules": []interface{}{map[string]interface{}{
			"id": "daily", "eventType": "timer.daily", "assignedAgentId": "operator",
			"runTemplate": map[string]interface{}{"entrypoint": "operate"},
		}},
	}
	if issues := validateCandidate(&candidate, nil); !hasValidationCode(issues, "runbook_entrypoint_unavailable") {
		t.Fatalf("dangling event Runbook entrypoint issues = %#v", issues)
	}
	candidate.Agents[0].Runbook = objectiveTestRunbook()
	if issues := validateCandidate(&candidate, nil); hasValidationCode(issues, "runbook_entrypoint_unavailable") {
		t.Fatalf("embedded event Runbook entrypoint was rejected: %#v", issues)
	}
}

func objectiveRunbookCandidate() WorkforceCandidate {
	return WorkforceCandidate{Agents: []*agent.AgentDefinition{{
		ID: "operator", Version: "1.0.0", DisplayName: "Operator", Purpose: "Operate deterministically",
		SystemPrompt: "Use the reviewed operation.", Authority: agent.AuthorityPolicy{MaximumRisk: capability.RiskLevelRead, MaxConcurrentRuns: 1},
		ObjectiveTemplates: []workforce.ObjectiveTemplate{{
			ID: "operate", Title: "Operate", Goal: "Run the reviewed operation", Priority: 1,
			Cadence: map[string]interface{}{
				"type": "interval", "intervalSeconds": float64(3600), "assignedAgentId": "operator",
				"runTemplate": map[string]interface{}{"entrypoint": "operate"},
			},
		}},
	}}}
}

func objectiveTestRunbook() *runbook.Definition {
	return &runbook.Definition{
		APIVersion: runbook.APIVersion, ID: "operator-operations", Version: "1.0.0", Name: "Operator operations",
		Entrypoints: map[string]string{"operate": "done"},
		Steps:       map[string]runbook.Step{"done": {Kind: runbook.StepEnd, End: &runbook.EndStep{}}},
	}
}
