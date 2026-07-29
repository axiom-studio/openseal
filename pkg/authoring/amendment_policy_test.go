package authoring

import (
	"testing"

	"github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/team"
	"github.com/axiom-studio/openseal/pkg/workforce"
)

func TestNormalizeUnboundAmendmentPoliciesFailsClosed(t *testing.T) {
	candidate := WorkforceCandidate{
		Agents: []*agent.AgentDefinition{{Amendments: workforce.AmendmentPolicy{
			AgentMayPropose: true, AllowedFields: []string{"systemPrompt"}, RequiresApproval: true,
		}}},
		Team: &team.Definition{Amendments: workforce.AmendmentPolicy{
			AgentMayPropose: true, AllowedFields: []string{"roles"}, RequiresApproval: true,
		}},
	}
	normalizeUnboundAmendmentPolicies(&candidate)
	if candidate.Agents[0].Amendments.AgentMayPropose || candidate.Agents[0].Amendments.RequiresApproval || len(candidate.Agents[0].Amendments.AllowedFields) != 0 {
		t.Fatalf("unbound Agent amendment authority survived: %#v", candidate.Agents[0].Amendments)
	}
	if candidate.Team.Amendments.AgentMayPropose || candidate.Team.Amendments.RequiresApproval || len(candidate.Team.Amendments.AllowedFields) != 0 {
		t.Fatalf("unbound Team amendment authority survived: %#v", candidate.Team.Amendments)
	}
}

func TestNormalizeUnboundAmendmentPoliciesPreservesBoundApproval(t *testing.T) {
	policy := workforce.AmendmentPolicy{
		AgentMayPropose: true, AllowedFields: []string{"systemPrompt"}, RequiresApproval: true,
		ApproverPrincipals: []string{"tenant-admin"},
	}
	candidate := WorkforceCandidate{Agents: []*agent.AgentDefinition{{Amendments: policy}}}
	normalizeUnboundAmendmentPolicies(&candidate)
	if !candidate.Agents[0].Amendments.AgentMayPropose || !candidate.Agents[0].Amendments.RequiresApproval || len(candidate.Agents[0].Amendments.ApproverPrincipals) != 1 {
		t.Fatalf("bound amendment policy changed: %#v", candidate.Agents[0].Amendments)
	}
}
