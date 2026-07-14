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
