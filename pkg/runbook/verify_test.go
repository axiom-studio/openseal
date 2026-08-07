package runbook

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
)

func TestVerifyResolvedCapabilityEnvironment(t *testing.T) {
	definition := verifierRunbook()
	environment := VerificationEnvironment{Actions: []ResolvedAction{
		{
			SkillID: "source", SkillVersion: "1.0.0", SourceIdentity: "registry.example/source", Action: "read", BindingID: "source-binding", BindingRevision: 2,
			Enabled: true, Allowed: true, MaximumRisk: capability.RiskLevelRead, Risk: capability.RiskLevelRead,
			SideEffect: capability.SideEffectRead, Idempotency: capability.IdempotencySupported,
		},
		{
			SkillID: "delivery", SkillVersion: "2.0.0", SourceIdentity: "registry.example/delivery", Action: "publish", BindingID: "delivery-binding", BindingRevision: 4,
			Enabled: true, Allowed: true, MaximumRisk: capability.RiskLevelExternal, Risk: capability.RiskLevelExternal,
			SideEffect: capability.SideEffectExternal, Idempotency: capability.IdempotencyRequired,
			RequiredCredentials: []capability.CredentialRequirement{{Name: "connection", Kind: "oauth2"}},
			BoundCredentials:    map[string]capability.CredentialReference{"connection": {Kind: "oauth2", ID: "credential://opaque"}},
			RequiresApproval:    true, ApprovalRoutePresent: true,
		},
	}}
	report := Verify(definition, environment)
	if !report.Valid || len(report.Diagnostics) != 0 {
		t.Fatalf("report = %#v", report)
	}
}

func TestVerifyRejectsUnresolvedUnsafeAndImpossibleActivation(t *testing.T) {
	definition := verifierRunbook()
	definition.Triggers["hourly"] = Trigger{
		Kind: TriggerSchedule, Schedule: &Schedule{Cron: "0 0 * * * *", Timezone: "UTC"}, Entrypoint: "start", ObjectiveID: "research",
		Budget: &BudgetAllocation{MaxActions: 1, MaxTurns: 4},
	}
	environment := VerificationEnvironment{Actions: []ResolvedAction{
		{
			SkillID: "source", SkillVersion: "1.0.0", Action: "read", BindingID: "source-a", Enabled: true, Allowed: true,
			MaximumRisk: capability.RiskLevelRead, Risk: capability.RiskLevelRead, SideEffect: capability.SideEffectRead, Idempotency: capability.IdempotencySupported,
		},
		{
			SkillID: "source", SkillVersion: "1.0.0", Action: "read", BindingID: "source-b", Enabled: true, Allowed: true,
			MaximumRisk: capability.RiskLevelRead, Risk: capability.RiskLevelRead, SideEffect: capability.SideEffectRead, Idempotency: capability.IdempotencySupported,
		},
		{
			SkillID: "delivery", SkillVersion: "2.0.0", Action: "publish", BindingID: "delivery-binding", Enabled: true, Allowed: false,
			MaximumRisk: capability.RiskLevelWrite, Risk: capability.RiskLevelExternal, SideEffect: capability.SideEffectExternal, Idempotency: capability.IdempotencyNone,
			RequiredCredentials: []capability.CredentialRequirement{{Name: "connection", Kind: "oauth2"}},
			RequiresApproval:    true,
		},
	}}
	report := Verify(definition, environment)
	if report.Valid {
		t.Fatalf("report unexpectedly valid: %#v", report)
	}
	codes := diagnosticCodes(report.Diagnostics)
	for _, code := range []string{
		"action.binding_ambiguous", "action.not_authorized", "action.risk_exceeds_binding",
		"action.credential_unsatisfied", "action.idempotency_required", "action.approval_unreachable", "budget.actions_impossible",
	} {
		if !codes[code] {
			t.Fatalf("missing %s in %#v", code, report.Diagnostics)
		}
	}
	replayed := Verify(definition, environment)
	if !reflect.DeepEqual(report, replayed) {
		t.Fatalf("verification is not deterministic:\n%#v\n%#v", report, replayed)
	}
}

func TestVerifyRejectsIncompleteTerminalPathAndOversizedChildBudget(t *testing.T) {
	definition := &Definition{
		APIVersion: APIVersion, ID: "delegate", Version: "1", Name: "Delegate",
		Entrypoints: map[string]string{"manual": "choose"},
		Triggers: map[string]Trigger{"manual": {
			Kind: TriggerEvent, EventType: "work.requested", Entrypoint: "manual", ObjectiveID: "work",
			Budget: &BudgetAllocation{MaxTurns: 2, MaxActions: 10},
		}},
		Steps: map[string]Step{
			"choose":   {Kind: StepDecision, Decision: &DecisionStep{Cases: []DecisionCase{{When: Predicate{Operator: PredicateTruthy, Left: value(literal(true))}, Next: "delegate"}}}},
			"delegate": {Kind: StepDelegate, Delegate: &DelegateStep{AgentID: literal("specialist"), Goal: literal("work"), ResultPath: "/child", Budget: &BudgetAllocation{MaxTurns: 3}, Next: "done"}},
			"done":     {Kind: StepEnd, End: &EndStep{}},
		},
	}
	report := Verify(definition, VerificationEnvironment{})
	codes := diagnosticCodes(report.Diagnostics)
	if !codes["graph.terminal_path_missing"] || !codes["budget.child_exceeds_parent"] {
		t.Fatalf("diagnostics = %#v", report.Diagnostics)
	}
}

