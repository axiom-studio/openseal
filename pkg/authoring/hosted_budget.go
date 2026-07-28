package authoring

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runbook"
)

// HostedSkillModelInputTokenCeiling returns a credential-free conservative
// ceiling for one activated binding of definition. Hosts publish only this
// scalar in authoring catalogs; private prompt instructions remain outside the
// candidate and provider-facing catalog.
func HostedSkillModelInputTokenCeiling(definition capability.Definition) (int64, error) {
	return capability.HostedSkillModelInputTokenCeiling(definition)
}

// validateHostedRunbookBudgets proves that every statically targeted cognitive
// delegate can carry the exact reviewed Skill catalog for the complete bounded
// workflow. A hosted Turn proposes at most one action, so N selected actions
// require at least N action Turns and one terminal Turn.
func validateHostedRunbookBudgets(candidate *WorkforceCandidate, catalog CapabilityCatalog) []ValidationIssue {
	if candidate == nil || catalog.HostedExecution == nil {
		return nil
	}
	agents := make(map[string]*agent.AgentDefinition, len(candidate.Agents))
	for _, definition := range candidate.Agents {
		if definition != nil {
			agents[strings.TrimSpace(definition.ID)] = definition
		}
	}
	issues := make([]ValidationIssue, 0)
	for ownerIndex, owner := range candidate.Agents {
		if owner == nil || owner.Runbook == nil {
			continue
		}
		stepIDs := make([]string, 0, len(owner.Runbook.Steps))
		for stepID := range owner.Runbook.Steps {
			stepIDs = append(stepIDs, stepID)
		}
		sort.Strings(stepIDs)
		for _, stepID := range stepIDs {
			step := owner.Runbook.Steps[stepID]
			if step.Kind != runbook.StepDelegate || step.Delegate == nil || step.Delegate.Mode != runbook.DelegateReason || step.Delegate.Budget == nil {
				continue
			}
			var targetID string
			if len(step.Delegate.AgentID.Literal) == 0 || json.Unmarshal(step.Delegate.AgentID.Literal, &targetID) != nil || strings.TrimSpace(targetID) == "" {
				continue
			}
			target := agents[strings.TrimSpace(targetID)]
			if target == nil {
				continue
			}
			path := fmt.Sprintf("agents[%d].runbook.steps.%s.delegate.budget", ownerIndex, stepID)
			issues = append(issues, validateHostedBudget(path, target, step.Delegate, catalog)...)
		}
	}
	return issues
}

func validateHostedBudget(path string, target *agent.AgentDefinition, delegate *runbook.DelegateStep, catalog CapabilityCatalog) []ValidationIssue {
	perTurn := catalog.HostedExecution.BaseInputTokens + hostedAgentDefinitionTokens(target, delegate)
	actions := int64(0)
	issues := make([]ValidationIssue, 0)
	for _, requirement := range target.SkillRequirements {
		skill, ok := catalog.Skills[strings.TrimSpace(requirement.SkillID)]
		if !ok || requirement.Optional {
			continue
		}
		if skill.HostedModelInputTokens < 1 && (len(requirement.RequiredActions) > 0 || requirement.PromptRequired) {
			issues = append(issues, issue(path, "hosted_budget_skill_envelope_unavailable",
				fmt.Sprintf("Skill %s has no host-attested model input ceiling; refresh the capability catalog before activation", skill.ID)))
			continue
		}
		perTurn += skill.HostedModelInputTokens
		actions += int64(len(requirement.RequiredActions))
	}
	turns := actions + 1
	if turns < 1 {
		turns = 1
	}
	requiredInput := saturatingMultiply(perTurn, turns)
	requiredOutput := saturatingMultiply(catalog.HostedExecution.MinimumOutputTokens, turns)
	requiredTotal := saturatingAdd(requiredInput, requiredOutput)
	budget := delegate.Budget
	checks := []struct {
		field, code, message string
		actual, required     int64
	}{
		{"maxAttempts", "hosted_budget_attempts_insufficient", "hosted workflow attempts", budget.MaxAttempts, turns},
		{"maxTurns", "hosted_budget_turns_insufficient", "hosted workflow turns", budget.MaxTurns, turns},
		{"maxActions", "hosted_budget_actions_insufficient", "hosted workflow actions", budget.MaxActions, actions},
		{"maxInputTokens", "hosted_budget_input_insufficient", "hosted model input tokens", budget.MaxInputTokens, requiredInput},
		{"maxOutputTokens", "hosted_budget_output_insufficient", "hosted model output tokens", budget.MaxOutputTokens, requiredOutput},
		{"maxTotalTokens", "hosted_budget_total_insufficient", "hosted total tokens", budget.MaxTotalTokens, requiredTotal},
	}
	for _, check := range checks {
		if check.required > 0 && check.actual > 0 && check.actual < check.required {
			issues = append(issues, issue(path+"."+check.field, check.code,
				fmt.Sprintf("%s requires at least %d for the reviewed hosted capability envelope; increase %s or remove unnecessary actions", check.message, check.required, check.field)))
		}
	}
	return issues
}

func hostedAgentDefinitionTokens(target *agent.AgentDefinition, delegate *runbook.DelegateStep) int64 {
	value := struct {
		Goal                runbook.Value            `json:"goal"`
		Context             map[string]runbook.Value `json:"context,omitempty"`
		SystemPrompt        string                   `json:"systemPrompt"`
		Personality         string                   `json:"personality,omitempty"`
		OperatingPrinciples []string                 `json:"operatingPrinciples,omitempty"`
	}{Goal: delegate.Goal, Context: delegate.Context, SystemPrompt: target.SystemPrompt, Personality: target.Personality, OperatingPrinciples: target.OperatingPrinciples}
	payload, _ := json.Marshal(value)
	return (int64(len(payload)) + 1) / 2
}

func saturatingMultiply(left, right int64) int64 {
	if left <= 0 || right <= 0 {
		return 0
	}
	const maximum = int64(^uint64(0) >> 1)
	if left > maximum/right {
		return maximum
	}
	return left * right
}

func saturatingAdd(left, right int64) int64 {
	const maximum = int64(^uint64(0) >> 1)
	if right > maximum-left {
		return maximum
	}
	return left + right
}
