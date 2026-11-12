package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/skill"
)

func observationRefAction(constraint map[string]interface{}, required ...interface{}) *skill.BoundAction {
	return &skill.BoundAction{Definition: &skill.Definition{ID: "browser", Version: "2"}, Action: skill.Action{Name: "click", InputSchema: map[string]interface{}{
		"type": "object", "required": required,
		"properties": map[string]interface{}{"target": map[string]interface{}{"type": "string", observationRefSchemaExtension: constraint}},
	}}}
}

func observation(id string, status models.ActionCallStatus, generation int, elements ...interface{}) models.ActionCall {
	return models.ActionCall{ID: id, SkillID: "browser", SkillVersion: "2", Action: "observe", Status: status,
		Output: map[string]interface{}{"generation": generation, "elements": elements}}
}

func validateObservationRef(bound *skill.BoundAction, history agentrun.RunActionHistory, arguments map[string]interface{}) error {
	_, err := ObservationRefActionProposalValidator{}.ValidateActionProposal(context.Background(), agentrun.ActionProposalValidationInput{
		Run: &models.AgentRun{ID: "run"}, Bound: bound, Arguments: arguments, History: history,
	})
	return err
}

func requireObservationError(t *testing.T, err error, want string) {
	t.Helper()
	if want == "" {
		if err != nil {
			t.Fatal(err)
		}
		return
	}
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want %q", err, want)
	}
}

func TestObservationRefUsesNewestSameSkillCall(t *testing.T) {
	f := newRunHistoryFixture(t)
	f.add(observation("old", models.ActionCallStatusSucceeded, 4, map[string]interface{}{"ref": "s4:e1", "role": "button"}))
	f.add(models.ActionCall{ID: "new", SkillID: "browser", SkillVersion: "2", Action: "observe", Status: models.ActionCallStatusSucceeded,
		// A compacted output keeps the observation under value.
		Output: map[string]interface{}{"compacted": true, "value": map[string]interface{}{"generation": 5, "elements": []interface{}{
			map[string]interface{}{"ref": "s5:e1", "role": "button", "state": map[string]interface{}{}},
			map[string]interface{}{"ref": "s5:e2", "role": "textbox", "state": map[string]interface{}{}},
			map[string]interface{}{"ref": "s5:e3", "role": "button", "state": map[string]interface{}{"disabled": true}},
		}}}})
	// A VM call of the same Skill is not part of the governed history.
	f.add(models.ActionCall{ID: "vm", Origin: models.ActionOriginVM, SkillID: "browser", SkillVersion: "2", Action: "observe", Status: models.ActionCallStatusFailed})
	bound := observationRefAction(map[string]interface{}{"roles": []string{"button"}, "requireEnabled": true})
	history := f.history()
	for _, testCase := range []struct{ name, target, want string }{
		{name: "enabled button", target: "s5:e1"},
		{name: "older observation", target: "s4:e1", want: "not from the latest observation"},
		{name: "wrong role", target: "s5:e2", want: "role \"textbox\""},
		{name: "disabled", target: "s5:e3", want: "is disabled"},
		{name: "missing", target: "s5:e9", want: "absent from the latest observation"},
		{name: "empty", target: " ", want: "requires a current observation reference"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			requireObservationError(t, validateObservationRef(bound, history, map[string]interface{}{"target": testCase.target}), testCase.want)
		})
	}
}

func TestObservationRefRejectsFailedLatestObservation(t *testing.T) {
	bound := observationRefAction(map[string]interface{}{"roles": []string{"button"}})
	arguments := map[string]interface{}{"target": "s5:e1"}
	requireObservationError(t, validateObservationRef(bound, nil, arguments), "current observation is unavailable")
	f := newRunHistoryFixture(t)
	requireObservationError(t, validateObservationRef(bound, f.history(), arguments), "current observation is unavailable")

	f.add(observation("observe", models.ActionCallStatusSucceeded, 5, map[string]interface{}{"ref": "s5:e1", "role": "button"}))
	f.add(models.ActionCall{ID: "click", SkillID: "browser", SkillVersion: "2", Action: "click", Status: models.ActionCallStatusFailed})
	err := validateObservationRef(bound, f.history(), arguments)
	requireObservationError(t, err, "latest action is not a successful observation; take a new observation")
	var recovery actionAdvanceRecoveryError
	if !errors.As(err, &recovery) {
		t.Fatalf("stale observation is not recoverable by a new observation: %T", err)
	}
}

func TestObservationRefRejectsStaleGeneration(t *testing.T) {
	f := newRunHistoryFixture(t)
	f.add(observation("observe-6", models.ActionCallStatusSucceeded, 6, map[string]interface{}{"ref": "s6:e1", "role": "button"}))
	f.add(observation("observe-7", models.ActionCallStatusSucceeded, 7, map[string]interface{}{"ref": "s7:e1", "role": "button"}))
	bound := observationRefAction(map[string]interface{}{"roles": []string{"button"}})
	history := f.history()
	requireObservationError(t, validateObservationRef(bound, history, map[string]interface{}{"target": "s6:e1"}), "reference is not from the latest observation; take a new observation")
	requireObservationError(t, validateObservationRef(bound, history, map[string]interface{}{"target": "s7:e1"}), "")
}

func TestObservationRefIgnoresOtherSkillCalls(t *testing.T) {
	f := newRunHistoryFixture(t)
	f.add(observation("observe", models.ActionCallStatusSucceeded, 3, map[string]interface{}{"ref": "s3:e2", "role": "button"}))
	// Unrelated reads after the observation, in the same Run.
	f.add(models.ActionCall{ID: "mail", SkillID: "mail", SkillVersion: "1", Action: "search", Status: models.ActionCallStatusSucceeded,
		Output: map[string]interface{}{"generation": 9}})
	f.add(models.ActionCall{ID: "files", SkillID: "files", SkillVersion: "1", Action: "read", Status: models.ActionCallStatusFailed})
	bound := observationRefAction(map[string]interface{}{"roles": []string{"button"}})
	requireObservationError(t, validateObservationRef(bound, f.history(), map[string]interface{}{"target": "s3:e2"}), "")
}

func TestPayButtonRequiresFinalPayFlag(t *testing.T) {
	f := newRunHistoryFixture(t)
	f.add(observation("checkout", models.ActionCallStatusSucceeded, 11,
		map[string]interface{}{"ref": "s11:e5", "role": "button", "finalPay": true, "state": map[string]interface{}{}},
		map[string]interface{}{"ref": "s11:e12", "role": "button", "state": map[string]interface{}{}},
	))
	constraint := map[string]interface{}{"roles": []interface{}{"button"}, "requireFlags": []interface{}{"finalPay"}}
	history := f.history()
	requireObservationError(t, validateObservationRef(observationRefAction(constraint), history, map[string]interface{}{"target": "s11:e5"}), "")
	requireObservationError(t, validateObservationRef(observationRefAction(constraint), history, map[string]interface{}{"target": "s11:e12"}), "not marked finalPay")
	// An optional reference may be omitted; a required one may not.
	requireObservationError(t, validateObservationRef(observationRefAction(constraint), history, map[string]interface{}{}), "")
	requireObservationError(t, validateObservationRef(observationRefAction(constraint, "target"), history, map[string]interface{}{}), "requires a current observation reference")
}
