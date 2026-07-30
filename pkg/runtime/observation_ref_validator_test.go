package runtime

import (
	"context"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestObservationRefActionProposalValidator(t *testing.T) {
	bound := &skill.BoundAction{Action: skill.Action{InputSchema: map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"target": map[string]interface{}{
				"type": "string",
				observationRefSchemaExtension: map[string]interface{}{
					"roles": []string{"button"}, "requireEnabled": true,
				},
			},
		},
	}}}
	run := &AgentRun{Checkpoint: map[string]interface{}{
		"lastAction": map[string]interface{}{
			"status": string(ActionCallStatusSucceeded),
			"result": map[string]interface{}{
				"generation": 5,
				"elements": []map[string]interface{}{
					{"ref": "s5:e1", "role": "button", "state": map[string]interface{}{}},
					{"ref": "s5:e2", "role": "textbox", "state": map[string]interface{}{}},
					{"ref": "s5:e3", "role": "button", "state": map[string]interface{}{"disabled": true}},
				},
			},
		},
	}}
	validator := ObservationRefActionProposalValidator{}

	for _, testCase := range []struct {
		name, target, wantError string
	}{
		{name: "enabled button", target: "s5:e1"},
		{name: "wrong role", target: "s5:e2", wantError: "role \"textbox\""},
		{name: "disabled", target: "s5:e3", wantError: "is disabled"},
		{name: "stale", target: "s4:e1", wantError: "not from the latest observation"},
		{name: "missing", target: "s5:e9", wantError: "absent from the latest observation"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := validator.ValidateActionProposal(context.Background(), ActionProposalValidationInput{
				Run: run, Bound: bound, Arguments: map[string]interface{}{"target": testCase.target},
			})
			if testCase.wantError == "" && err != nil {
				t.Fatal(err)
			}
			if testCase.wantError != "" && (err == nil || !strings.Contains(err.Error(), testCase.wantError)) {
				t.Fatalf("error = %v, want %q", err, testCase.wantError)
			}
		})
	}
}
