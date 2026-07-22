package authoring

import (
	"sort"
	"strings"

	"github.com/axiom-studio/openseal/pkg/source"
)

// sourcePolicyProposals projects only exact catalog-owned drafts that match an
// unresolved source-policy reference. Provider output can request a reference,
// but cannot author or widen this policy object.
func sourcePolicyProposals(missing []MissingRequirement, catalog CapabilityCatalog) []SourcePolicyProposal {
	requiredBy := make(map[string][]string)
	for _, requirement := range missing {
		if requirement.Kind != "source_policy" {
			continue
		}
		reference := strings.TrimSpace(requirement.ID)
		requiredBy[reference] = append(requiredBy[reference], strings.TrimSpace(requirement.RequiredBy))
	}
	proposals := make([]SourcePolicyProposal, 0)
	seen := make(map[string]bool)
	for _, need := range catalog.CapabilityNeeds {
		draft := need.SourcePolicyProposal
		if draft == nil {
			continue
		}
		reference := draft.Policy.ID + "@" + draft.Policy.Version
		consumers := requiredBy[reference]
		if len(consumers) == 0 || seen[reference] {
			continue
		}
		seen[reference] = true
		consumers = nonEmptyUnique(consumers)
		sort.Strings(consumers)
		proposals = append(proposals, SourcePolicyProposal{
			APIVersion: source.LifecycleAPIVersion, CapabilityNeedID: need.ID,
			Reference: reference, Policy: draft.Policy, Reason: draft.Reason,
			RequiredBy: consumers, RequiresApproval: true,
		})
	}
	sort.Slice(proposals, func(i, j int) bool { return proposals[i].Reference < proposals[j].Reference })
	return proposals
}

// sourcePolicyProposalRepairableMissing excludes only source-policy gaps for
// which the trusted catalog supplies the exact requested draft. Asking the
// provider to "repair" such a gap could make it silently remove the requested
// monitor; the correct next step is governed human review and activation.
func sourcePolicyProposalRepairableMissing(missing []MissingRequirement, catalog CapabilityCatalog) []MissingRequirement {
	proposed := make(map[string]bool)
	for _, need := range catalog.CapabilityNeeds {
		if draft := need.SourcePolicyProposal; draft != nil {
			proposed[draft.Policy.ID+"@"+draft.Policy.Version] = true
		}
	}
	result := make([]MissingRequirement, 0, len(missing))
	for _, requirement := range missing {
		if requirement.Kind == "source_policy" && proposed[strings.TrimSpace(requirement.ID)] {
			continue
		}
		result = append(result, requirement)
	}
	return result
}
