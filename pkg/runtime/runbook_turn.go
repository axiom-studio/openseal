package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/runbook"
)

const maximumRunbookStepsPerTurn = 256

type RunbookTurnRunner struct {
	definition *runbook.Definition
	entrypoint string
	now        func() time.Time
}

func NewRunbookTurnRunner(definition *runbook.Definition, entrypoint string) (*RunbookTurnRunner, error) {
	if diagnostics := runbook.Validate(definition); len(diagnostics) > 0 {
		return nil, fmt.Errorf("invalid runbook %s: %s", diagnostics[0].Path, diagnostics[0].Message)
	}
	entrypoint = strings.TrimSpace(entrypoint)
	if entrypoint == "" {
		entrypoint = "manual"
	}
	if _, ok := definition.Entrypoints[entrypoint]; !ok {
		return nil, fmt.Errorf("runbook entrypoint %q is not defined", entrypoint)
	}
	return &RunbookTurnRunner{definition: definition, entrypoint: entrypoint, now: time.Now}, nil
}

type runbookExecutionState struct {
	Current       string                 `json:"current"`
	PendingAction string                 `json:"pendingAction,omitempty"`
	PendingFork   string                 `json:"pendingFork,omitempty"`
	BranchFork    string                 `json:"branchFork,omitempty"`
	BranchID      string                 `json:"branchId,omitempty"`
	BranchJoin    string                 `json:"branchJoin,omitempty"`
	Waiting       string                 `json:"waiting,omitempty"`
	Loops         map[string]runbookLoop `json:"loops,omitempty"`
}
type runbookLoop struct {
	Items []interface{} `json:"items"`
	Index int           `json:"index"`
}

