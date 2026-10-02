package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/builtin"
	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestDefaultActionPolicyProfileImageExceptionIsNarrow(t *testing.T) {
	for _, tc := range []struct {
		name, skillID, action string
		risk                  skill.RiskLevel
		effect                skill.SideEffect
		want                  ActionDisposition
	}{
		{"profile", builtin.SkillID, builtin.GenerateProfileImage, skill.RiskLevelWrite, skill.SideEffectWrite, ActionDispositionAllow},
		{"other skill", "other", builtin.GenerateProfileImage, skill.RiskLevelWrite, skill.SideEffectWrite, ActionDispositionRequireApproval},
		{"other action", builtin.SkillID, "generate_image", skill.RiskLevelWrite, skill.SideEffectWrite, ActionDispositionRequireApproval},
		{"higher risk", builtin.SkillID, builtin.GenerateProfileImage, skill.RiskLevelDestructive, skill.SideEffectWrite, ActionDispositionRequireApproval},
		{"external effect", builtin.SkillID, builtin.GenerateProfileImage, skill.RiskLevelWrite, skill.SideEffectExternal, ActionDispositionRequireApproval},
	} {
		t.Run(tc.name, func(t *testing.T) {
			decision, err := NewDefaultActionPolicy().EvaluateAction(context.Background(), ActionPolicyInput{Bound: &skill.BoundAction{
				Definition: &skill.Definition{ID: tc.skillID}, Binding: &skill.Binding{},
				Action: skill.Action{Name: tc.action, Risk: tc.risk, SideEffect: tc.effect},
			}})
			if err != nil || decision.Disposition != tc.want {
				t.Fatalf("decision = %#v, error = %v", decision, err)
			}
		})
	}
}

func TestProfileImageProposalProceedsWithoutApproval(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	catalog, scope := governedActionCatalog(t)
	definition := builtin.SkillDefinition()
	if err := catalog.Register(ctx, definition); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Bind(ctx, &skill.Binding{
		ID: "profile", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "release-agent",
		SkillID: definition.ID, SkillVersion: definition.Version, AllowedActions: []string{builtin.GenerateProfileImage}, MaximumRisk: skill.RiskLevelWrite, Revision: 1,
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	run := claimedActionRun(t, store, scope, now, "worker")
	result, err := NewActionCoordinator(store, store, catalog, NewDefaultActionPolicy()).Propose(ctx, ProposeActionRequest{
		Scope: scope, RunID: run.ID, WorkerID: "worker", DeploymentID: "release-agent",
		SkillID: definition.ID, SkillVersion: definition.Version, Action: builtin.GenerateProfileImage,
		Arguments: map[string]interface{}{"prompt": "A friendly illustrated portrait"}, IdempotencyKey: "profile-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Approval != nil || result.Call.Status != ActionCallStatusReady {
		t.Fatalf("profile generation created an approval: %#v", result)
	}
	// The cosmetic exception does not let the caller choose another Agent.
	bound, err := catalog.Resolve(ctx, skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, "release-agent", definition.ID, definition.Version, builtin.GenerateProfileImage)
	if err != nil {
		t.Fatal(err)
	}
	err = catalog.ValidateInput(ctx, bound, map[string]interface{}{"prompt": "portrait", "target": "other-agent"})
	if err == nil {
		t.Fatal("profile action accepted a target override")
	}
}
