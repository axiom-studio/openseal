package authoring

import (
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
)

func TestValidateCredentialPlacementUsesRequiredKindsAndAuthorizedOpaqueChoices(t *testing.T) {
	candidate := &WorkforceCandidate{Agents: []*agent.AgentDefinition{{ID: "sre"}, {ID: "observer"}}}
	required := map[string][]string{"sre": {"kubernetes-cluster"}}
	choices := []capability.CredentialBindingChoice{
		{Reference: capability.CredentialReference{Kind: "kubernetes-cluster", ID: "cluster://7"}, DisplayName: "Development"},
		{Reference: capability.CredentialReference{Kind: "kubernetes-cluster", ID: "cluster://8"}, DisplayName: "Production"},
	}
	valid := ChangeSetPlacement{CredentialReferences: map[string]map[string]capability.CredentialReference{
		"sre": {"kubernetes-cluster": {Kind: "kubernetes-cluster", ID: "cluster://7"}},
	}}
	if err := ValidateCredentialPlacement(candidate, required, valid, choices); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name      string
		agentID   string
		key       string
		reference capability.CredentialReference
		message   string
	}{
		{name: "requirement name is not a kind", agentID: "sre", key: "clusterId", reference: capability.CredentialReference{Kind: "kubernetes-cluster", ID: "cluster://7"}, message: "must match credential kind"},
		{name: "unrequired kind", agentID: "observer", key: "kubernetes-cluster", reference: capability.CredentialReference{Kind: "kubernetes-cluster", ID: "cluster://7"}, message: "does not require"},
		{name: "unknown agent", agentID: "other", key: "kubernetes-cluster", reference: capability.CredentialReference{Kind: "kubernetes-cluster", ID: "cluster://7"}, message: "not part of this workforce"},
		{name: "stale reference", agentID: "sre", key: "kubernetes-cluster", reference: capability.CredentialReference{Kind: "kubernetes-cluster", ID: "cluster://9"}, message: "no longer authorized"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			placement := ChangeSetPlacement{CredentialReferences: map[string]map[string]capability.CredentialReference{
				test.agentID: {test.key: test.reference},
			}}
			if err := ValidateCredentialPlacement(candidate, required, placement, choices); err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("error = %v, want %q", err, test.message)
			}
		})
	}
}

func TestValidateCredentialPlacementRejectsUnprovenChoiceWithoutLeakingReference(t *testing.T) {
	placement := ChangeSetPlacement{CredentialReferences: map[string]map[string]capability.CredentialReference{
		"sre": {"kubernetes-cluster": {Kind: "kubernetes-cluster", ID: "cluster://secret-internal-id"}},
	}}
	err := ValidateCredentialPlacement(nil, nil, placement, nil)
	if err == nil || strings.Contains(err.Error(), "secret-internal-id") {
		t.Fatalf("secret-safe unavailable error = %v", err)
	}
}

func TestValidateCredentialPlacementAcceptsAuthorizedDeploymentBinding(t *testing.T) {
	candidate := &WorkforceCandidate{Agents: []*agent.AgentDefinition{{ID: "operator"}}}
	required := map[string][]string{"operator": {agent.ModelProviderCredentialBinding}}
	choices := []capability.CredentialBindingChoice{{
		Reference:   capability.CredentialReference{Kind: "host-vault", ID: "credential-29"},
		DisplayName: "Primary model",
		BindingKeys: []string{agent.ModelProviderCredentialBinding},
	}}
	placement := ChangeSetPlacement{CredentialReferences: map[string]map[string]capability.CredentialReference{
		"operator": {agent.ModelProviderCredentialBinding: {Kind: "host-vault", ID: "credential-29"}},
	}}
	if err := ValidateCredentialPlacement(candidate, required, placement, choices); err != nil {
		t.Fatal(err)
	}
	placement.CredentialReferences["operator"][agent.ModelProviderCredentialBinding] =
		capability.CredentialReference{Kind: "host-vault", ID: "credential-other"}
	if err := ValidateCredentialPlacement(candidate, required, placement, choices); err == nil ||
		!strings.Contains(err.Error(), "unavailable or no longer authorized") {
		t.Fatalf("unadvertised deployment binding error = %v", err)
	}
}

