package authoring

import (
	"sort"
	"strings"

	"github.com/axiom-studio/openseal/pkg/source"
)

// sourcePolicyProposals projects only exact catalog-owned drafts that match an
// unresolved source-policy reference. Provider output can request a reference,
// but cannot author or widen this policy object.
func sourcePolicyProposals(candidate *WorkforceCandidate, missing []MissingRequirement, catalog CapabilityCatalog) []SourcePolicyProposal {
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
		if len(consumers) == 0 || seen[reference] || !candidateUsesProposedSourcePolicy(candidate, reference, draft.SkillIDs) {
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

func candidateUsesProposedSourcePolicy(candidate *WorkforceCandidate, reference string, skillIDs []string) bool {
	if candidate == nil || candidate.Initiative == nil {
		return false
	}
	allowed := stringSet(skillIDs)
	for _, monitor := range candidate.Initiative.SourceMonitors {
		if monitor.SourcePolicyRef == reference && allowed[monitor.SkillID] {
			return true
		}
	}
	return false
}

// validateSourceActionProjection prevents an authoring candidate from placing
// the built-in governed source action outside the Initiative monitor envelope
// required by runtime policy authorization and evidence checkpoints.
func validateSourceActionProjection(candidate *WorkforceCandidate) []ValidationIssue {
	issues := make([]ValidationIssue, 0)
	for _, invocation := range candidateObjectiveCapabilityInvocations(candidate) {
		if invocation.action == nil {
			continue
		}
		skillID, action := invocation.action.SkillID, invocation.action.Action
		if strings.TrimSpace(skillID) != source.SkillID || strings.TrimSpace(action) != source.ObserveFeed {
			continue
		}
		objectiveRef := invocation.objectiveRef
		projected := false
		if candidate != nil && candidate.Initiative != nil && objectiveRef != "" {
			for _, monitor := range candidate.Initiative.SourceMonitors {
				if monitor.ObjectiveRef == objectiveRef && monitor.SkillID == source.SkillID && monitor.Action == source.ObserveFeed {
					projected = true
					break
				}
			}
		}
		if !projected {
			issues = append(issues, issue(
				invocation.path,
				"source_action_requires_monitor",
				"The governed source observer must be projected by an exact Initiative source monitor so policy decisions, checkpoints, and evidence remain enforceable",
			))
		}
	}
	return issues
}

// providerRepairableMissingRequirements excludes host-owned readiness work and
// exact operator choices from probabilistic repair. Asking the provider to
// "fix" one of these gaps can make it silently discard an approved policy or
// substitute an installed Skill for the exact Skill the operator selected.
func providerRepairableMissingRequirements(candidate *WorkforceCandidate, missing []MissingRequirement, request GenerateRequest) []MissingRequirement {
	proposed := make(map[string]bool)
	for _, need := range request.Catalog.CapabilityNeeds {
		if draft := need.SourcePolicyProposal; draft != nil {
			reference := draft.Policy.ID + "@" + draft.Policy.Version
			proposed[reference] = candidateUsesProposedSourcePolicy(candidate, reference, draft.SkillIDs)
		}
	}
	selectedSkills := answeredCapabilityNeedSkills(request)
	result := make([]MissingRequirement, 0, len(missing))
	for _, requirement := range missing {
		if requirement.Kind == "source_policy" && proposed[strings.TrimSpace(requirement.ID)] {
			continue
		}
		if selectedCapabilityReadinessRequirement(requirement, selectedSkills) {
			continue
		}
		result = append(result, requirement)
	}
	return result
}

func answeredCapabilityNeedSkills(request GenerateRequest) map[string]bool {
	selected := make(map[string]bool)
	if request.Refinement == nil {
		return selected
	}
	needs := make(map[string]bool, len(request.Catalog.CapabilityNeeds))
	for _, need := range request.Catalog.CapabilityNeeds {
		needs[CapabilityNeedQuestionID(need.ID)] = true
	}
	for _, answer := range request.Refinement.Answers {
		if !needs[strings.TrimSpace(answer.QuestionID)] {
			continue
		}
		for _, skillID := range answer.Value.SkillIDs {
			if skillID = strings.TrimSpace(skillID); skillID != "" {
				selected[skillID] = true
			}
		}
	}
	return selected
}

func selectedCapabilityReadinessRequirement(requirement MissingRequirement, selected map[string]bool) bool {
	switch requirement.Kind {
	case "skill_installation", "skill_binding":
		return selected[strings.TrimSpace(requirement.ID)]
	case "credential":
		for skillID := range selected {
			if strings.HasSuffix(strings.TrimSpace(requirement.RequiredBy), "/skill:"+skillID) {
				return true
			}
		}
	}
	return false
}
