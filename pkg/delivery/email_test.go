package delivery

import (
	"context"
	"testing"

	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestEmailSkillIsGovernedCredentialedAndArtifactSafe(t *testing.T) {
	ctx := context.Background()
	definition := SkillDefinition()
	action := definition.Actions[SendEmail]
	if definition.Version != "1.1.0" {
		t.Fatalf("email semantic contract must use a new immutable version, got %q", definition.Version)
	}
	if action.SemanticArguments["target"] != "to" || action.SemanticArguments["body"] != "body" {
		t.Fatalf("email action must expose portable outreach arguments, got %#v", action.SemanticArguments)
	}
	if definition.ID != SkillID || definition.Transport.Endpoint != SkillID || action.Risk != skill.RiskLevelExternal ||
		action.SideEffect != skill.SideEffectExternal || action.Idempotency != skill.IdempotencyRequired || action.Retry.MaxAttempts != 1 ||
		len(action.Credentials) != 1 || action.Credentials[0].Name != EmailCredentialName || action.Credentials[0].Kind != EmailCredentialKind {
		t.Fatalf("email definition = %#v", definition)
	}
	if len(definition.Installers) != 1 || definition.Installers[0].Kind != "oci" || definition.Installers[0].Package != SkillImage {
		t.Fatalf("delivery Skill OCI installer mismatch: %#v", definition.Installers)
	}
	catalog := skill.NewCatalog()
	if err := catalog.Register(ctx, definition); err != nil {
		t.Fatal(err)
	}
	scope := skill.ScopeReference{Kind: "tenant", ID: "1"}
	if err := catalog.Bind(ctx, &skill.Binding{
		ID: "delivery", Scope: scope, DeploymentID: "agent", SkillID: SkillID, SkillVersion: SkillVersion,
		AllowedActions: []string{SendEmail}, MaximumRisk: skill.RiskLevelExternal,
		Credentials: map[string]skill.CredentialReference{EmailCredentialName: {Kind: EmailCredentialKind, ID: "opaque-vault-reference"}}, Revision: 1,
	}); err != nil {
		t.Fatal(err)
	}
	bound, err := catalog.Resolve(ctx, scope, "agent", SkillID, SkillVersion, SendEmail)
	if err != nil {
		t.Fatal(err)
	}
	input := map[string]interface{}{
		"to": []interface{}{"research@example.com"}, "subject": "Market research", "body": "The cited report is attached.",
		"artifactRefs": []interface{}{map[string]interface{}{"id": "pdf-report", "version": 1}},
	}
	if err = catalog.ValidateInput(ctx, bound, input); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(map[string]interface{}){
		"raw credential":    func(value map[string]interface{}) { value[EmailCredentialName] = "secret" },
		"attachment bytes":  func(value map[string]interface{}) { value["attachmentBase64"] = "JVBERi0=" },
		"sender spoofing":   func(value map[string]interface{}) { value["from"] = "other@example.com" },
		"invalid recipient": func(value map[string]interface{}) { value["to"] = []interface{}{"not-an-email"} },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := make(map[string]interface{}, len(input)+1)
			for key, value := range input {
				candidate[key] = value
			}
			mutate(candidate)
			if err := catalog.ValidateInput(ctx, bound, candidate); err == nil {
				t.Fatal("unsafe email input was accepted")
			}
		})
	}
	output := map[string]interface{}{
		"receiptId": "receipt-1", "status": "accepted", "recipientCount": 1,
		"deliveredAt": "2026-07-13T00:00:00Z", "artifactRefs": input["artifactRefs"],
	}
	if err = catalog.ValidateOutput(ctx, bound, output); err != nil {
		t.Fatal(err)
	}
}
