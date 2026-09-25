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
		{"private site", builtin.SkillDefinition(), builtin.CreateSite, ActionDispositionAllow},
		{"site update", builtin.SkillDefinition(), builtin.UpdateSite, ActionDispositionAllow},
		{"chart", presentation.SkillDefinition(), presentation.PlotChart, ActionDispositionAllow},
		{"conversation surface", presentation.SkillDefinition(), presentation.Publish, ActionDispositionAllow},
		{"public site", builtin.SkillDefinition(), builtin.PublishSite, ActionDispositionRequireApproval},
		{"PDF", builtin.SkillDefinition(), builtin.CreatePDF, ActionDispositionRequireApproval},
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