func (r *RunbookTurnRunner) RunTurn(_ context.Context, input TurnExecutionContext) (*TurnOutcome, error) {
	if r == nil || r.definition == nil || input.Run == nil || input.Turn == nil {
		return nil, errors.New("runbook turn requires a definition, Run, and Turn")
	}
	checkpoint := cloneMap(input.Run.Checkpoint)
	if checkpoint == nil {
		checkpoint = map[string]interface{}{}
	}
	initializeRunbookInput(checkpoint, input.Run)
	state, err := decodeRunbookState(checkpoint)
	if err != nil {
		return nil, err
	}
	if state.Current == "" {
		state.Current = r.definition.Entrypoints[r.entrypoint]
	}
	decisions := []TurnDecision{}
	for count := 0; count < maximumRunbookStepsPerTurn; count++ {
		step, ok := r.definition.Steps[state.Current]
		if !ok {
			return nil, fmt.Errorf("runbook step %q does not exist", state.Current)
		}
		switch step.Kind {
		case runbook.StepTransform:
			for pointer, value := range step.Transform.Assignments {
				resolved, resolveErr := resolveRunbookValue(checkpoint, value)
				if resolveErr != nil {
					return nil, stepError(state.Current, resolveErr)
				}
				if setErr := setRunbookPointer(checkpoint, pointer, resolved); setErr != nil {
					return nil, stepError(state.Current, setErr)
				}
			}
			decisions = append(decisions, TurnDecision{Summary: "Applied deterministic transform " + state.Current})
			state.Current = step.Transform.Next
		case runbook.StepDecision:
			next := step.Decision.Default
			for _, candidate := range step.Decision.Cases {
				matched, matchErr := evaluateRunbookPredicate(checkpoint, candidate.When)
				if matchErr != nil {
					return nil, stepError(state.Current, matchErr)
				}
				if matched {
					next = candidate.Next
					break
				}
			}
			if next == "" {
				return nil, fmt.Errorf("runbook decision %q matched no case and has no default", state.Current)
			}
			decisions = append(decisions, TurnDecision{Summary: fmt.Sprintf("Selected %s from decision %s", next, state.Current)})
			state.Current = next
		case runbook.StepAction:
			if state.PendingAction == state.Current {
				last, ok := checkpoint["lastAction"].(map[string]interface{})
				if !ok {
					return nil, fmt.Errorf("runbook action %q resumed without a durable action result", state.Current)
				}
				if last["status"] != "succeeded" {
					return r.failed(checkpoint, state, decisions, fmt.Sprintf("runbook action %s %v", state.Current, last["status"])), nil
				}
				if err := setRunbookPointer(checkpoint, step.Action.ResultPath, last["result"]); err != nil {
					return nil, stepError(state.Current, err)
				}
				delete(checkpoint, "lastAction")
				state.PendingAction = ""
				state.Current = step.Action.Next
				decisions = append(decisions, TurnDecision{Summary: "Applied governed result for " + state.Current})
				continue
			}
			arguments := map[string]interface{}{}
			for name, value := range step.Action.Arguments {
				resolved, resolveErr := resolveRunbookValue(checkpoint, value)
				if resolveErr != nil {
					return nil, stepError(state.Current, resolveErr)
				}
				arguments[name] = resolved
			}
			inputPointer := "/runbookActionInputs/" + escapeRunbookPointer(state.Current)
			if err := setRunbookPointer(checkpoint, inputPointer, arguments); err != nil {
				return nil, err
			}
			state.PendingAction = state.Current
			encodeRunbookState(checkpoint, state)
			return &TurnOutcome{Decisions: decisions, ProposedActions: []TurnAction{{Type: "skill_action", Capability: step.Action.SkillID + "." + step.Action.Action, Summary: "Execute " + step.Action.SkillID + "." + step.Action.Action, IdempotencyKey: "runbook:" + r.definition.ID + ":" + input.Run.ID + ":" + state.Current + ":" + strconv.FormatInt(input.Turn.Sequence, 10), InputRef: inputPointer}}, OutputSummary: "Requested governed runbook action " + state.Current, ContinuationCheckpoint: checkpoint, NextRunStatus: AgentRunStatusRunning}, nil
		case runbook.StepWait:
			if state.Waiting == state.Current {
				state.Waiting = ""
				state.Current = step.Wait.Next
				decisions = append(decisions, TurnDecision{Summary: "Resumed wait " + state.Current})
				continue
			}
			state.Waiting = state.Current
			encodeRunbookState(checkpoint, state)
			wake := &WakeCondition{}
			status := AgentRunStatusWaitingForEvent
			if step.Wait.Duration > 0 {
				at := r.now().UTC().Add(step.Wait.Duration)
				wake.Type = "timer"
				wake.WakeAt = &at
				wake.Reference = state.Current
				status = AgentRunStatusSleeping
			} else {
				wake.Type = "event"
				wake.Reference = step.Wait.Event
			}
			return &TurnOutcome{Decisions: decisions, OutputSummary: "Waiting at runbook step " + state.Current, ContinuationCheckpoint: checkpoint, NextRunStatus: status, WakeCondition: wake}, nil
		case runbook.StepForEach:
			if state.Loops == nil {
				state.Loops = map[string]runbookLoop{}
			}
			frame, exists := state.Loops[state.Current]
			if !exists {
				resolved, resolveErr := resolveRunbookValue(checkpoint, step.ForEach.Items)
				if resolveErr != nil {
					return nil, stepError(state.Current, resolveErr)
				}
				items, ok := resolved.([]interface{})
				if !ok {
					return nil, fmt.Errorf("runbook for_each %q items must resolve to an array", state.Current)
				}
				if len(items) > step.ForEach.MaxIterations {
					return nil, fmt.Errorf("runbook for_each %q exceeds maxIterations", state.Current)
				}
				frame = runbookLoop{Items: items}
			}
			if frame.Index >= len(frame.Items) {
				delete(state.Loops, state.Current)
				state.Current = step.ForEach.Next
				continue
			}
			state.Loops[state.Current] = frame
			if err := setRunbookPointer(checkpoint, "/loop/"+escapeRunbookPointer(step.ForEach.ItemName), frame.Items[frame.Index]); err != nil {
				return nil, err
			}
			state.Current = step.ForEach.Body
		case runbook.StepLoopReturn:
			loopStep := r.definition.Steps[step.LoopReturn.ForEach]
			frame, ok := state.Loops[step.LoopReturn.ForEach]
			if !ok {
				return nil, fmt.Errorf("runbook loop_return %q has no active loop", state.Current)
			}
			frame.Index++
			state.Loops[step.LoopReturn.ForEach] = frame
			state.Current = step.LoopReturn.ForEach
			_ = loopStep
		case runbook.StepFork:
			if state.PendingFork == state.Current {
				return r.proposeFork(checkpoint, state, decisions, input)
			}
			state.PendingFork = state.Current
			return r.proposeFork(checkpoint, state, decisions, input)
		case runbook.StepJoin:
			if state.BranchJoin == state.Current {
				encodeRunbookState(checkpoint, state)
				return &TurnOutcome{
					Decisions: decisions, OutputSummary: "Completed concurrent runbook branch " + state.BranchID,
					ContinuationCheckpoint: checkpoint, NextRunStatus: AgentRunStatusCompleted,
					RunOutput: map[string]interface{}{"branchId": state.BranchID, "forkId": state.BranchFork},
				}, nil
			}
			if state.PendingFork != "" {
				return nil, fmt.Errorf("runbook join %q resumed without durable fork results", state.Current)
			}
			return nil, fmt.Errorf("runbook join %q has no active concurrent fork", state.Current)
		case runbook.StepEnd:
			outputs := map[string]interface{}{}
			for name, value := range step.End.Outputs {
				resolved, resolveErr := resolveRunbookValue(checkpoint, value)
				if resolveErr != nil {
					return nil, stepError(state.Current, resolveErr)
				}
				outputs[name] = resolved
			}
			encodeRunbookState(checkpoint, state)
			return &TurnOutcome{Decisions: decisions, OutputSummary: "Completed runbook " + r.definition.Name, ContinuationCheckpoint: checkpoint, NextRunStatus: AgentRunStatusCompleted, RunOutput: outputs}, nil
		default:
			return nil, fmt.Errorf("unsupported runbook step kind %q", step.Kind)
		}
	}
	return nil, fmt.Errorf("runbook exceeded %d internal steps in one Turn", maximumRunbookStepsPerTurn)
}