func TestValidateCredentialPlacementKeepsOptionalDeploymentBindingSeparateFromSkillCredential(t *testing.T) {
	candidate := &WorkforceCandidate{Agents: []*agent.AgentDefinition{{ID: "slack-agent"}}}
	required := map[string][]string{"slack-agent": {"slack_bot_token"}}
	choices := []capability.CredentialBindingChoice{
		{
			Reference:   capability.CredentialReference{Kind: "vault", ID: "36"},
			DisplayName: "Primary model",
			BindingKeys: []string{agent.ModelProviderCredentialBinding},
		},
		{
			Reference:   capability.CredentialReference{Kind: "slack_bot_token", ID: "vault://007.token"},
			DisplayName: "Slack workspace",
		},
	}
	placement := ChangeSetPlacement{CredentialReferences: map[string]map[string]capability.CredentialReference{
		"slack-agent": {
			agent.ModelProviderCredentialBinding: {Kind: "vault", ID: "36"},
			"slack_bot_token":                    {Kind: "slack_bot_token", ID: "vault://007.token"},
		},
	}}
	if err := ValidateCredentialPlacement(candidate, required, placement, choices); err != nil {
		t.Fatalf("separate deployment and Skill credentials = %v", err)
	}

	placement.CredentialReferences["slack-agent"]["unrequired_skill_token"] =
		capability.CredentialReference{Kind: "unrequired_skill_token", ID: "credential"}
	choices = append(choices, capability.CredentialBindingChoice{
		Reference: capability.CredentialReference{Kind: "unrequired_skill_token", ID: "credential"},
	})
	if err := ValidateCredentialPlacement(candidate, required, placement, choices); err == nil ||
		!strings.Contains(err.Error(), "not a required kind") {
		t.Fatalf("unrequired Skill credential error = %v", err)
	}
}

func TestRequiredCredentialsIncludesRuntimeBindingOnlyForActiveCandidates(t *testing.T) {
	catalog := CapabilityCatalog{AgentCredentialRequirements: []AgentCredentialRequirement{{
		BindingKey: agent.ModelProviderCredentialBinding, DisplayName: "Model provider",
		Prompt: "Choose the model provider this Agent should use.", RequiredForActivation: true,
	}}}
	active := WorkforceCandidate{
		Agents:     []*agent.AgentDefinition{{ID: "operator"}},
		Activation: WorkforceActivationActive,
	}
	if values := requiredCredentials(active, catalog)["operator"]; len(values) != 1 || values[0] != agent.ModelProviderCredentialBinding {
		t.Fatalf("active required credentials = %#v", values)
	}
	active.Activation = WorkforceActivationInactive
	if values := requiredCredentials(active, catalog)["operator"]; len(values) != 0 {
		t.Fatalf("inactive required credentials = %#v", values)
	}
}

func TestActiveChangeSetCannotApplyWithoutRuntimeCredentialPlacement(t *testing.T) {
	changeSet := &ChangeSet{
		Result: CompileResult{Valid: true, Candidate: WorkforceCandidate{
			Agents:     []*agent.AgentDefinition{{ID: "operator"}},
			Activation: WorkforceActivationActive,
		}},
		RequiredCredentials: map[string][]string{
			"operator": {agent.ModelProviderCredentialBinding},
		},
		Placement: ChangeSetPlacement{
			Environment:        "production",
			AgentDeploymentIDs: map[string]string{"operator": "operator-live"},
		},
	}
	if err := validateApplyPlacement(changeSet); err == nil ||
		!strings.Contains(err.Error(), "requires an opaque MODEL_PROVIDER credential reference") {
		t.Fatalf("missing active runtime credential error = %v", err)
	}
	changeSet.Placement.CredentialReferences = map[string]map[string]capability.CredentialReference{
		"operator": {
			agent.ModelProviderCredentialBinding: {Kind: "host-vault", ID: "credential-29"},
		},
	}
	if err := validateApplyPlacement(changeSet); err != nil {
		t.Fatalf("placed active runtime credential = %v", err)
	}
}
