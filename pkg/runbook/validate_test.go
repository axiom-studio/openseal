package runbook

import (
	"encoding/json"
	"testing"
)

func literal(value interface{}) Value {
	encoded, _ := json.Marshal(value)
	return Value{Literal: encoded}
}
func ref(pointer string) Value { return Value{Ref: pointer} }

func TestValidateRepresentativeGovernedRunbook(t *testing.T) {
	definition := &Definition{
		APIVersion: APIVersion, ID: "release-observer", Version: "1.0.0", Name: "Release observer",
		Entrypoints: map[string]string{"manual": "normalize"},
		Steps: map[string]Step{
			"normalize": {Kind: StepTransform, Transform: &TransformStep{Assignments: map[string]Value{"/state/services": ref("/input/services")}, Next: "choose"}},
			"choose":    {Kind: StepDecision, Decision: &DecisionStep{Cases: []DecisionCase{{When: Predicate{Operator: PredicateTruthy, Left: value(ref("/input/parallel"))}, Next: "fork"}}, Default: "loop"}},
			"fork":      {Kind: StepFork, Fork: &ForkStep{Branches: map[string]string{"health": "health", "events": "events"}, Join: "joined"}},
			"health":    {Kind: StepAction, Action: &ActionStep{SkillID: "kubernetes", SkillVersion: "1.0.0", Action: "health", Arguments: map[string]Value{"service": ref("/input/service")}, ResultPath: "/steps/health", Next: "joined"}},
			"events":    {Kind: StepWait, Wait: &WaitStep{Event: "kubernetes.event", Next: "joined"}},
			"joined":    {Kind: StepJoin, Join: &JoinStep{Fork: "fork", Mode: JoinAll, Next: "done"}},
			"loop":      {Kind: StepForEach, ForEach: &ForEachStep{Items: ref("/state/services"), ItemName: "service", MaxIterations: 100, Body: "inspect", Next: "done"}},
			"inspect":   {Kind: StepAction, Action: &ActionStep{SkillID: "kubernetes", SkillVersion: "1.0.0", Action: "inspect", Arguments: map[string]Value{"service": ref("/loop/service")}, ResultPath: "/steps/inspect", Next: "again"}},
			"again":     {Kind: StepLoopReturn, LoopReturn: &LoopReturnStep{ForEach: "loop"}},
			"done":      {Kind: StepEnd, End: &EndStep{Outputs: map[string]Value{"status": literal("complete")}}},
		},
	}
	if diagnostics := Validate(definition); len(diagnostics) != 0 {
		t.Fatalf("diagnostics = %#v", diagnostics)
	}
}

func TestValidateReturnsPathSpecificDiagnostics(t *testing.T) {
	definition := &Definition{APIVersion: "wrong", ID: "bad", Version: "1", Name: "Bad", Entrypoints: map[string]string{"manual": "a"}, Steps: map[string]Step{
		"a":      {Kind: StepAction, Action: &ActionStep{SkillID: "", SkillVersion: "1", Action: "run", ResultPath: "not-pointer", Next: "b"}},
		"b":      {Kind: StepTransform, Transform: &TransformStep{Assignments: map[string]Value{"bad": {}}, Next: "a"}},
		"orphan": {Kind: StepEnd, End: &EndStep{}},
	}}
	diagnostics := Validate(definition)
	codes := map[string]bool{}
	paths := map[string]bool{}
	for _, diagnostic := range diagnostics {
		codes[diagnostic.Code] = true
		paths[diagnostic.Path] = true
	}
	for _, code := range []string{"api_version.unsupported", "action.identity_required", "pointer.invalid", "value.source", "graph.implicit_cycle", "step.unreachable"} {
		if !codes[code] {
			t.Fatalf("missing %s in %#v", code, diagnostics)
		}
	}
	if !paths["steps.a.action.resultPath"] || !paths["steps.orphan"] {
		t.Fatalf("paths = %#v", diagnostics)
	}
}

func TestValidateDurableAgentDelegation(t *testing.T) {
	definition := &Definition{APIVersion: APIVersion, ID: "delegate", Version: "1", Name: "Delegate", Entrypoints: map[string]string{"manual": "specialist"}, Steps: map[string]Step{
		"specialist": {Kind: StepDelegate, Delegate: &DelegateStep{
			AgentID: literal("marketing"), Goal: ref("/input/goal"), Context: map[string]Value{"release": ref("/input/release")},
			ResultPath: "/steps/specialist", Next: "done",
		}},
		"done": {Kind: StepEnd, End: &EndStep{Outputs: map[string]Value{"result": ref("/steps/specialist")}}},
	}}
	if diagnostics := Validate(definition); len(diagnostics) != 0 {
		t.Fatalf("diagnostics=%#v", diagnostics)
	}
	definition.Steps["specialist"] = Step{Kind: StepDelegate, Delegate: &DelegateStep{ResultPath: "bad", Next: "missing"}}
	diagnostics := Validate(definition)
	codes := map[string]bool{}
	for _, diagnostic := range diagnostics {
		codes[diagnostic.Code] = true
	}
	if !codes["value.source"] || !codes["pointer.invalid"] || !codes["step.reference_unknown"] {
		t.Fatalf("diagnostics=%#v", diagnostics)
	}
}

func value(input Value) *Value { return &input }
