package runtime

import (
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runtime/activity"
)

func TestTurnActionSelectionRefusesDuplicateNamesEvenWithExactBinding(t *testing.T) {
	actions := []capability.ModelAction{{Name: "release.deploy", Version: "1.0.0", BindingID: "account-a", BindingRevision: 1}, {Name: "release.deploy", Version: "2.0.0", BindingID: "account-b", BindingRevision: 1}}
	for _, request := range []activity.TurnAction{
		{Capability: "release.deploy", ExpectedSkillVersion: "1.0.0"},
		{Capability: "release.deploy", BindingID: "account-a", BindingRevision: 1},
	} {
		if selected, err := selectTurnModelAction(actions, request); selected != nil || err == nil {
			t.Fatalf("duplicate capability names were routed: %#v %v", selected, err)
		}
	}
	unique := []capability.ModelAction{actions[0], {Name: "release.rollback", Version: "1.0.0", BindingID: "account-b", BindingRevision: 1}}
	for _, expected := range []string{"", "1.0.0"} {
		selected, err := selectTurnModelAction(unique, activity.TurnAction{Capability: "release.deploy", BindingID: "account-a", BindingRevision: 1, ExpectedSkillVersion: expected})
		if err != nil || selected == nil || selected.BindingID != "account-a" {
			t.Fatalf("valid exact binding was rejected: %#v %v", selected, err)
		}
	}
	if selected, err := selectTurnModelAction(unique, activity.TurnAction{Capability: "release.deploy", BindingID: "account-b", BindingRevision: 1}); selected != nil || err != nil {
		t.Fatalf("a proposal for another binding was routed: %#v %v", selected, err)
	}
}
