package runbook

import (
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

func diagnosticCodes(diagnostics []Diagnostic) map[string]bool {
	result := make(map[string]bool, len(diagnostics))
	for _, diagnostic := range diagnostics {
		result[diagnostic.Code] = true
	}
	return result
}
