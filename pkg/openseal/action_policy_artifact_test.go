package openseal

import (
	"testing"

	"github.com/axiom-studio/openseal/pkg/builtin"
	"github.com/axiom-studio/openseal/pkg/presentation"
)

func TestBoundedConversationArtifactsDoNotRequireApproval(t *testing.T) {
	engine, err := New()
	if err != nil {
		t.Fatal(err)
	}
	run := &AgentRun{ID: "run", Scope: Scope{Kind: "tenant", ID: "one"}}
	for _, tc := range []struct {
		name       string
		definition *SkillDefinition
		action     string
		want       ActionDisposition
	}{
		{"generated image", builtin.SkillDefinition(), builtin.GenerateImage, ActionDispositionAllow},
		{"private app", builtin.SkillDefinition(), builtin.SaveApp, ActionDispositionAllow},
		{"present files", builtin.SkillDefinition(), builtin.Present, ActionDispositionAllow},
		{"sheet", builtin.SkillDefinition(), builtin.CreateSheet, ActionDispositionAllow},
		{"app data", builtin.SkillDefinition(), builtin.SaveAppData, ActionDispositionAllow},
		{"read app data", builtin.SkillDefinition(), builtin.ReadAppData, ActionDispositionAllow},
		{"picture style", builtin.SkillDefinition(), builtin.SetImageStyle, ActionDispositionAllow},
		{"update visual", presentation.SkillDefinition(), presentation.UpdateVisual, ActionDispositionAllow},
		{"public app", builtin.SkillDefinition(), builtin.PublishApp, ActionDispositionRequireApproval},
		{"PDF", builtin.SkillDefinition(), builtin.CreatePDF, ActionDispositionAllow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			decision, policyErr := engine.evaluateAgentActionAuthority(t.Context(), ActionPolicyInput{
				Run:   run,
				Bound: &BoundSkillAction{Definition: tc.definition, Action: tc.definition.Actions[tc.action], Binding: &SkillBinding{ID: "binding"}},
			})
			if policyErr != nil || decision.Disposition != tc.want {
				t.Fatalf("policy = %#v, %v; want %s", decision, policyErr, tc.want)
			}
		})
	}
	spoofed := builtin.SkillDefinition()
	spoofed.Version = "older-version"
	decision, err := engine.evaluateAgentActionAuthority(t.Context(), ActionPolicyInput{
		Run:   run,
		Bound: &BoundSkillAction{Definition: spoofed, Action: spoofed.Actions[builtin.GenerateImage], Binding: &SkillBinding{ID: "binding"}},
	})
	if err != nil || decision.Disposition != ActionDispositionRequireApproval {
		t.Fatalf("unrecognized image capability bypassed approval: %#v, %v", decision, err)
	}
}
