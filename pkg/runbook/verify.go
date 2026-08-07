package runbook

import (
	"fmt"
	"sort"
	"strings"

	"github.com/axiom-studio/openseal/pkg/capability"
)

// DiagnosticSeverity classifies verifier findings without requiring callers to
// parse human-readable messages. Errors block activation; warnings are durable
// review information.
type DiagnosticSeverity string

const (
	DiagnosticError   DiagnosticSeverity = "error"
	DiagnosticWarning DiagnosticSeverity = "warning"
)

// VerificationReport is the deterministic, source-addressable proof produced
// before a Runbook can be activated in a resolved capability environment.
type VerificationReport struct {
	Valid       bool         `json:"valid"`
	Diagnostics []Diagnostic `json:"diagnostics,omitempty"`
}

// ResolvedAction is the credential-free projection of one exact Skill binding
// and action. Hosts build this from their catalog and opaque binding records;
// secret values never cross the verifier boundary.
type ResolvedAction struct {
	SkillID              string                                    `json:"skillId"`
	SkillVersion         string                                    `json:"skillVersion"`
	SourceIdentity       string                                    `json:"sourceIdentity,omitempty"`
	Action               string                                    `json:"action"`
	BindingID            string                                    `json:"bindingId"`
	BindingRevision      int64                                     `json:"bindingRevision"`
	Enabled              bool                                      `json:"enabled"`
	Allowed              bool                                      `json:"allowed"`
	MaximumRisk          capability.RiskLevel                      `json:"maximumRisk"`
	Risk                 capability.RiskLevel                      `json:"risk"`
	SideEffect           capability.SideEffect                     `json:"sideEffect"`
	Idempotency          capability.IdempotencyMode                `json:"idempotency"`
	RequiredCredentials  []capability.CredentialRequirement        `json:"requiredCredentials,omitempty"`
	BoundCredentials     map[string]capability.CredentialReference `json:"boundCredentials,omitempty"`
	CompensationAction   string                                    `json:"compensationAction,omitempty"`
	FinalizerAction      string                                    `json:"finalizerAction,omitempty"`
	RequiresApproval     bool                                      `json:"requiresApproval,omitempty"`
	ApprovalRoutePresent bool                                      `json:"approvalRoutePresent,omitempty"`
}

// VerificationEnvironment is a portable resolved snapshot. It contains only
// public policy metadata and opaque credential references, never credentials.
type VerificationEnvironment struct {
	Actions []ResolvedAction `json:"actions,omitempty"`
}

// Verify proves both the canonical Runbook structure and the facts that are
// knowable only after exact Skills, bindings, credentials, and authority have
// been resolved. The result ordering is stable across processes and replicas.
func Verify(definition *Definition, environment VerificationEnvironment) VerificationReport {
	diagnostics := append([]Diagnostic(nil), Validate(definition)...)
	if definition != nil {
		verifier := staticVerifier{definition: definition, environment: environment, diagnostics: diagnostics}
		verifier.verify()
		diagnostics = verifier.diagnostics
	}
	sort.SliceStable(diagnostics, func(i, j int) bool {
		if diagnostics[i].Path != diagnostics[j].Path {
			return diagnostics[i].Path < diagnostics[j].Path
		}
		if diagnostics[i].Code != diagnostics[j].Code {
			return diagnostics[i].Code < diagnostics[j].Code
		}
		return diagnostics[i].Message < diagnostics[j].Message
	})
	valid := true
	for i := range diagnostics {
		if diagnostics[i].Severity == "" {
			diagnostics[i].Severity = DiagnosticError
		}
		if diagnostics[i].Severity == DiagnosticError {
			valid = false
		}
	}
	return VerificationReport{Valid: valid, Diagnostics: diagnostics}
}

type staticVerifier struct {
	definition  *Definition
	environment VerificationEnvironment
	diagnostics []Diagnostic
}

func (v *staticVerifier) add(path, code, message string, related ...string) {
	v.diagnostics = append(v.diagnostics, Diagnostic{
		Path: path, Code: code, Message: message, Severity: DiagnosticError,
		RelatedPaths: append([]string(nil), related...),
	})
}

func (v *staticVerifier) warn(path, code, message string, related ...string) {
	v.diagnostics = append(v.diagnostics, Diagnostic{
		Path: path, Code: code, Message: message, Severity: DiagnosticWarning,
		RelatedPaths: append([]string(nil), related...),
	})
}

func (v *staticVerifier) verify() {
	v.verifyTerminalPaths()
	v.verifyActions()
	v.verifyBudgets()
}

