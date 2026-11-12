package runtime

import (
	"github.com/axiom-studio/openseal/pkg/capability"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
	"github.com/axiom-studio/openseal/pkg/workforce"
)

func sqliteTeamDefinition(version string) *kernelteam.Definition {
	return &kernelteam.Definition{
		ID: "research-team", Version: version, DisplayName: "Research Team", Purpose: "Produce evidence-backed findings",
		Delegation: kernelteam.DelegationPolicy{MaximumDepth: 2, MaximumConcurrent: 4, RequireAcceptance: true},
		Approvals:  kernelteam.ApprovalPolicy{MaximumRisk: capability.RiskLevelExternal},
		Amendments: workforce.AmendmentPolicy{
			AllowedFields: []string{"purpose"}, RequiresApproval: true, ApproverPrincipals: []string{"user:operator"},
		},
	}
}