func (r *RunbookTurnRunner) proposeFork(checkpoint map[string]interface{}, state runbookExecutionState, decisions []TurnDecision, input TurnExecutionContext) (*TurnOutcome, error) {
	step := r.definition.Steps[state.Current]
	if step.Fork == nil {
		return nil, fmt.Errorf("runbook fork %q is missing its definition", state.Current)
	}
	join, ok := r.definition.Steps[step.Fork.Join]
	if !ok || join.Join == nil || join.Join.Fork != state.Current {
		return nil, fmt.Errorf("runbook fork %q has no matching join", state.Current)
	}
	mode := FanInModeAll
	failure := DependencyFailureFailFast
	if join.Join.Mode == runbook.JoinAny {
		mode = FanInModeAny
		failure = DependencyFailureWait
	}
	names := make([]string, 0, len(step.Fork.Branches))
	for name := range step.Fork.Branches {
		names = append(names, name)
	}
	sort.Strings(names)
	branches := make([]RunForkBranch, 0, len(names))
	groupID := stableForkIdentifier(input.Run.ID, state.Current, "group")
	for _, name := range names {
		branchCheckpoint := cloneMap(checkpoint)
		delete(branchCheckpoint, "lastAction")
		delete(branchCheckpoint, "lastFork")
		childState := state
		childState.Current = step.Fork.Branches[name]
		childState.PendingFork = ""
		childState.BranchFork = state.Current
		childState.BranchID = name
		childState.BranchJoin = step.Fork.Join
		encodeRunbookState(branchCheckpoint, childState)
		branchCheckpoint["forkChild"] = map[string]interface{}{
			"sourceRunId": input.Run.ID, "groupId": groupID, "dependencyId": name,
			"forkId": state.Current, "joinId": step.Fork.Join,
		}
		branches = append(branches, RunForkBranch{
			ID: name, Goal: fmt.Sprintf("Execute branch %s of runbook fork %s", name, state.Current), Checkpoint: branchCheckpoint,
		})
	}
	encodeRunbookState(checkpoint, state)
	decisions = append(decisions, TurnDecision{Summary: fmt.Sprintf("Forked %d concurrent runbook branches at %s", len(branches), state.Current)})
	return &TurnOutcome{
		Decisions: decisions, ProposedFork: &TurnForkProposal{
			ForkID: state.Current, Policy: RunDependencyPolicy{Mode: mode, FailureMode: failure}, Branches: branches,
		},
		OutputSummary:          "Requested durable concurrent runbook fork " + state.Current,
		ContinuationCheckpoint: checkpoint, NextRunStatus: AgentRunStatusRunning,
	}, nil
}

func (r *RunbookTurnRunner) failed(checkpoint map[string]interface{}, state runbookExecutionState, decisions []TurnDecision, message string) *TurnOutcome {
	encodeRunbookState(checkpoint, state)
	return &TurnOutcome{Decisions: decisions, OutputSummary: message, ContinuationCheckpoint: checkpoint, NextRunStatus: AgentRunStatusFailed, RunError: message}
}
func stepError(id string, err error) error { return fmt.Errorf("runbook step %s: %w", id, err) }

func initializeRunbookInput(checkpoint map[string]interface{}, run *AgentRun) {
	if _, ok := checkpoint["input"]; ok {
		return
	}
	input := cloneMap(run.Context)
	if input == nil {
		input = map[string]interface{}{}
	}
	if trigger, ok := checkpoint["trigger"].(map[string]interface{}); ok {
		if payload, ok := trigger["payload"].(map[string]interface{}); ok {
			for key, value := range payload {
				input[key] = value
			}
		}
	}
	checkpoint["input"] = input
}
func decodeRunbookState(checkpoint map[string]interface{}) (runbookExecutionState, error) {
	var state runbookExecutionState
	if raw, ok := checkpoint["runbook"].(map[string]interface{}); ok {
		encoded, _ := json.Marshal(raw)
		if err := json.Unmarshal(encoded, &state); err != nil {
			return state, fmt.Errorf("decode runbook checkpoint: %w", err)
		}
	}
	return state, nil
}
func encodeRunbookState(checkpoint map[string]interface{}, state runbookExecutionState) {
	encoded, _ := json.Marshal(state)
	var raw map[string]interface{}
	_ = json.Unmarshal(encoded, &raw)
	checkpoint["runbook"] = raw
}