func (v *staticVerifier) verifyTerminalPaths() {
	terminal := map[string]bool{}
	for id, step := range v.definition.Steps {
		if step.Kind == StepEnd && step.End != nil {
			terminal[id] = true
		}
	}
	changed := true
	for changed {
		changed = false
		for id := range v.definition.Steps {
			if terminal[id] {
				continue
			}
			step := v.definition.Steps[id]
			if step.Kind == StepLoopReturn && step.LoopReturn != nil {
				loop := v.definition.Steps[step.LoopReturn.ForEach]
				if loop.ForEach != nil && terminal[loop.ForEach.Next] {
					terminal[id], changed = true, true
				}
				continue
			}
			if step.Kind == StepDecision && step.Decision != nil && step.Decision.Default == "" {
				continue
			}
			successors := (&validator{definition: v.definition}).successors(id)
			if len(successors) == 0 {
				continue
			}
			allTerminate := true
			for _, next := range successors {
				if !terminal[next] {
					allTerminate = false
					break
				}
			}
			if allTerminate {
				terminal[id], changed = true, true
			}
		}
	}
	for name, start := range v.definition.Entrypoints {
		if !terminal[start] {
			v.add("entrypoints."+name, "graph.terminal_path_missing", "entrypoint does not have a complete path to an end step", "steps."+start)
		}
	}
}

func (v *staticVerifier) verifyActions() {
	for stepID, step := range v.definition.Steps {
		if step.Kind != StepAction || step.Action == nil {
			continue
		}
		path := "steps." + stepID + ".action"
		matches := make([]ResolvedAction, 0, 1)
		for _, candidate := range v.environment.Actions {
			if candidate.SkillID == step.Action.SkillID && candidate.SkillVersion == step.Action.SkillVersion && candidate.Action == step.Action.Action {
				matches = append(matches, candidate)
			}
		}
		sort.Slice(matches, func(i, j int) bool {
			left := matches[i].SourceIdentity + "\x00" + matches[i].BindingID
			right := matches[j].SourceIdentity + "\x00" + matches[j].BindingID
			return left < right
		})
		switch len(matches) {
		case 0:
			v.add(path, "action.binding_missing", fmt.Sprintf("exact Skill action %s@%s/%s has no resolved binding", step.Action.SkillID, step.Action.SkillVersion, step.Action.Action))
			continue
		case 1:
		default:
			related := make([]string, 0, len(matches))
			for _, match := range matches {
				related = append(related, "bindings."+match.BindingID)
			}
			v.add(path, "action.binding_ambiguous", fmt.Sprintf("exact Skill action resolves to %d bindings; Runbook activation requires one binding", len(matches)), related...)
			continue
		}
		resolved := matches[0]
		bindingPath := "bindings." + resolved.BindingID
		if !resolved.Enabled {
			v.add(path, "action.binding_disabled", "exact Skill binding is disabled", bindingPath)
		}
		if !resolved.Allowed {
			v.add(path, "action.not_authorized", "exact Skill action is not allowed by its binding", bindingPath)
		}
		if riskRank(resolved.Risk) > riskRank(resolved.MaximumRisk) {
			v.add(path, "action.risk_exceeds_binding", fmt.Sprintf("action risk %s exceeds binding maximum %s", resolved.Risk, resolved.MaximumRisk), bindingPath)
		}
		for _, requirement := range resolved.RequiredCredentials {
			reference, present := resolved.BoundCredentials[requirement.Name]
			if requirement.Optional && !present {
				continue
			}
			if !present || strings.TrimSpace(reference.ID) == "" || reference.Kind != requirement.Kind {
				v.add(path, "action.credential_unsatisfied", fmt.Sprintf("credential %s requires one opaque %s binding", requirement.Name, requirement.Kind), bindingPath+".credentials."+requirement.Name)
			}
		}
		if isMutatingSideEffect(resolved.SideEffect) && resolved.Idempotency == capability.IdempotencyNone {
			v.add(path, "action.idempotency_required", "action with durable side effects must support idempotency", bindingPath)
		}
		if resolved.RequiresApproval && !resolved.ApprovalRoutePresent {
			v.add(path, "action.approval_unreachable", "action requires approval but no reviewed approval route is reachable", bindingPath)
		}
		if resolved.SideEffect == capability.SideEffectDestructive && resolved.CompensationAction == "" {
			v.warn(path, "action.compensation_absent", "destructive action has no declared compensation action", bindingPath)
		}
	}
}

func (v *staticVerifier) verifyBudgets() {
	for triggerID, trigger := range v.definition.Triggers {
		if trigger.Budget == nil {
			continue
		}
		path := "triggers." + triggerID + ".budget"
		minimumActions := v.minimumActions(v.definition.Entrypoints[trigger.Entrypoint], map[string]bool{})
		if trigger.Budget.MaxActions > 0 && minimumActions > trigger.Budget.MaxActions {
			v.add(path+".maxActions", "budget.actions_impossible", fmt.Sprintf("budget allows %d actions but every complete path requires at least %d", trigger.Budget.MaxActions, minimumActions))
		}
		for stepID, step := range v.definition.Steps {
			if step.Delegate != nil && step.Delegate.Budget != nil {
				v.compareChildBudget(path, "steps."+stepID+".delegate.budget", *trigger.Budget, *step.Delegate.Budget)
			}
			if step.Fork != nil {
				for branch, budget := range step.Fork.BranchBudgets {
					v.compareChildBudget(path, "steps."+stepID+".fork.branchBudgets."+branch, *trigger.Budget, budget)
				}
				if join := v.definition.Steps[step.Fork.Join].Join; join != nil && join.Mode == JoinAll {
					v.compareAggregateBranchBudget(path, "steps."+stepID+".fork.branchBudgets", *trigger.Budget, step.Fork.BranchBudgets)
				}
			}
		}
	}
}

