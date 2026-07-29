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
	// Provider-authored budgets are planning hints, not trusted execution
	// limits. Raise them deterministically to the smallest envelope that can
	// execute the exact reviewed catalog; the resulting values remain visible
	// in the proposal before activation.
	normalizeHostedRunbookBudgets(candidate, catalog)
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
			required, budgetIssues := validateHostedBudget(path, target, step.Delegate, catalog)
			issues = append(issues, budgetIssues...)
			issues = append(issues, validateHostedTriggerCapacity(ownerIndex, owner.Runbook, stepID, step.Delegate.Budget, required)...)
		}
	}
	return issues
}

func normalizeHostedRunbookBudgets(candidate *WorkforceCandidate, catalog CapabilityCatalog) {
	agents := make(map[string]*agent.AgentDefinition, len(candidate.Agents))
	for _, definition := range candidate.Agents {
		if definition != nil {
			agents[strings.TrimSpace(definition.ID)] = definition
		}
	}
	for _, owner := range candidate.Agents {
		if owner == nil || owner.Runbook == nil {
			continue
		}
		for stepID, step := range owner.Runbook.Steps {
			if step.Kind != runbook.StepDelegate || step.Delegate == nil || step.Delegate.Mode != runbook.DelegateReason {
				continue
			}
			var targetID string
			if len(step.Delegate.AgentID.Literal) == 0 || json.Unmarshal(step.Delegate.AgentID.Literal, &targetID) != nil {
				continue
			}
			target := agents[strings.TrimSpace(targetID)]
			if target == nil {
				continue
			}
			if step.Delegate.Budget == nil {
				step.Delegate.Budget = &runbook.BudgetAllocation{}
			}
			required, _ := validateHostedBudget("", target, step.Delegate, catalog)
			raiseHostedBudget(step.Delegate.Budget, required, 0)
			owner.Runbook.Steps[stepID] = step
			for triggerID, trigger := range owner.Runbook.Triggers {
				if !runbookStepReachable(owner.Runbook, trigger.Entrypoint, stepID) {
					continue
				}
				if trigger.Budget == nil {
					trigger.Budget = &runbook.BudgetAllocation{}
				}
				raiseHostedBudget(trigger.Budget, *step.Delegate.Budget, 2)
				owner.Runbook.Triggers[triggerID] = trigger
			}
		}
	}
}

func raiseHostedBudget(current *runbook.BudgetAllocation, required runbook.BudgetAllocation, orchestrationOverhead int64) {
	if current == nil {
		return
	}
	current.MaxAttempts = maximumInt64(current.MaxAttempts, saturatingAdd(required.MaxAttempts, orchestrationOverhead))
	current.MaxTurns = maximumInt64(current.MaxTurns, saturatingAdd(required.MaxTurns, orchestrationOverhead))
	current.MaxInputTokens = maximumInt64(current.MaxInputTokens, required.MaxInputTokens)
	current.MaxOutputTokens = maximumInt64(current.MaxOutputTokens, required.MaxOutputTokens)
	current.MaxTotalTokens = maximumInt64(current.MaxTotalTokens, required.MaxTotalTokens)
	current.MaxTotalTokens = maximumInt64(current.MaxTotalTokens, saturatingAdd(current.MaxInputTokens, current.MaxOutputTokens))
	current.MaxActions = maximumInt64(current.MaxActions, required.MaxActions)
}

func maximumInt64(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
}

func validateHostedBudget(path string, target *agent.AgentDefinition, delegate *runbook.DelegateStep, catalog CapabilityCatalog) (runbook.BudgetAllocation, []ValidationIssue) {
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
	if requiredOutput < capability.HostedMinimumChildOutputTokens {
		requiredOutput = capability.HostedMinimumChildOutputTokens
	}
	requiredTotal := saturatingAdd(requiredInput, requiredOutput)
	required := runbook.BudgetAllocation{
		MaxAttempts: turns, MaxTurns: turns, MaxActions: actions,
		MaxInputTokens: requiredInput, MaxOutputTokens: requiredOutput, MaxTotalTokens: requiredTotal,
	}
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
	return required, issues
}

