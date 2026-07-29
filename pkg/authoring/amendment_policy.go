package authoring

// normalizeUnboundAmendmentPolicies removes unusable self-amendment authority
// invented by a provider. Approval without an eligible principal can never be
// exercised; disabling proposal authority is the conservative deterministic
// result. A host may add a fully bound policy through a later reviewed change.
func normalizeUnboundAmendmentPolicies(candidate *WorkforceCandidate) {
	if candidate == nil {
		return
	}
	for _, definition := range candidate.Agents {
		if definition == nil || !definition.Amendments.RequiresApproval || len(definition.Amendments.ApproverPrincipals) > 0 {
			continue
		}
		definition.Amendments.AgentMayPropose = false
		definition.Amendments.AllowedFields = nil
		definition.Amendments.RequiresApproval = false
	}
	if candidate.Team != nil && candidate.Team.Amendments.RequiresApproval && len(candidate.Team.Amendments.ApproverPrincipals) == 0 {
		candidate.Team.Amendments.AgentMayPropose = false
		candidate.Team.Amendments.AllowedFields = nil
		candidate.Team.Amendments.RequiresApproval = false
	}
}
