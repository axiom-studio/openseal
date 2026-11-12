package runtime

import (
	"reflect"
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestSkillUpgradeCompatibilityIncludesExecutableAdapterContracts(t *testing.T) {
	base := &skill.Definition{ID: "skill", Version: "1", Actions: map[string]skill.Action{"read": {Name: "read", InputSchema: map[string]interface{}{"type": "object"}, Risk: skill.RiskLevelRead, SideEffect: skill.SideEffectRead}}, ConversationAdapters: map[string]skill.ConversationAdapter{"conversation": {Name: "Before", Provider: "provider", SubjectEvidence: []capability.ConversationSubjectEvidence{{Action: "read", SubjectPath: "channel"}}}}, CallbackAdapters: map[string]skill.CallbackAdapter{"callback": {Name: "Before", Provider: "provider", EventTypes: []string{"received"}}}}
	binding := &skill.Binding{AllowedActions: []string{"read"}, EnabledConversationAdapters: []string{"conversation"}, EnabledCallbackAdapters: []string{"callback"}}
	for _, test := range []struct {
		name   string
		change func(*skill.Definition)
		code   string
	}{
		{"operation policy", func(d *skill.Definition) {
			a := d.Actions["read"]
			a.ExternalOperationPolicy = skill.ExternalOperationRequired
			d.Actions["read"] = a
		}, "action_contract_changed"},
		{"action transport", func(d *skill.Definition) {
			a := d.Actions["read"]
			a.Transport = &skill.TransportReference{Kind: "http", Endpoint: "changed"}
			d.Actions["read"] = a
		}, "action_contract_changed"},
		{"subject receipt", func(d *skill.Definition) {
			a := d.ConversationAdapters["conversation"]
			a.SubjectEvidence = []capability.ConversationSubjectEvidence{{Action: "read", SubjectPath: "other"}}
			d.ConversationAdapters["conversation"] = a
		}, "conversation_adapter_contract_changed"},
		{"callback acquisition", func(d *skill.Definition) {
			a := d.CallbackAdapters["callback"]
			a.EventTypes = []string{"other"}
			d.CallbackAdapters["callback"] = a
		}, "callback_adapter_contract_changed"},
		{"descriptions only", func(d *skill.Definition) {
			a := d.ConversationAdapters["conversation"]
			a.Name = "After"
			a.Description = "Revised copy"
			d.ConversationAdapters["conversation"] = a
			a2 := d.CallbackAdapters["callback"]
			a2.Name = "After"
			d.CallbackAdapters["callback"] = a2
		}, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			target := cloneUpgradeDefinition(t, base)
			test.change(target)
			findings := append(compareUpgradeContracts(base, target, binding.AllowedActions, nil), compareUpgradeAdapterContracts(base, target, binding)...)
			if test.code == "" {
				if len(findings) != 0 {
					t.Fatalf("copy triggered approval: %#v", findings)
				}
				return
			}
			found := false
			for _, finding := range findings {
				found = found || finding.Code == test.code
			}
			if !found {
				t.Fatalf("missing %s: %#v", test.code, findings)
			}
		})
	}
	unchanged := cloneUpgradeDefinition(t, base)
	if !reflect.DeepEqual(base, unchanged) {
		t.Fatal("comparison mutated original")
	}
}