func TestVerifyRejectsAggregateParallelBudget(t *testing.T) {
	definition := &Definition{
		APIVersion: APIVersion, ID: "parallel", Version: "1", Name: "Parallel",
		Entrypoints: map[string]string{"start": "fork"},
		Triggers:    map[string]Trigger{"scheduled": {Kind: TriggerSchedule, Schedule: &Schedule{Cron: "0 0 * * * *", Timezone: "UTC"}, Entrypoint: "start", Budget: &BudgetAllocation{MaxActions: 3}}},
		Steps: map[string]Step{
			"fork": {Kind: StepFork, Fork: &ForkStep{Branches: map[string]string{"a": "a", "b": "b"}, BranchBudgets: map[string]BudgetAllocation{"a": {MaxActions: 2}, "b": {MaxActions: 2}}, Join: "join"}},
			"a":    {Kind: StepTransform, Transform: &TransformStep{Assignments: map[string]Value{"a": literal(true)}, Next: "join"}},
			"b":    {Kind: StepTransform, Transform: &TransformStep{Assignments: map[string]Value{"b": literal(true)}, Next: "join"}},
			"join": {Kind: StepJoin, Join: &JoinStep{Fork: "fork", Mode: JoinAll, Next: "done"}},
			"done": {Kind: StepEnd, End: &EndStep{}},
		},
	}
	report := Verify(definition, VerificationEnvironment{})
	if !diagnosticCodes(report.Diagnostics)["budget.parallel_exceeds_parent"] {
		t.Fatalf("diagnostics = %#v", report.Diagnostics)
	}
}

func TestVerifyMutationsFailClosedWithStableDiagnostics(t *testing.T) {
	tests := []struct {
		name   string
		code   string
		mutate func(*Definition, *VerificationEnvironment)
	}{
		{name: "unknown successor", code: "step.reference_unknown", mutate: func(definition *Definition, _ *VerificationEnvironment) {
			step := definition.Steps["collect"]
			step.Action.Next = "missing"
			definition.Steps["collect"] = step
		}},
		{name: "invalid result pointer", code: "pointer.invalid", mutate: func(definition *Definition, _ *VerificationEnvironment) {
			step := definition.Steps["collect"]
			step.Action.ResultPath = "not-a-pointer"
			definition.Steps["collect"] = step
		}},
		{name: "missing binding", code: "action.binding_missing", mutate: func(_ *Definition, environment *VerificationEnvironment) {
			environment.Actions = environment.Actions[1:]
		}},
		{name: "missing credential", code: "action.credential_unsatisfied", mutate: func(_ *Definition, environment *VerificationEnvironment) {
			environment.Actions[1].BoundCredentials = nil
		}},
		{name: "missing approval route", code: "action.approval_unreachable", mutate: func(_ *Definition, environment *VerificationEnvironment) {
			environment.Actions[1].ApprovalRoutePresent = false
		}},
		{name: "destructive action without compensation", code: "action.compensation_required", mutate: func(_ *Definition, environment *VerificationEnvironment) {
			environment.Actions[1].MaximumRisk = capability.RiskLevelDestructive
			environment.Actions[1].Risk = capability.RiskLevelDestructive
			environment.Actions[1].SideEffect = capability.SideEffectDestructive
		}},
		{name: "impossible action budget", code: "budget.actions_impossible", mutate: func(definition *Definition, _ *VerificationEnvironment) {
			trigger := definition.Triggers["hourly"]
			trigger.Budget.MaxActions = 1
			definition.Triggers["hourly"] = trigger
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			definition := cloneVerifierValue(t, verifierRunbook())
			environment := cloneVerifierValue(t, verifierEnvironment())
			test.mutate(definition, &environment)
			first := Verify(definition, environment)
			second := Verify(definition, environment)
			if first.Valid || !diagnosticCodes(first.Diagnostics)[test.code] {
				t.Fatalf("expected %s in %#v", test.code, first.Diagnostics)
			}
			if !reflect.DeepEqual(first, second) {
				t.Fatalf("diagnostics drifted:\n%#v\n%#v", first, second)
			}
		})
	}
}