func validateHostedTriggerCapacity(ownerIndex int, definition *runbook.Definition, delegateStepID string, allocation *runbook.BudgetAllocation, required runbook.BudgetAllocation) []ValidationIssue {
	if definition == nil {
		return nil
	}
	triggerIDs := make([]string, 0, len(definition.Triggers))
	for triggerID := range definition.Triggers {
		triggerIDs = append(triggerIDs, triggerID)
	}
	sort.Strings(triggerIDs)
	issues := make([]ValidationIssue, 0)
	for _, triggerID := range triggerIDs {
		trigger := definition.Triggers[triggerID]
		if trigger.Budget == nil || !runbookStepReachable(definition, trigger.Entrypoint, delegateStepID) {
			continue
		}
		child := runbook.BudgetAllocation{}
		if allocation != nil {
			child = *allocation
		}
		for _, dimension := range []struct {
			field, code, message   string
			parent, child, minimum int64
			overhead               int64
		}{
			{"maxAttempts", "hosted_parent_attempts_insufficient", "hosted workflow attempts", trigger.Budget.MaxAttempts, child.MaxAttempts, required.MaxAttempts, 2},
			{"maxTurns", "hosted_parent_turns_insufficient", "hosted workflow turns", trigger.Budget.MaxTurns, child.MaxTurns, required.MaxTurns, 2},
			{"maxInputTokens", "hosted_parent_input_insufficient", "hosted model input tokens", trigger.Budget.MaxInputTokens, child.MaxInputTokens, required.MaxInputTokens, 0},
			{"maxOutputTokens", "hosted_parent_output_insufficient", "hosted model output tokens", trigger.Budget.MaxOutputTokens, child.MaxOutputTokens, required.MaxOutputTokens, 0},
			{"maxTotalTokens", "hosted_parent_total_insufficient", "hosted total tokens", trigger.Budget.MaxTotalTokens, child.MaxTotalTokens, required.MaxTotalTokens, 0},
			{"maxActions", "hosted_parent_actions_insufficient", "hosted actions", trigger.Budget.MaxActions, child.MaxActions, required.MaxActions, 0},
		} {
			if dimension.parent == 0 {
				continue
			}
			childRequired := dimension.minimum
			if dimension.child > childRequired {
				childRequired = dimension.child
			}
			totalRequired := saturatingAdd(childRequired, dimension.overhead)
			if dimension.parent < totalRequired {
				path := fmt.Sprintf("agents[%d].runbook.triggers.%s.budget.%s", ownerIndex, triggerID, dimension.field)
				issues = append(issues, issue(path, dimension.code,
					fmt.Sprintf("%s requires at least %d to fund delegated step %s and deterministic orchestration", dimension.message, totalRequired, delegateStepID)))
			}
		}
	}
	return issues
}

func runbookStepReachable(definition *runbook.Definition, entrypoint, target string) bool {
	if definition == nil || strings.TrimSpace(entrypoint) == "" || strings.TrimSpace(target) == "" {
		return false
	}
	first := entrypoint
	if stepID, ok := definition.Entrypoints[entrypoint]; ok {
		first = stepID
	}
	queue := []string{first}
	seen := make(map[string]bool, len(definition.Steps))
	for len(queue) > 0 {
		stepID := queue[0]
		queue = queue[1:]
		if stepID == target {
			return true
		}
		if seen[stepID] {
			continue
		}
		seen[stepID] = true
		step, ok := definition.Steps[stepID]
		if !ok {
			continue
		}
		switch step.Kind {
		case runbook.StepAction:
			if step.Action != nil {
				queue = append(queue, step.Action.Next)
			}
		case runbook.StepDelegate:
			if step.Delegate != nil {
				queue = append(queue, step.Delegate.Next)
			}
		case runbook.StepDecision:
			if step.Decision != nil {
				queue = append(queue, step.Decision.Default)
				for _, decisionCase := range step.Decision.Cases {
					queue = append(queue, decisionCase.Next)
				}
			}
		case runbook.StepTransform:
			if step.Transform != nil {
				queue = append(queue, step.Transform.Next)
			}
		case runbook.StepWait:
			if step.Wait != nil {
				queue = append(queue, step.Wait.Next)
			}
		case runbook.StepFork:
			if step.Fork != nil {
				queue = append(queue, step.Fork.Join)
				for _, branch := range step.Fork.Branches {
					queue = append(queue, branch)
				}
			}
		case runbook.StepJoin:
			if step.Join != nil {
				queue = append(queue, step.Join.Next)
			}
		case runbook.StepForEach:
			if step.ForEach != nil {
				queue = append(queue, step.ForEach.Body, step.ForEach.Next)
			}
		}
	}
	return false
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
