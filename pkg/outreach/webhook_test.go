package outreach

import (
	"context"
	"testing"

	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestWebhookOutreachSkillIsExactExternalAndReceiptBound(t *testing.T) {
	definition := SkillDefinition()
	action := definition.Actions[PostReply]
	if definition.ID != SkillID || definition.Transport.Endpoint != SkillID || action.SideEffect != skill.SideEffectExternal ||
		action.Risk != skill.RiskLevelExternal || action.Idempotency != skill.IdempotencyRequired || action.Retry.MaxAttempts != 1 ||
		action.SemanticArguments["target"] != "targetUri" || action.SemanticArguments["body"] != "body" {
		t.Fatalf("definition = %#v", definition)
	}
	if len(definition.Installers) != 1 || definition.Installers[0].Kind != "oci" || definition.Installers[0].Package != SkillImage {
		t.Fatalf("outreach Skill OCI installer mismatch: %#v", definition.Installers)
	}
	catalog := skill.NewCatalog()
	if err := catalog.Register(context.Background(), definition); err != nil {
		t.Fatal(err)
	}
	scope := skill.ScopeReference{Kind: "tenant", ID: "1"}
	if err := catalog.Bind(context.Background(), &skill.Binding{
		ID: "outreach", Scope: scope, DeploymentID: "agent", SkillID: SkillID, SkillVersion: SkillVersion,
		AllowedActions: []string{PostReply}, MaximumRisk: skill.RiskLevelExternal, Revision: 1,
	}); err != nil {
		t.Fatal(err)
	}
	bound, err := catalog.Resolve(context.Background(), scope, "agent", SkillID, SkillVersion, PostReply)
	if err != nil {
		t.Fatal(err)
	}
	input := map[string]interface{}{"targetUri": "https://forum.example/replies/1", "body": "Disclosure: I build OpenSeal. What was difficult?"}
	if err := catalog.ValidateInput(context.Background(), bound, input); err != nil {
		t.Fatal(err)
	}
	unsafe := map[string]interface{}{"targetUri": input["targetUri"], "body": input["body"], PolicyDecisionTransportKey: map[string]interface{}{"enabled": true}}
	if err := catalog.ValidateInput(context.Background(), bound, unsafe); err == nil {
		t.Fatal("model-visible trusted policy decision was accepted")
	}
}