func TestVerifyRepresentativePortableRunbooks(t *testing.T) {
	tests := []struct {
		name, id         string
		firstSkill       string
		firstAction      string
		secondSkill      string
		secondAction     string
		secondRisk       capability.RiskLevel
		secondSideEffect capability.SideEffect
	}{
		{name: "sre response", id: "sre-response", firstSkill: "kubernetes", firstAction: "inspect", secondSkill: "kubernetes", secondAction: "remediate", secondRisk: capability.RiskLevelProduction, secondSideEffect: capability.SideEffectWrite},
		{name: "market research", id: "market-research", firstSkill: "source", firstAction: "read", secondSkill: "delivery", secondAction: "publish", secondRisk: capability.RiskLevelExternal, secondSideEffect: capability.SideEffectExternal},
		{name: "community engagement", id: "community-engagement", firstSkill: "browser", firstAction: "snapshot", secondSkill: "browser", secondAction: "comment", secondRisk: capability.RiskLevelExternal, secondSideEffect: capability.SideEffectExternal},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			definition := cloneVerifierValue(t, verifierRunbook())
			definition.ID = test.id
			definition.Name = test.name
			first := definition.Steps["collect"]
			first.Action.SkillID, first.Action.Action = test.firstSkill, test.firstAction
			definition.Steps["collect"] = first
			second := definition.Steps["publish"]
			second.Action.SkillID, second.Action.Action = test.secondSkill, test.secondAction
			definition.Steps["publish"] = second
			environment := VerificationEnvironment{Actions: []ResolvedAction{
				{SkillID: test.firstSkill, SkillVersion: "1.0.0", Action: test.firstAction, BindingID: "observe", Enabled: true, Allowed: true, MaximumRisk: capability.RiskLevelRead, Risk: capability.RiskLevelRead, SideEffect: capability.SideEffectRead, Idempotency: capability.IdempotencySupported},
				{SkillID: test.secondSkill, SkillVersion: "2.0.0", Action: test.secondAction, BindingID: "change", Enabled: true, Allowed: true, MaximumRisk: test.secondRisk, Risk: test.secondRisk, SideEffect: test.secondSideEffect, Idempotency: capability.IdempotencyRequired, RequiresApproval: true, ApprovalRoutePresent: true},
			}}
			report := Verify(definition, environment)
			if !report.Valid || len(report.Diagnostics) != 0 {
				t.Fatalf("report = %#v", report)
			}
		})
	}
}

func verifierRunbook() *Definition {
	return &Definition{
		APIVersion: APIVersion, ID: "research-delivery", Version: "1.0.0", Name: "Research delivery",
		Entrypoints: map[string]string{"start": "collect"},
		Triggers: map[string]Trigger{"hourly": {
			Kind: TriggerSchedule, Schedule: &Schedule{Cron: "0 0 * * * *", Timezone: "UTC"}, Entrypoint: "start", ObjectiveID: "research",
			Budget: &BudgetAllocation{MaxActions: 4, MaxTurns: 8},
		}},
		Steps: map[string]Step{
			"collect": {Kind: StepAction, Action: &ActionStep{SkillID: "source", SkillVersion: "1.0.0", Action: "read", ResultPath: "/steps/collect", Next: "publish"}},
			"publish": {Kind: StepAction, Action: &ActionStep{SkillID: "delivery", SkillVersion: "2.0.0", Action: "publish", ResultPath: "/steps/publish", Next: "done"}},
			"done":    {Kind: StepEnd, End: &EndStep{}},
		},
	}
}

func verifierEnvironment() VerificationEnvironment {
	return VerificationEnvironment{Actions: []ResolvedAction{
		{SkillID: "source", SkillVersion: "1.0.0", SourceIdentity: "registry.example/source", Action: "read", BindingID: "source", BindingRevision: 1, Enabled: true, Allowed: true, MaximumRisk: capability.RiskLevelRead, Risk: capability.RiskLevelRead, SideEffect: capability.SideEffectRead, Idempotency: capability.IdempotencySupported},
		{SkillID: "delivery", SkillVersion: "2.0.0", SourceIdentity: "registry.example/delivery", Action: "publish", BindingID: "delivery", BindingRevision: 1, Enabled: true, Allowed: true, MaximumRisk: capability.RiskLevelExternal, Risk: capability.RiskLevelExternal, SideEffect: capability.SideEffectExternal, Idempotency: capability.IdempotencyRequired, RequiredCredentials: []capability.CredentialRequirement{{Name: "connection", Kind: "oauth2"}}, BoundCredentials: map[string]capability.CredentialReference{"connection": {Kind: "oauth2", ID: "credential://opaque"}}, RequiresApproval: true, ApprovalRoutePresent: true},
	}}
}

func cloneVerifierValue[T any](t *testing.T, value T) T {
	t.Helper()
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var clone T
	if err := json.Unmarshal(payload, &clone); err != nil {
		t.Fatal(err)
	}
	return clone
}

func diagnosticCodes(diagnostics []Diagnostic) map[string]bool {
	result := make(map[string]bool, len(diagnostics))
	for _, diagnostic := range diagnostics {
		result[diagnostic.Code] = true
	}
	return result
}