func (v *staticVerifier) compareAggregateBranchBudget(parentPath, childPath string, parent BudgetAllocation, branches map[string]BudgetAllocation) {
	checks := []struct {
		name   string
		parent int64
		value  func(BudgetAllocation) int64
	}{
		{"maxAttempts", parent.MaxAttempts, func(value BudgetAllocation) int64 { return value.MaxAttempts }},
		{"maxTurns", parent.MaxTurns, func(value BudgetAllocation) int64 { return value.MaxTurns }},
		{"maxInputTokens", parent.MaxInputTokens, func(value BudgetAllocation) int64 { return value.MaxInputTokens }},
		{"maxOutputTokens", parent.MaxOutputTokens, func(value BudgetAllocation) int64 { return value.MaxOutputTokens }},
		{"maxTotalTokens", parent.MaxTotalTokens, func(value BudgetAllocation) int64 { return value.MaxTotalTokens }},
		{"maxCostMicros", parent.MaxCostMicros, func(value BudgetAllocation) int64 { return value.MaxCostMicros }},
		{"maxDurationMs", parent.MaxDurationMS, func(value BudgetAllocation) int64 { return value.MaxDurationMS }},
		{"maxActions", parent.MaxActions, func(value BudgetAllocation) int64 { return value.MaxActions }},
	}
	for _, check := range checks {
		if check.parent == 0 || len(branches) == 0 {
			continue
		}
		var total int64
		complete := true
		for _, budget := range branches {
			value := check.value(budget)
			if value == 0 {
				complete = false
				break
			}
			total += value
		}
		if complete && total > check.parent {
			v.add(childPath+"."+check.name, "budget.parallel_exceeds_parent", fmt.Sprintf("parallel branches allocate %d but trigger ceiling is %d", total, check.parent), parentPath+"."+check.name)
		}
	}
}

func (v *staticVerifier) compareChildBudget(parentPath, childPath string, parent, child BudgetAllocation) {
	checks := []struct {
		name          string
		parent, child int64
	}{
		{"maxAttempts", parent.MaxAttempts, child.MaxAttempts}, {"maxTurns", parent.MaxTurns, child.MaxTurns},
		{"maxInputTokens", parent.MaxInputTokens, child.MaxInputTokens}, {"maxOutputTokens", parent.MaxOutputTokens, child.MaxOutputTokens},
		{"maxTotalTokens", parent.MaxTotalTokens, child.MaxTotalTokens}, {"maxCostMicros", parent.MaxCostMicros, child.MaxCostMicros},
		{"maxDurationMs", parent.MaxDurationMS, child.MaxDurationMS}, {"maxActions", parent.MaxActions, child.MaxActions},
	}
	for _, check := range checks {
		if check.parent > 0 && (check.child == 0 || check.child > check.parent) {
			v.add(childPath+"."+check.name, "budget.child_exceeds_parent", "child allocation is unbounded or exceeds its trigger ceiling", parentPath+"."+check.name)
		}
	}
}

func (v *staticVerifier) minimumActions(id string, visiting map[string]bool) int64 {
	if visiting[id] {
		return 0
	}
	step, ok := v.definition.Steps[id]
	if !ok || step.Kind == StepEnd {
		return 0
	}
	nextVisiting := make(map[string]bool, len(visiting)+1)
	for key, value := range visiting {
		nextVisiting[key] = value
	}
	nextVisiting[id] = true
	successors := (&validator{definition: v.definition}).successors(id)
	if len(successors) == 0 {
		return 0
	}
	minimum := int64(-1)
	for _, next := range successors {
		value := v.minimumActions(next, nextVisiting)
		if minimum < 0 || value < minimum {
			minimum = value
		}
	}
	if step.Kind == StepAction {
		minimum++
	}
	return minimum
}

func riskRank(value capability.RiskLevel) int {
	switch value {
	case capability.RiskLevelRead:
		return 1
	case capability.RiskLevelWrite:
		return 2
	case capability.RiskLevelExternal:
		return 3
	case capability.RiskLevelProduction:
		return 4
	case capability.RiskLevelDestructive:
		return 5
	default:
		return 99
	}
}

func isMutatingSideEffect(value capability.SideEffect) bool {
	return value == capability.SideEffectWrite || value == capability.SideEffectExternal || value == capability.SideEffectDestructive
}
