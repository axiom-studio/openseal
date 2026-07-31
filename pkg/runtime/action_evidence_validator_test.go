package runtime

import (
	"context"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestRequiredActionEvidenceValidatorUsesDurableSucceededHistory(t *testing.T) {
	bound := &skill.BoundAction{Definition: &skill.Definition{ID: "browser", Version: "2"}, Action: skill.Action{
		Name: "commit", RequiredEvidence: []skill.ActionEvidenceRequirement{{Action: "fill", MatchingArguments: []string{"sessionId"}}},
	}}
	run := &AgentRun{Checkpoint: appendActionHistory(nil, &ActionCall{
		ID: "fill", SkillID: "browser", SkillVersion: "2", Action: "fill", Status: ActionCallStatusSucceeded,
		Arguments: map[string]interface{}{"sessionId": "session-a", "value": "exact draft"},
	})}
	validator := RequiredActionEvidenceValidator{}
	if _, err := validator.ValidateActionProposal(context.Background(), ActionProposalValidationInput{
		Run: run, Bound: bound, Arguments: map[string]interface{}{"sessionId": "session-a"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := validator.ValidateActionProposal(context.Background(), ActionProposalValidationInput{
		Run: run, Bound: bound, Arguments: map[string]interface{}{"sessionId": "session-b"},
	}); err == nil || !strings.Contains(err.Error(), "successful fill evidence") {
		t.Fatalf("mismatched session evidence error = %v", err)
	}
	run.Checkpoint = map[string]interface{}{"summary": "I filled the form"}
	if _, err := validator.ValidateActionProposal(context.Background(), ActionProposalValidationInput{
		Run: run, Bound: bound, Arguments: map[string]interface{}{"sessionId": "session-a"},
	}); err == nil || !strings.Contains(err.Error(), "successful fill evidence") {
		t.Fatalf("unsubstantiated summary error = %v", err)
	}
}
