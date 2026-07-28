package runbook

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

const maximumIterations = 10000

var concreteEventTypePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]*(\.[a-z][a-z0-9_-]*)+$`)

func Validate(definition *Definition) []Diagnostic {
	v := validator{definition: definition}
	v.validate()
	sort.SliceStable(v.diagnostics, func(i, j int) bool {
		if v.diagnostics[i].Path == v.diagnostics[j].Path {
			return v.diagnostics[i].Code < v.diagnostics[j].Code
		}
		return v.diagnostics[i].Path < v.diagnostics[j].Path
	})
	return v.diagnostics
}

type validator struct {
	definition  *Definition
	diagnostics []Diagnostic
}

func (v *validator) add(path, code, format string, args ...interface{}) {
	v.diagnostics = append(v.diagnostics, Diagnostic{Path: path, Code: code, Message: fmt.Sprintf(format, args...)})
}

func (v *validator) validate() {
	if v.definition == nil {
		v.add("$", "definition.required", "runbook definition is required")
		return
	}
	if v.definition.APIVersion != APIVersion {
		v.add("apiVersion", "api_version.unsupported", "apiVersion must be %q", APIVersion)
	}
	for path, value := range map[string]string{"id": v.definition.ID, "version": v.definition.Version, "name": v.definition.Name} {
		if strings.TrimSpace(value) == "" {
			v.add(path, "identity.required", "%s is required", path)
		}
	}
	if len(v.definition.Entrypoints) == 0 {
		v.add("entrypoints", "entrypoint.required", "at least one entrypoint is required")
	}
	if len(v.definition.Steps) == 0 {
		v.add("steps", "step.required", "at least one step is required")
		return
	}
	for name, target := range v.definition.Entrypoints {
		if strings.TrimSpace(name) == "" {
			v.add("entrypoints", "entrypoint.name_required", "entrypoint names cannot be empty")
		}
		v.requireStep("entrypoints."+name, target)
	}
	for name, contract := range v.definition.Interfaces {
		if _, ok := v.definition.Entrypoints[name]; !ok {
			v.add("interfaces."+name, "interface.entrypoint_unknown", "callable interface must name an exact entrypoint")
		}
		if strings.TrimSpace(contract.Description) == "" {
			v.add("interfaces."+name+".description", "interface.description_required", "callable interface description is required")
		}
		if schemaType, _ := contract.InputSchema["type"].(string); schemaType != "object" {
			v.add("interfaces."+name+".inputSchema", "interface.input_object_required", "callable input schema must have type object")
		} else if _, err := compileInterfaceSchema(contract.InputSchema); err != nil {
			v.add("interfaces."+name+".inputSchema", "interface.input_schema_invalid", "callable input schema is invalid: %v", err)
		}
		if len(contract.OutputSchema) > 0 {
			if schemaType, _ := contract.OutputSchema["type"].(string); schemaType != "object" {
				v.add("interfaces."+name+".outputSchema", "interface.output_object_required", "callable output schema must have type object")
			} else if _, err := compileInterfaceSchema(contract.OutputSchema); err != nil {
				v.add("interfaces."+name+".outputSchema", "interface.output_schema_invalid", "callable output schema is invalid: %v", err)
			}
		}
	}
	for id, trigger := range v.definition.Triggers {
		path := "triggers." + id
		if strings.TrimSpace(id) == "" || len(id) > 128 {
			v.add(path, "trigger.id_invalid", "trigger id must be 1-128 characters")
		}
		switch trigger.Kind {
		case TriggerEvent:
			if !concreteEventTypePattern.MatchString(trigger.EventType) || len(trigger.EventType) > 160 {
				v.add(path+".eventType", "trigger.event_type_invalid", "event trigger must name a concrete portable event type")
			}
			if trigger.Schedule != nil {
				v.add(path+".schedule", "trigger.schedule_forbidden", "event trigger cannot declare a schedule")
			}
		case TriggerSchedule:
			if strings.TrimSpace(trigger.EventType) != "" {
				v.add(path+".eventType", "trigger.event_type_forbidden", "schedule trigger cannot declare an event type")
			}
			if err := trigger.Schedule.Validate(); err != nil {
				v.add(path+".schedule", "trigger.schedule_invalid", "%v", err)
			}
			if strings.TrimSpace(trigger.ObjectiveID) == "" || len(trigger.ObjectiveID) > 160 {
				v.add(path+".objectiveId", "trigger.objective_required", "schedule trigger must belong to one Objective")
			}
			if trigger.MaximumConcurrent < 0 {
				v.add(path+".maximumConcurrent", "trigger.concurrency_invalid", "maximum concurrency cannot be negative")
			}
			if trigger.Budget != nil {
				if err := trigger.Budget.Validate(); err != nil {
					v.add(path+".budget", "trigger.budget_invalid", "%v", err)
				}
			}
			for name, value := range trigger.Input {
				inputPath := path + ".input." + name
				if strings.TrimSpace(name) == "" {
					v.add(path+".input", "trigger.input_name", "trigger input names cannot be empty")
				}
				v.validateValue(inputPath, value)
				if value.Ref != "" || len(value.Template) > 0 {
					v.add(inputPath, "trigger.input_static", "scheduled trigger input must be a credential-free literal")
				}
			}
		default:
			v.add(path+".kind", "trigger.kind_unsupported", "trigger kind must be %q or %q", TriggerEvent, TriggerSchedule)
		}
		if _, ok := v.definition.Entrypoints[trigger.Entrypoint]; !ok {
			v.add(path+".entrypoint", "trigger.entrypoint_unknown", "trigger must name an exact entrypoint")
		}
	}
	for id, step := range v.definition.Steps {
		path := "steps." + id
		if strings.TrimSpace(id) == "" {
			v.add("steps", "step.id_required", "step IDs cannot be empty")
		}
		v.validateStep(path, id, step)
	}
	v.validateStructuredControl()
	v.validateReachability()
	v.validateAcyclic()
}

func (v *validator) validateStructuredControl() {
	for id, step := range v.definition.Steps {
		path := "steps." + id
		switch step.Kind {
		case StepFork:
			if step.Fork == nil {
				continue
			}
			if join, ok := v.definition.Steps[step.Fork.Join]; ok && join.Join != nil && join.Join.Fork != id {
				v.add(path+".fork.join", "fork.join_mismatch", "join %q belongs to fork %q", step.Fork.Join, join.Join.Fork)
			}
			for name, start := range step.Fork.Branches {
				if _, ok := v.definition.Steps[start]; ok && !v.pathReaches(start, step.Fork.Join, nil) {
					v.add(path+".fork.branches."+name, "fork.join_unreachable", "branch does not reach join %q", step.Fork.Join)
				}
			}
		case StepJoin:
			if step.Join == nil {
				continue
			}
			if fork, ok := v.definition.Steps[step.Join.Fork]; ok && fork.Fork != nil && fork.Fork.Join != id {
				v.add(path+".join.fork", "join.fork_mismatch", "fork %q targets join %q", step.Join.Fork, fork.Fork.Join)
			}
		case StepForEach:
			if step.ForEach == nil {
				continue
			}
			matchesLoop := func(candidate string) bool {
				loop, ok := v.definition.Steps[candidate]
				return ok && loop.LoopReturn != nil && loop.LoopReturn.ForEach == id
			}
			if _, ok := v.definition.Steps[step.ForEach.Body]; ok && !v.pathReaches(step.ForEach.Body, "", matchesLoop) {
				v.add(path+".forEach.body", "loop.return_unreachable", "loop body must reach a loop_return for %q", id)
			}
		case StepLoopReturn:
			if step.LoopReturn == nil {
				continue
			}
			if loop, ok := v.definition.Steps[step.LoopReturn.ForEach]; ok && loop.ForEach != nil && !v.pathReaches(loop.ForEach.Body, id, nil) {
				v.add(path+".loopReturn.forEach", "loop.return_detached", "loop_return is not reachable from for_each %q body", step.LoopReturn.ForEach)
			}
		}
	}
}

func (v *validator) pathReaches(start, target string, match func(string) bool) bool {
	visited := map[string]bool{}
	var visit func(string) bool
	visit = func(id string) bool {
		if id == target && target != "" || match != nil && match(id) {
			return true
		}
		if visited[id] {
			return false
		}
		visited[id] = true
		for _, next := range v.successors(id) {
			if visit(next) {
				return true
			}
		}
		return false
	}
	return visit(start)
}

func (v *validator) validateStep(path, id string, step Step) {
	count := 0
	for _, present := range []bool{
		step.Action != nil, step.Delegate != nil, step.Decision != nil, step.Transform != nil, step.Wait != nil,
		step.Fork != nil, step.Join != nil, step.ForEach != nil, step.LoopReturn != nil, step.End != nil,
	} {
		if present {
			count++
		}
	}
	if count != 1 {
		v.add(path, "step.payload", "a step must contain exactly one typed payload")
	}
	validKind := step.Kind == StepAction || step.Kind == StepDelegate || step.Kind == StepDecision || step.Kind == StepTransform || step.Kind == StepWait ||
		step.Kind == StepFork || step.Kind == StepJoin || step.Kind == StepForEach || step.Kind == StepLoopReturn || step.Kind == StepEnd
	if !validKind {
		v.add(path+".kind", "step.kind_unsupported", "unsupported step kind %q", step.Kind)
	} else if !stepPayloadMatchesKind(step) {
		v.add(path, "step.kind_mismatch", "step kind %q does not match its payload", step.Kind)
	}
	switch step.Kind {
	case StepAction:
		if step.Action == nil {
			return
		}
		for field, value := range map[string]string{"skillId": step.Action.SkillID, "skillVersion": step.Action.SkillVersion, "action": step.Action.Action, "resultPath": step.Action.ResultPath} {
			if strings.TrimSpace(value) == "" {
				v.add(path+".action."+field, "action.identity_required", "%s is required", field)
			}
		}
		v.validatePointer(path+".action.resultPath", step.Action.ResultPath)
		for key, value := range step.Action.Arguments {
			v.validateValue(path+".action.arguments."+key, value)
		}
		v.requireStep(path+".action.next", step.Action.Next)
	case StepDelegate:
		if step.Delegate == nil {
			return
		}
		v.validateValue(path+".delegate.agentId", step.Delegate.AgentID)
		v.validateValue(path+".delegate.goal", step.Delegate.Goal)
		for key, value := range step.Delegate.Context {
			if strings.TrimSpace(key) == "" {
				v.add(path+".delegate.context", "delegate.context_key", "context keys cannot be empty")
			}
			v.validateValue(path+".delegate.context."+key, value)
		}
		v.validatePointer(path+".delegate.resultPath", step.Delegate.ResultPath)
		if step.Delegate.Mode != "" && step.Delegate.Mode != DelegateBehavior && step.Delegate.Mode != DelegateReason {
			v.add(path+".delegate.mode", "delegate.mode", "delegation mode must be behavior or reason")
		}
		if step.Delegate.Timeout < 0 {
			v.add(path+".delegate.timeout", "delegate.timeout", "timeout cannot be negative")
		}
		if step.Delegate.Budget != nil {
			if err := step.Delegate.Budget.Validate(); err != nil {
				v.add(path+".delegate.budget", "delegate.budget", "%v", err)
			}
		}
		v.requireStep(path+".delegate.next", step.Delegate.Next)
	case StepDecision:
		if step.Decision == nil {
			return
		}
		if len(step.Decision.Cases) == 0 {
			v.add(path+".decision.cases", "decision.case_required", "at least one case is required")
		}
		for index, item := range step.Decision.Cases {
			casePath := fmt.Sprintf("%s.decision.cases[%d]", path, index)
			v.validatePredicate(casePath+".when", item.When)
			v.requireStep(casePath+".next", item.Next)
		}
		if step.Decision.Default != "" {
			v.requireStep(path+".decision.default", step.Decision.Default)
		}
	case StepTransform:
		if step.Transform == nil {
			return
		}
		if len(step.Transform.Assignments) == 0 {
			v.add(path+".transform.assignments", "transform.assignment_required", "at least one assignment is required")
		}
		for pointer, value := range step.Transform.Assignments {
			v.validatePointer(path+".transform.assignments."+pointer, pointer)
			v.validateValue(path+".transform.assignments."+pointer, value)
		}
		v.requireStep(path+".transform.next", step.Transform.Next)
	case StepWait:
		if step.Wait == nil {
			return
		}
		if (step.Wait.Duration > 0) == (strings.TrimSpace(step.Wait.Event) != "") {
			v.add(path+".wait", "wait.source", "exactly one positive duration or event is required")
		}
		v.requireStep(path+".wait.next", step.Wait.Next)
	case StepFork:
		if step.Fork == nil {
			return
		}
		if len(step.Fork.BranchBudgets) > 0 {
			for name := range step.Fork.Branches {
				budget, ok := step.Fork.BranchBudgets[name]
				if !ok {
					v.add(path+".fork.branchBudgets."+name, "fork.budget_required", "branch %q requires an explicit budget allocation", name)
					continue
				}
				if err := budget.Validate(); err != nil {
					v.add(path+".fork.branchBudgets."+name, "fork.budget", "%v", err)
				}
			}
			for name := range step.Fork.BranchBudgets {
				if _, ok := step.Fork.Branches[name]; !ok {
					v.add(path+".fork.branchBudgets."+name, "fork.budget_unknown_branch", "budget references unknown branch %q", name)
				}
			}
		}
		if len(step.Fork.Branches) < 2 {
			v.add(path+".fork.branches", "fork.branches", "a fork requires at least two named branches")
		}
		for name, target := range step.Fork.Branches {
			if strings.TrimSpace(name) == "" {
				v.add(path+".fork.branches", "fork.branch_name", "branch names cannot be empty")
			}
			v.requireStep(path+".fork.branches."+name, target)
		}
		v.requireKind(path+".fork.join", step.Fork.Join, StepJoin)
	case StepJoin:
		if step.Join == nil {
			return
		}
		v.requireKind(path+".join.fork", step.Join.Fork, StepFork)
		if step.Join.Mode != JoinAll && step.Join.Mode != JoinAny {
			v.add(path+".join.mode", "join.mode", "join mode must be all or any")
		}
		v.requireStep(path+".join.next", step.Join.Next)
	case StepForEach:
		if step.ForEach == nil {
			return
		}
		v.validateValue(path+".forEach.items", step.ForEach.Items)
		if strings.TrimSpace(step.ForEach.ItemName) == "" {
			v.add(path+".forEach.itemName", "loop.item_name", "itemName is required")
		}
		if step.ForEach.MaxIterations < 1 || step.ForEach.MaxIterations > maximumIterations {
			v.add(path+".forEach.maxIterations", "loop.bound", "maxIterations must be between 1 and %d", maximumIterations)
		}
		v.requireStep(path+".forEach.body", step.ForEach.Body)
		v.requireStep(path+".forEach.next", step.ForEach.Next)
	case StepLoopReturn:
		if step.LoopReturn == nil {
			return
		}
		v.requireKind(path+".loopReturn.forEach", step.LoopReturn.ForEach, StepForEach)
	case StepEnd:
		if step.End == nil {
			return
		}
		for key, value := range step.End.Outputs {
			v.validateValue(path+".end.outputs."+key, value)
		}
	}
	_ = id
}

func stepPayloadMatchesKind(step Step) bool {
	switch step.Kind {
	case StepAction:
		return step.Action != nil
	case StepDelegate:
		return step.Delegate != nil
	case StepDecision:
		return step.Decision != nil
	case StepTransform:
		return step.Transform != nil
	case StepWait:
		return step.Wait != nil
	case StepFork:
		return step.Fork != nil
	case StepJoin:
		return step.Join != nil
	case StepForEach:
		return step.ForEach != nil
	case StepLoopReturn:
		return step.LoopReturn != nil
	case StepEnd:
		return step.End != nil
	default:
		return false
	}
}

func (v *validator) validateValue(path string, value Value) {
	hasLiteral := len(value.Literal) > 0
	hasRef := strings.TrimSpace(value.Ref) != ""
	hasTemplate := len(value.Template) > 0
	sources := 0
	for _, present := range []bool{hasLiteral, hasRef, hasTemplate} {
		if present {
			sources++
		}
	}
	if sources != 1 {
		v.add(path, "value.source", "exactly one literal, ref, or template is required")
		return
	}
	if hasLiteral && !json.Valid(value.Literal) {
		v.add(path+".literal", "value.literal_invalid", "literal must be valid JSON")
	}
	if hasRef {
		v.validatePointer(path+".ref", value.Ref)
	}
	if hasTemplate {
		for index, segment := range value.Template {
			segmentPath := fmt.Sprintf("%s.template[%d]", path, index)
			if (segment.Text == "") == (strings.TrimSpace(segment.Ref) == "") {
				v.add(segmentPath, "template.segment", "template segment requires exactly one text or ref")
				continue
			}
			if segment.Ref != "" {
				v.validatePointer(segmentPath+".ref", segment.Ref)
			}
		}
	}
}

func (v *validator) validatePointer(path, pointer string) {
	if pointer == "" || !strings.HasPrefix(pointer, "/") {
		v.add(path, "pointer.invalid", "value must be a non-empty JSON Pointer")
	}
}

func (v *validator) validatePredicate(path string, predicate Predicate) {
	switch predicate.Operator {
	case PredicateAll, PredicateAny:
		if len(predicate.Operands) < 2 {
			v.add(path+".operands", "predicate.arity", "%s requires at least two operands", predicate.Operator)
		}
		for index, child := range predicate.Operands {
			v.validatePredicate(fmt.Sprintf("%s.operands[%d]", path, index), child)
		}
	case PredicateNot:
		if len(predicate.Operands) != 1 {
			v.add(path+".operands", "predicate.arity", "not requires one operand")
		} else {
			v.validatePredicate(path+".operands[0]", predicate.Operands[0])
		}
	case PredicateExists, PredicateTruthy:
		if predicate.Left == nil || predicate.Right != nil || len(predicate.Operands) != 0 {
			v.add(path, "predicate.arity", "%s requires only left", predicate.Operator)
		} else {
			v.validateValue(path+".left", *predicate.Left)
		}
	case PredicateEqual, PredicateNotEqual, PredicateGreater, PredicateAtLeast, PredicateLess, PredicateAtMost, PredicateContains:
		if predicate.Left == nil || predicate.Right == nil || len(predicate.Operands) != 0 {
			v.add(path, "predicate.arity", "%s requires left and right", predicate.Operator)
		} else {
			v.validateValue(path+".left", *predicate.Left)
			v.validateValue(path+".right", *predicate.Right)
		}
	default:
		v.add(path+".operator", "predicate.operator", "unsupported predicate operator %q", predicate.Operator)
	}
}

func (v *validator) requireStep(path, id string) {
	if strings.TrimSpace(id) == "" {
		v.add(path, "step.reference_required", "step reference is required")
		return
	}
	if _, ok := v.definition.Steps[id]; !ok {
		v.add(path, "step.reference_unknown", "unknown step %q", id)
	}
}

func (v *validator) requireKind(path, id string, kind StepKind) {
	v.requireStep(path, id)
	if step, ok := v.definition.Steps[id]; ok && step.Kind != kind {
		v.add(path, "step.reference_kind", "step %q must have kind %q", id, kind)
	}
}

func (v *validator) successors(id string) []string {
	step, ok := v.definition.Steps[id]
	if !ok {
		return nil
	}
	switch step.Kind {
	case StepAction:
		if step.Action != nil {
			return []string{step.Action.Next}
		}
	case StepDelegate:
		if step.Delegate != nil {
			return []string{step.Delegate.Next}
		}
	case StepDecision:
		if step.Decision != nil {
			out := make([]string, 0, len(step.Decision.Cases)+1)
			for _, c := range step.Decision.Cases {
				out = append(out, c.Next)
			}
			if step.Decision.Default != "" {
				out = append(out, step.Decision.Default)
			}
			return out
		}
	case StepTransform:
		if step.Transform != nil {
			return []string{step.Transform.Next}
		}
	case StepWait:
		if step.Wait != nil {
			return []string{step.Wait.Next}
		}
	case StepFork:
		if step.Fork != nil {
			out := make([]string, 0, len(step.Fork.Branches))
			for _, target := range step.Fork.Branches {
				out = append(out, target)
			}
			return out
		}
	case StepJoin:
		if step.Join != nil {
			return []string{step.Join.Next}
		}
	case StepForEach:
		if step.ForEach != nil {
			return []string{step.ForEach.Body, step.ForEach.Next}
		}
	}
	return nil
}

func (v *validator) validateReachability() {
	reachable := map[string]bool{}
	var visit func(string)
	visit = func(id string) {
		if reachable[id] {
			return
		}
		if _, ok := v.definition.Steps[id]; !ok {
			return
		}
		reachable[id] = true
		for _, next := range v.successors(id) {
			visit(next)
		}
	}
	for _, entry := range v.definition.Entrypoints {
		visit(entry)
	}
	for id := range v.definition.Steps {
		if !reachable[id] {
			v.add("steps."+id, "step.unreachable", "step is unreachable from every entrypoint")
		}
	}
}

func (v *validator) validateAcyclic() {
	state := map[string]int{}
	var visit func(string)
	visit = func(id string) {
		if state[id] == 2 {
			return
		}
		if state[id] == 1 {
			v.add("steps."+id, "graph.implicit_cycle", "implicit cycles are forbidden; use a bounded for_each with loop_return")
			return
		}
		state[id] = 1
		for _, next := range v.successors(id) {
			visit(next)
		}
		state[id] = 2
	}
	for _, entry := range v.definition.Entrypoints {
		visit(entry)
	}
}
