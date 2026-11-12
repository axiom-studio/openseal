package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/builtin"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/action/actiontest"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestDefaultActionPolicyProfileImageExceptionIsNarrow(t *testing.T) {
	for _, tc := range []struct {
		name, skillID, action string
		risk                  skill.RiskLevel
		effect                skill.SideEffect
		want                  action.ActionDisposition
	}{
		{"profile", builtin.SkillID, builtin.GenerateProfileImage, skill.RiskLevelWrite, skill.SideEffectWrite, action.ActionDispositionAllow},
		{"other skill", "other", builtin.GenerateProfileImage, skill.RiskLevelWrite, skill.SideEffectWrite, action.ActionDispositionRequireApproval},
		{"other action", builtin.SkillID, "generate_image", skill.RiskLevelWrite, skill.SideEffectWrite, action.ActionDispositionRequireApproval},
		{"higher risk", builtin.SkillID, builtin.GenerateProfileImage, skill.RiskLevelDestructive, skill.SideEffectWrite, action.ActionDispositionRequireApproval},
		{"external effect", builtin.SkillID, builtin.GenerateProfileImage, skill.RiskLevelWrite, skill.SideEffectExternal, action.ActionDispositionRequireApproval},
	} {
		t.Run(tc.name, func(t *testing.T) {
			decision, err := action.NewDefaultActionPolicy().EvaluateAction(context.Background(), action.ActionPolicyInput{Bound: &skill.BoundAction{
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
	store := kerneltest.NewStore(t)
	catalog, scope := actiontest.GovernedCatalog(t)
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
	run := actiontest.ClaimedRun(t, store, scope, now, "worker")
	result, err := action.NewActionCoordinator(store, store, catalog, action.NewDefaultActionPolicy(), nil).Propose(ctx, actiontest.LoopProposeRequest(action.ProposeActionRequest{
		Scope: scope, RunID: run.ID, WorkerID: "worker", DeploymentID: "release-agent",
		SkillID: definition.ID, SkillVersion: definition.Version, Action: builtin.GenerateProfileImage,
		Arguments: map[string]interface{}{"prompt": "A friendly illustrated portrait"},
	}, "profile-1"))
	if err != nil {
		t.Fatal(err)
	}
	if result.Approval != nil || result.Call.Status != models.ActionCallStatusReady {
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
