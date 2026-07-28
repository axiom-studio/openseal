package runbook

import (
	"encoding/json"
	"testing"
	"time"
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

func TestValidateCallableEntrypointInterfaces(t *testing.T) {
	definition := &Definition{
		APIVersion: APIVersion, ID: "release", Version: "1", Name: "Release",
		Entrypoints: map[string]string{"collect": "done"},
		Interfaces: map[string]Interface{"collect": {
			Description:  "Collect release evidence.",
			InputSchema:  map[string]interface{}{"type": "object"},
			OutputSchema: map[string]interface{}{"type": "object"},
		}},
		Steps: map[string]Step{"done": {Kind: StepEnd, End: &EndStep{}}},
	}
	if diagnostics := Validate(definition); len(diagnostics) != 0 {
		t.Fatalf("diagnostics=%#v", diagnostics)
	}
	definition.Interfaces["unknown"] = Interface{
		Description: "Unknown.", InputSchema: map[string]interface{}{"type": "array"},
	}
	diagnostics := Validate(definition)
	if len(diagnostics) != 2 || diagnostics[0].Code != "interface.entrypoint_unknown" ||
		diagnostics[1].Code != "interface.input_object_required" {
		t.Fatalf("diagnostics=%#v", diagnostics)
	}
}

func TestValidatePortableEventTriggers(t *testing.T) {
	definition := &Definition{
		APIVersion: APIVersion, ID: "chatbot", Version: "1", Name: "Chatbot",
		Entrypoints: map[string]string{"on-message": "done"},
		Triggers: map[string]Trigger{"conversation-message": {
			Kind: TriggerEvent, EventType: "conversation.message.received", Entrypoint: "on-message",
		}},
		Steps: map[string]Step{"done": {Kind: StepEnd, End: &EndStep{}}},
	}
	if diagnostics := Validate(definition); len(diagnostics) != 0 {
		t.Fatalf("diagnostics=%#v", diagnostics)
	}
	definition.Triggers["invalid"] = Trigger{Kind: "timer", EventType: "slack_message", Entrypoint: "missing"}
	diagnostics := Validate(definition)
	codes := map[string]bool{}
	for _, diagnostic := range diagnostics {
		codes[diagnostic.Code] = true
	}
	for _, code := range []string{"trigger.kind_unsupported", "trigger.entrypoint_unknown"} {
		if !codes[code] {
			t.Fatalf("missing %s in %#v", code, diagnostics)
		}
	}
}

func TestValidatePortableScheduledTriggers(t *testing.T) {
	definition := &Definition{
		APIVersion: APIVersion, ID: "daily-review", Version: "1", Name: "Daily review",
		Entrypoints: map[string]string{"start": "done"},
		Steps:       map[string]Step{"done": {Kind: StepEnd, End: &EndStep{}}},
	}
	definition.Triggers = map[string]Trigger{"daily": {
		Kind: TriggerSchedule, Schedule: &Schedule{Cron: "0 0 0 * * *", Timezone: "UTC", JitterSeconds: 86399}, Entrypoint: "start",
	}}
	if diagnostics := Validate(definition); len(diagnostics) != 0 {
		t.Fatalf("scheduled trigger diagnostics = %#v", diagnostics)
	}

	definition.Triggers["daily"] = Trigger{Kind: TriggerSchedule, EventType: "clock.tick", Schedule: &Schedule{Cron: "0 0 0 * *", Timezone: "UTC"}, Entrypoint: "start"}
	diagnostics := Validate(definition)
	if len(diagnostics) != 2 || diagnostics[0].Code != "trigger.event_type_forbidden" || diagnostics[1].Code != "trigger.schedule_invalid" {
		t.Fatalf("invalid scheduled trigger diagnostics = %#v", diagnostics)
	}
}

func TestScheduleJitterIsStableAndOwnedByCronOccurrence(t *testing.T) {
	schedule := &Schedule{Cron: "0 0 0 * * *", Timezone: "UTC", JitterSeconds: 86399}
	base, err := schedule.NextBase(time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC))
	if err != nil || !base.Equal(time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("next base = %v, %v", base, err)
	}
	first, err := schedule.DueAt("tenant/3/rowan:daily", base)
	if err != nil {
		t.Fatal(err)
	}
	replayed, _ := schedule.DueAt("tenant/3/rowan:daily", base)
	other, _ := schedule.DueAt("tenant/3/another:daily", base)
	windowStart, windowEnd, _ := schedule.Window(base)
	if first != replayed || first.Before(windowStart) || first.After(windowEnd) || first == other {
		t.Fatalf("jitter first=%v replayed=%v other=%v window=[%v,%v]", first, replayed, other, windowStart, windowEnd)
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

func TestValidateExplicitChildBudgets(t *testing.T) {
	definition := &Definition{APIVersion: APIVersion, ID: "budgeted", Version: "1", Name: "Budgeted", Entrypoints: map[string]string{"manual": "delegate"}, Steps: map[string]Step{
		"delegate": {Kind: StepDelegate, Delegate: &DelegateStep{AgentID: literal("agent"), Goal: literal("work"), ResultPath: "/steps/delegate", Budget: &BudgetAllocation{MaxTurns: 2}, Next: "fork"}},
		"fork":     {Kind: StepFork, Fork: &ForkStep{Branches: map[string]string{"a": "a", "b": "b"}, BranchBudgets: map[string]BudgetAllocation{"a": {MaxActions: 1}, "b": {MaxDurationMS: 1000}}, Join: "join"}},
		"a":        {Kind: StepTransform, Transform: &TransformStep{Assignments: map[string]Value{"/state/a": literal(true)}, Next: "join"}},
		"b":        {Kind: StepTransform, Transform: &TransformStep{Assignments: map[string]Value{"/state/b": literal(true)}, Next: "join"}},
		"join":     {Kind: StepJoin, Join: &JoinStep{Fork: "fork", Mode: JoinAll, Next: "done"}},
		"done":     {Kind: StepEnd, End: &EndStep{}},
	}}
	if diagnostics := Validate(definition); len(diagnostics) != 0 {
		t.Fatalf("diagnostics=%#v", diagnostics)
	}
	definition.Steps["delegate"].Delegate.Budget = &BudgetAllocation{}
	delete(definition.Steps["fork"].Fork.BranchBudgets, "b")
	definition.Steps["fork"].Fork.BranchBudgets["unknown"] = BudgetAllocation{MaxTurns: -1}
	diagnostics := Validate(definition)
	codes := map[string]bool{}
	for _, diagnostic := range diagnostics {
		codes[diagnostic.Code] = true
	}
	for _, code := range []string{"delegate.budget", "fork.budget_required", "fork.budget_unknown_branch"} {
		if !codes[code] {
			t.Fatalf("missing %s in %#v", code, diagnostics)
		}
	}
}

func TestValidateTypedTemplateSegments(t *testing.T) {
	definition := &Definition{APIVersion: APIVersion, ID: "template", Version: "1", Name: "Template", Entrypoints: map[string]string{"manual": "set"}, Steps: map[string]Step{
		"set":  {Kind: StepTransform, Transform: &TransformStep{Assignments: map[string]Value{"/state/message": {Template: []TemplateSegment{{Text: "Hello "}, {Ref: "/input/name"}}}}, Next: "done"}},
		"done": {Kind: StepEnd, End: &EndStep{}},
	}}
	if diagnostics := Validate(definition); len(diagnostics) != 0 {
		t.Fatalf("diagnostics=%#v", diagnostics)
	}
	definition.Steps["set"].Transform.Assignments["/state/message"] = Value{Template: []TemplateSegment{{Text: "bad", Ref: "/input/name"}}}
	diagnostics := Validate(definition)
	if len(diagnostics) == 0 || diagnostics[0].Code != "template.segment" {
		t.Fatalf("diagnostics=%#v", diagnostics)
	}
}

func value(input Value) *Value { return &input }