func resolveRunbookValue(root map[string]interface{}, value runbook.Value) (interface{}, error) {
	if value.Ref != "" {
		return getRunbookPointer(root, value.Ref)
	}
	var result interface{}
	if err := json.Unmarshal(value.Literal, &result); err != nil {
		return nil, err
	}
	return result, nil
}
func getRunbookPointer(root map[string]interface{}, pointer string) (interface{}, error) {
	var current interface{} = root
	for _, encoded := range strings.Split(strings.TrimPrefix(pointer, "/"), "/") {
		segment := strings.ReplaceAll(strings.ReplaceAll(encoded, "~1", "/"), "~0", "~")
		switch typed := current.(type) {
		case map[string]interface{}:
			var ok bool
			current, ok = typed[segment]
			if !ok {
				return nil, fmt.Errorf("JSON Pointer %q does not exist", pointer)
			}
		case []interface{}:
			index, err := strconv.Atoi(segment)
			if err != nil || index < 0 || index >= len(typed) {
				return nil, fmt.Errorf("JSON Pointer %q has invalid array index", pointer)
			}
			current = typed[index]
		default:
			return nil, fmt.Errorf("JSON Pointer %q traverses a scalar", pointer)
		}
	}
	return current, nil
}
func setRunbookPointer(root map[string]interface{}, pointer string, value interface{}) error {
	segments := strings.Split(strings.TrimPrefix(pointer, "/"), "/")
	if pointer == "" || !strings.HasPrefix(pointer, "/") || len(segments) == 0 {
		return fmt.Errorf("invalid JSON Pointer %q", pointer)
	}
	current := root
	for _, encoded := range segments[:len(segments)-1] {
		segment := strings.ReplaceAll(strings.ReplaceAll(encoded, "~1", "/"), "~0", "~")
		child, ok := current[segment].(map[string]interface{})
		if !ok {
			child = map[string]interface{}{}
			current[segment] = child
		}
		current = child
	}
	last := strings.ReplaceAll(strings.ReplaceAll(segments[len(segments)-1], "~1", "/"), "~0", "~")
	current[last] = value
	return nil
}
func escapeRunbookPointer(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, "~", "~0"), "/", "~1")
}

func evaluateRunbookPredicate(root map[string]interface{}, predicate runbook.Predicate) (bool, error) {
	switch predicate.Operator {
	case runbook.PredicateAll:
		for _, child := range predicate.Operands {
			ok, err := evaluateRunbookPredicate(root, child)
			if err != nil || !ok {
				return ok, err
			}
		}
		return true, nil
	case runbook.PredicateAny:
		for _, child := range predicate.Operands {
			ok, err := evaluateRunbookPredicate(root, child)
			if err != nil {
				return false, err
			}
			if ok {
				return true, nil
			}
		}
		return false, nil
	case runbook.PredicateNot:
		ok, err := evaluateRunbookPredicate(root, predicate.Operands[0])
		return !ok, err
	}
	left, err := resolveRunbookValue(root, *predicate.Left)
	if err != nil {
		return false, err
	}
	if predicate.Operator == runbook.PredicateExists {
		return left != nil, nil
	}
	if predicate.Operator == runbook.PredicateTruthy {
		return truthy(left), nil
	}
	right, err := resolveRunbookValue(root, *predicate.Right)
	if err != nil {
		return false, err
	}
	switch predicate.Operator {
	case runbook.PredicateEqual:
		return reflect.DeepEqual(left, right), nil
	case runbook.PredicateNotEqual:
		return !reflect.DeepEqual(left, right), nil
	case runbook.PredicateContains:
		return strings.Contains(fmt.Sprint(left), fmt.Sprint(right)), nil
	}
	l, okL := numeric(left)
	rr, okR := numeric(right)
	if !okL || !okR {
		return false, errors.New("ordered predicates require numeric operands")
	}
	switch predicate.Operator {
	case runbook.PredicateGreater:
		return l > rr, nil
	case runbook.PredicateAtLeast:
		return l >= rr, nil
	case runbook.PredicateLess:
		return l < rr, nil
	case runbook.PredicateAtMost:
		return l <= rr, nil
	}
	return false, fmt.Errorf("unsupported predicate %q", predicate.Operator)
}
func truthy(value interface{}) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case bool:
		return typed
	case string:
		return typed != ""
	case float64:
		return typed != 0
	case []interface{}:
		return len(typed) > 0
	case map[string]interface{}:
		return len(typed) > 0
	default:
		return true
	}
}
func numeric(value interface{}) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case json.Number:
		value, err := typed.Float64()
		return value, err == nil
	}
	return 0, false
}
