package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/collaboration"
	"github.com/axiom-studio/openseal/pkg/runtime/control"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/modelstep"
)

// The runbook StepProducer (D6, section 04 §12). A runbook Run has no model:
// each step walks the pinned definition from the engine state in
// Run.Checkpoint until a node needs the kernel, then returns that need as the
// step's output:
//
//   - action: one governed tool call pinned by a directive (binding, Skill
//     version, idempotency key); the inline executor runs it (approvals park
//     as for every governed call) and the next step reads the ActionCall;
//   - fork / delegate: one kernel control call (runbook_fork /
//     runbook_delegate). It settles after the step is applied, so the engine
//     state the children are built from is durable, and calls
//     ControlExecutor.Fork / Delegate with kernel origin; the tool entry waits
//     awaiting_child and the next step reads the joined result from Run.Output
//     exactly as before;
//   - timer or event wait: a Park;
//   - end / branch join: a Final with the runbook output; a failed node: a
//     Failure.
//
// Every output carries the engine state as KernelState, which the loop writes
// to Run.Checkpoint when it applies the step. The session the tool entries
// live in is the Run's own never-rendered run session (A3).

// maximumRunbookNodesPerStep bounds the nodes one step may walk without a
// kernel need (transform/decision/for_each cycles).
const maximumRunbookNodesPerStep = 256

type RunbookStepProducer struct {
	definition *runbook.Definition
	entrypoint string
	calls      ActionCallReader
	now        func() time.Time
}

func NewRunbookStepProducer(definition *runbook.Definition, entrypoint string, calls ActionCallReader, opts ...models.ServiceOption) (*RunbookStepProducer, error) {
	if diagnostics := runbook.Validate(definition); len(diagnostics) > 0 {
		return nil, fmt.Errorf("invalid runbook %s: %s", diagnostics[0].Path, diagnostics[0].Message)
	}
	if calls == nil {
		return nil, errors.New("runbook producer requires the action-call store")
	}
	entrypoint = strings.TrimSpace(entrypoint)
	if entrypoint == "" {
		entrypoint = "manual"
	}
	if _, ok := definition.Entrypoints[entrypoint]; !ok {
		return nil, fmt.Errorf("runbook entrypoint %q is not defined", entrypoint)
	}
	return &RunbookStepProducer{definition: definition, entrypoint: entrypoint, calls: calls, now: models.ClockFrom(opts)}, nil
}

func (p *RunbookStepProducer) Kind() StepProducerKind { return StepProducerDeterministic }

type runbookExecutionState struct {
	Current           string `json:"current"`
	PendingAction     string `json:"pendingAction,omitempty"`
	PendingFork       string `json:"pendingFork,omitempty"`
	PendingDelegation string `json:"pendingDelegation,omitempty"`
	// PendingCall is the tool call id of the pending action, fork or
	// delegation; its tool entry holds the outcome.
	PendingCall string                 `json:"pendingCall,omitempty"`
	BranchFork  string                 `json:"branchFork,omitempty"`
	BranchID    string                 `json:"branchId,omitempty"`
	BranchJoin  string                 `json:"branchJoin,omitempty"`
	Waiting     string                 `json:"waiting,omitempty"`
	Loops       map[string]runbookLoop `json:"loops,omitempty"`
}
type runbookLoop struct {
	Items []interface{} `json:"items"`
	Index int           `json:"index"`
}

// runbookStoreError marks a kernel store failure while a node is open. It
// aborts the step (the loop retries or fails it) instead of becoming a
// runbook failure recorded in the node trace.
type runbookStoreError struct{ err error }

func (e *runbookStoreError) Error() string { return e.err.Error() }
func (e *runbookStoreError) Unwrap() error { return e.err }

func (p *RunbookStepProducer) Produce(ctx context.Context, in agentrun.StepInput) (out agentrun.StepOutput, runErr error) {
	if p == nil || p.definition == nil || in.Run == nil || in.Step == nil {
		return agentrun.StepOutput{}, errors.New("runbook step requires a definition, Run, and step")
	}
	checkpoint := maps.Clone(in.Run.Checkpoint)
	if checkpoint == nil {
		checkpoint = map[string]interface{}{}
	}
	initializeRunbookRuntime(checkpoint, in.Run)
	initializeRunbookInput(checkpoint, in.Run)
	state, err := decodeRunbookState(checkpoint)
	if err != nil {
		return agentrun.StepOutput{}, err
	}
	if state.Current == "" {
		state.Current = p.definition.Entrypoints[p.entrypoint]
	}
	var activeTraceSequence int64
	defer func() {
		var storeErr *runbookStoreError
		if runErr == nil || activeTraceSequence == 0 || errors.As(runErr, &storeErr) {
			return
		}
		message := runErr.Error()
		if traceErr := updateRunbookStepTrace(checkpoint, activeTraceSequence, RunbookStepTraceFailed, "", "Runbook node failed", message, nil, p.now()); traceErr != nil {
			return
		}
		out = p.failed(checkpoint, state, message)
		runErr = nil
	}()
	beginStep := func(stepID string) (int64, error) {
		sequence, err := beginRunbookStepTrace(checkpoint, p.definition, stepID, in.Step.ID, p.now())
		if err == nil {
			activeTraceSequence = sequence
		}
		return sequence, err
	}
	finishStep := func(sequence int64, status RunbookStepTraceStatus, next, summary, runError string, metadata map[string]string) error {
		err := updateRunbookStepTrace(checkpoint, sequence, status, next, summary, runError, metadata, p.now())
		if err == nil && sequence == activeTraceSequence && (status == RunbookStepTraceSucceeded || status == RunbookStepTraceFailed) {
			activeTraceSequence = 0
		}
		return err
	}
	for count := 0; count < maximumRunbookNodesPerStep; count++ {
		step, ok := p.definition.Steps[state.Current]
		if !ok {
			return agentrun.StepOutput{}, fmt.Errorf("runbook step %q does not exist", state.Current)
		}
		switch step.Kind {
		case runbook.StepTransform:
			traceSequence, traceErr := beginStep(state.Current)
			if traceErr != nil {
				return agentrun.StepOutput{}, traceErr
			}
			for pointer, value := range step.Transform.Assignments {
				resolved, resolveErr := resolveRunbookValue(checkpoint, value)
				if resolveErr != nil {
					return agentrun.StepOutput{}, stepError(state.Current, resolveErr)
				}
				if setErr := setRunbookPointer(checkpoint, pointer, resolved); setErr != nil {
					return agentrun.StepOutput{}, stepError(state.Current, setErr)
				}
			}
			if err := finishStep(traceSequence, RunbookStepTraceSucceeded, step.Transform.Next, "Applied deterministic transform", "", nil); err != nil {
				return agentrun.StepOutput{}, err
			}
			state.Current = step.Transform.Next
		case runbook.StepDecision:
			traceSequence, traceErr := beginStep(state.Current)
			if traceErr != nil {
				return agentrun.StepOutput{}, traceErr
			}
			next := step.Decision.Default
			for _, candidate := range step.Decision.Cases {
				matched, matchErr := evaluateRunbookPredicate(checkpoint, candidate.When)
				if matchErr != nil {
					return agentrun.StepOutput{}, stepError(state.Current, matchErr)
				}
				if matched {
					next = candidate.Next
					break
				}
			}
			if next == "" {
				return agentrun.StepOutput{}, fmt.Errorf("runbook decision %q matched no case and has no default", state.Current)
			}
			if err := finishStep(traceSequence, RunbookStepTraceSucceeded, next, "Selected decision branch", "", nil); err != nil {
				return agentrun.StepOutput{}, err
			}
			state.Current = next
		case runbook.StepAction:
			if state.PendingAction == state.Current {
				traceSequence, traceErr := pendingRunbookStepTrace(checkpoint, state.Current)
				if traceErr != nil {
					return agentrun.StepOutput{}, traceErr
				}
				activeTraceSequence = traceSequence
				result, call, resultErr := modelstep.ProducerToolResult(ctx, p.calls, in.Run, in.Path, state.PendingCall)
				if resultErr != nil {
					return agentrun.StepOutput{}, &runbookStoreError{resultErr}
				}
				if result == nil {
					return agentrun.StepOutput{}, fmt.Errorf("runbook action %q resumed without its tool entry", state.Current)
				}
				if call == nil {
					message := fmt.Sprintf("runbook action %s was refused before it ran", state.Current)
					if text := runbookToolResultText(result); text != "" {
						message += ": " + text
					}
					if err := finishStep(traceSequence, RunbookStepTraceFailed, "", "Governed action was refused", message, nil); err != nil {
						return agentrun.StepOutput{}, err
					}
					return p.failed(checkpoint, state, message), nil
				}
				metadata := runbookActionTraceMetadata(call)
				// The ActionCall is authoritative, whatever state its tool
				// entry ended in.
				if call.Status != ActionCallStatusSucceeded {
					message := fmt.Sprintf("runbook action %s %s", state.Current, call.Status)
					if err := finishStep(traceSequence, RunbookStepTraceFailed, "", "Governed action did not succeed", message, metadata); err != nil {
						return agentrun.StepOutput{}, err
					}
					return p.failed(checkpoint, state, message), nil
				}
				if err := setRunbookPointer(checkpoint, string(step.Action.ResultPath), runbookActionResult(result.Output)); err != nil {
					return agentrun.StepOutput{}, stepError(state.Current, err)
				}
				if err := finishStep(traceSequence, RunbookStepTraceSucceeded, step.Action.Next, "Governed action completed", "", metadata); err != nil {
					return agentrun.StepOutput{}, err
				}
				state.PendingAction, state.PendingCall = "", ""
				state.Current = step.Action.Next
				continue
			}
			traceSequence, traceErr := beginStep(state.Current)
			if traceErr != nil {
				return agentrun.StepOutput{}, traceErr
			}
			arguments := map[string]interface{}{}
			for name, value := range step.Action.Arguments {
				resolved, resolveErr := resolveRunbookValue(checkpoint, value)
				if resolveErr != nil {
					return agentrun.StepOutput{}, stepError(state.Current, resolveErr)
				}
				arguments[name] = resolved
			}
			selected, selectErr := runbookGovernedAction(in.Binding, step.Action)
			if selectErr != nil {
				return agentrun.StepOutput{}, stepError(state.Current, selectErr)
			}
			callID := runbookToolCallID(state.Current, traceSequence)
			call, callErr := modelstep.ProducerGovernedToolCall(callID, selected, arguments, &agentrun.ToolCallDirective{
				IdempotencyKey: "runbook:" + p.definition.ID + ":" + in.Run.ID + ":" + state.Current + ":" + strconv.FormatInt(in.Step.Sequence, 10),
				Summary:        "Execute " + step.Action.SkillID + "." + step.Action.Action,
			})
			if callErr != nil {
				return agentrun.StepOutput{}, stepError(state.Current, callErr)
			}
			state.PendingAction, state.PendingCall = state.Current, callID
			if err := finishStep(traceSequence, RunbookStepTraceWaiting, "", "Waiting for governed action", "", nil); err != nil {
				return agentrun.StepOutput{}, err
			}
			encodeRunbookState(checkpoint, state)
			return agentrun.StepOutput{ToolCalls: []agentrun.LoopToolCall{call}, KernelState: checkpoint}, nil
		case runbook.StepDelegate:
			if state.PendingDelegation == state.Current {
				traceSequence, traceErr := pendingRunbookStepTrace(checkpoint, state.Current)
				if traceErr != nil {
					return agentrun.StepOutput{}, traceErr
				}
				activeTraceSequence = traceSequence
				consumed, failure, consumeErr := p.consumeDelegationResult(checkpoint, &state, in.Run)
				if consumeErr != nil {
					return agentrun.StepOutput{}, consumeErr
				}
				if failure != "" {
					if err := finishStep(traceSequence, RunbookStepTraceFailed, "", "Delegated Agent work failed", failure, nil); err != nil {
						return agentrun.StepOutput{}, err
					}
					return p.failed(checkpoint, state, failure), nil
				}
				if consumed {
					if err := finishStep(traceSequence, RunbookStepTraceSucceeded, state.Current, "Delegated Agent work completed", "", nil); err != nil {
						return agentrun.StepOutput{}, err
					}
					continue
				}
				return p.unresolvedKernelCall(ctx, in, checkpoint, state, ControlToolRunbookDelegate, traceSequence, "delegated Agent step", finishStep)
			}
			traceSequence, traceErr := beginStep(state.Current)
			if traceErr != nil {
				return agentrun.StepOutput{}, traceErr
			}
			// Resolve now, so an unresolvable delegation fails this node; the
			// settle rebuilds the same spec from the applied engine state.
			if _, err := p.delegationSpec(checkpoint, state.Current); err != nil {
				return agentrun.StepOutput{}, err
			}
			state.PendingDelegation = state.Current
			state.PendingCall = runbookToolCallID(state.Current, traceSequence)
			if err := finishStep(traceSequence, RunbookStepTraceWaiting, "", "Waiting for delegated Agent work", "", nil); err != nil {
				return agentrun.StepOutput{}, err
			}
			return p.kernelCall(checkpoint, state, ControlToolRunbookDelegate, traceSequence)
		case runbook.StepWait:
			if state.Waiting == state.Current {
				traceSequence, traceErr := pendingRunbookStepTrace(checkpoint, state.Current)
				if traceErr != nil {
					return agentrun.StepOutput{}, traceErr
				}
				if err := finishStep(traceSequence, RunbookStepTraceSucceeded, step.Wait.Next, "Wait condition resolved", "", nil); err != nil {
					return agentrun.StepOutput{}, err
				}
				state.Waiting = ""
				state.Current = step.Wait.Next
				continue
			}
			traceSequence, traceErr := beginStep(state.Current)
			if traceErr != nil {
				return agentrun.StepOutput{}, traceErr
			}
			state.Waiting = state.Current
			if err := finishStep(traceSequence, RunbookStepTraceWaiting, "", "Waiting for condition", "", nil); err != nil {
				return agentrun.StepOutput{}, err
			}
			encodeRunbookState(checkpoint, state)
			park := &agentrun.LoopPark{Status: AgentRunStatusWaitingForEvent, Wake: &WakeCondition{Type: "event", Reference: step.Wait.Event}}
			if step.Wait.Duration > 0 {
				at := p.now().UTC().Add(step.Wait.Duration)
				park = &agentrun.LoopPark{Status: AgentRunStatusSleeping, Wake: &WakeCondition{Type: "timer", WakeAt: &at, Reference: state.Current}}
			}
			park.Summary = "Waiting at runbook step " + state.Current
			return agentrun.StepOutput{Park: park, KernelState: checkpoint}, nil
		case runbook.StepForEach:
			traceSequence, traceErr := beginStep(state.Current)
			if traceErr != nil {
				return agentrun.StepOutput{}, traceErr
			}
			if state.Loops == nil {
				state.Loops = map[string]runbookLoop{}
			}
			frame, exists := state.Loops[state.Current]
			if !exists {
				resolved, resolveErr := resolveRunbookValue(checkpoint, step.ForEach.Items)
				if resolveErr != nil {
					return agentrun.StepOutput{}, stepError(state.Current, resolveErr)
				}
				items, ok := resolved.([]interface{})
				if !ok {
					return agentrun.StepOutput{}, fmt.Errorf("runbook for_each %q items must resolve to an array", state.Current)
				}
				if len(items) > step.ForEach.MaxIterations {
					return agentrun.StepOutput{}, fmt.Errorf("runbook for_each %q exceeds maxIterations", state.Current)
				}
				frame = runbookLoop{Items: items}
			}
			if frame.Index >= len(frame.Items) {
				delete(state.Loops, state.Current)
				if err := finishStep(traceSequence, RunbookStepTraceSucceeded, step.ForEach.Next, "Iteration completed", "", nil); err != nil {
					return agentrun.StepOutput{}, err
				}
				state.Current = step.ForEach.Next
				continue
			}
			state.Loops[state.Current] = frame
			if err := setRunbookPointer(checkpoint, "/loop/"+escapeRunbookPointer(step.ForEach.ItemName), frame.Items[frame.Index]); err != nil {
				return agentrun.StepOutput{}, err
			}
			if err := finishStep(traceSequence, RunbookStepTraceSucceeded, step.ForEach.Body, fmt.Sprintf("Selected iteration %d", frame.Index+1), "", nil); err != nil {
				return agentrun.StepOutput{}, err
			}
			state.Current = step.ForEach.Body
		case runbook.StepLoopReturn:
			traceSequence, traceErr := beginStep(state.Current)
			if traceErr != nil {
				return agentrun.StepOutput{}, traceErr
			}
			frame, ok := state.Loops[step.LoopReturn.ForEach]
			if !ok {
				return agentrun.StepOutput{}, fmt.Errorf("runbook loop_return %q has no active loop", state.Current)
			}
			frame.Index++
			state.Loops[step.LoopReturn.ForEach] = frame
			if err := finishStep(traceSequence, RunbookStepTraceSucceeded, step.LoopReturn.ForEach, "Returned to iteration", "", nil); err != nil {
				return agentrun.StepOutput{}, err
			}
			state.Current = step.LoopReturn.ForEach
		case runbook.StepFork:
			if state.PendingFork == state.Current {
				traceSequence, traceErr := pendingRunbookStepTrace(checkpoint, state.Current)
				if traceErr != nil {
					return agentrun.StepOutput{}, traceErr
				}
				activeTraceSequence = traceSequence
				consumed, failure, consumeErr := p.consumeForkResult(checkpoint, &state, in.Run)
				if consumeErr != nil {
					return agentrun.StepOutput{}, consumeErr
				}
				if failure != "" {
					if err := finishStep(traceSequence, RunbookStepTraceFailed, "", "Concurrent branches failed", failure, nil); err != nil {
						return agentrun.StepOutput{}, err
					}
					return p.failed(checkpoint, state, failure), nil
				}
				if consumed {
					if err := finishStep(traceSequence, RunbookStepTraceSucceeded, state.Current, "Concurrent branches joined", "", nil); err != nil {
						return agentrun.StepOutput{}, err
					}
					continue
				}
				return p.unresolvedKernelCall(ctx, in, checkpoint, state, ControlToolRunbookFork, traceSequence, "concurrent runbook fork", finishStep)
			}
			traceSequence, traceErr := beginStep(state.Current)
			if traceErr != nil {
				return agentrun.StepOutput{}, traceErr
			}
			state.PendingFork = state.Current
			state.PendingCall = runbookToolCallID(state.Current, traceSequence)
			if _, err := p.forkSpec(checkpoint, state, in.Run.ID); err != nil {
				return agentrun.StepOutput{}, err
			}
			if err := finishStep(traceSequence, RunbookStepTraceWaiting, "", "Waiting for concurrent branches", "", nil); err != nil {
				return agentrun.StepOutput{}, err
			}
			return p.kernelCall(checkpoint, state, ControlToolRunbookFork, traceSequence)
		case runbook.StepJoin:
			traceSequence, traceErr := beginStep(state.Current)
			if traceErr != nil {
				return agentrun.StepOutput{}, traceErr
			}
			if state.BranchJoin == state.Current {
				if err := finishStep(traceSequence, RunbookStepTraceSucceeded, "", "Concurrent branch completed", "", nil); err != nil {
					return agentrun.StepOutput{}, err
				}
				encodeRunbookState(checkpoint, state)
				return agentrun.StepOutput{Final: &agentrun.LoopFinal{
					Answer: "Completed concurrent runbook branch " + state.BranchID,
					Output: map[string]interface{}{"branchId": state.BranchID, "forkId": state.BranchFork},
					Status: AgentRunStatusCompleted,
				}, KernelState: checkpoint}, nil
			}
			if state.PendingFork != "" {
				return agentrun.StepOutput{}, fmt.Errorf("runbook join %q resumed without durable fork results", state.Current)
			}
			return agentrun.StepOutput{}, fmt.Errorf("runbook join %q has no active concurrent fork", state.Current)
		case runbook.StepEnd:
			traceSequence, traceErr := beginStep(state.Current)
			if traceErr != nil {
				return agentrun.StepOutput{}, traceErr
			}
			outputs := map[string]interface{}{}
			for name, value := range step.End.Outputs {
				resolved, resolveErr := resolveRunbookValue(checkpoint, value)
				if resolveErr != nil {
					return agentrun.StepOutput{}, stepError(state.Current, resolveErr)
				}
				outputs[name] = resolved
			}
			if contract, ok := p.definition.Interfaces[p.entrypoint]; ok && len(contract.OutputSchema) > 0 {
				if err := runbook.ValidateInterfaceInput(contract.OutputSchema, outputs); err != nil {
					return agentrun.StepOutput{}, fmt.Errorf("runbook output does not satisfy interface %q: %w", p.entrypoint, err)
				}
			}
			if err := finishStep(traceSequence, RunbookStepTraceSucceeded, "", "Runbook completed", "", nil); err != nil {
				return agentrun.StepOutput{}, err
			}
			outputObservations := make([]RunbookDataObservation, 0, len(outputs))
			for name, value := range outputs {
				outputObservations = append(outputObservations, observeRunbookValue("/output/"+escapeRunbookPointer(name), value))
			}
			sort.Slice(outputObservations, func(i, j int) bool { return outputObservations[i].Ref < outputObservations[j].Ref })
			if err := replaceRunbookStepTraceOutputs(checkpoint, traceSequence, outputObservations); err != nil {
				return agentrun.StepOutput{}, err
			}
			encodeRunbookState(checkpoint, state)
			return agentrun.StepOutput{Final: &agentrun.LoopFinal{
				Answer: "Completed runbook " + p.definition.Name, Output: outputs, Status: AgentRunStatusCompleted,
			}, KernelState: checkpoint}, nil
		default:
			return agentrun.StepOutput{}, fmt.Errorf("unsupported runbook step kind %q", step.Kind)
		}
	}
	return agentrun.StepOutput{}, fmt.Errorf("runbook exceeded %d internal nodes in one step", maximumRunbookNodesPerStep)
}

// runbookToolCallID is the tool call id of one node visit; the trace
// sequence makes every visit (for_each iterations included) distinct and is
// the same when a crashed step is produced again from the same state.
func runbookToolCallID(stepID string, traceSequence int64) string {
	return "runbook." + strconv.FormatInt(traceSequence, 10) + "." + stepID
}

// runbookGovernedAction is the binding's governed action an action node
// names: exactly one authorized action of that Skill version.
func runbookGovernedAction(binding *agentrun.TurnRunnerBinding, action *runbook.ActionStep) (*capability.ModelAction, error) {
	if binding == nil {
		return nil, errors.New("runbook action needs the Run's binding")
	}
	var selected *capability.ModelAction
	for index := range binding.ModelActions {
		candidate := &binding.ModelActions[index]
		if candidate.SkillID == action.SkillID && candidate.Version == action.SkillVersion && candidate.Action == action.Action {
			if selected != nil {
				return nil, fmt.Errorf("governed action %s@%s/%s matches multiple authorized bindings", action.SkillID, action.SkillVersion, action.Action)
			}
			selected = candidate
		}
	}
	if selected == nil {
		return nil, fmt.Errorf("governed action %s@%s/%s is not authorized for this Agent", action.SkillID, action.SkillVersion, action.Action)
	}
	return selected, nil
}

func runbookActionTraceMetadata(call *ActionCall) map[string]string {
	metadata := map[string]string{}
	if id := strings.TrimSpace(call.ID); id != "" {
		metadata["actionCallId"] = id
	}
	if id := strings.TrimSpace(call.ApprovalID); id != "" {
		metadata["approvalId"] = id
	}
	return metadata
}

// runbookActionResultLimit caps the action output an action node writes into
// the engine state. That state is written on every step and copied into every
// fork branch, so it never carries media or unbounded payloads.
const runbookActionResultLimit = 64 << 10

// runbookResultCompactLevel bounds the depth, entries and text of a
// compacted action result.
type runbookResultCompactLevel struct{ depth, entries, text int }

// runbookActionResult is the value an action node writes to its result path:
// the ActionCall output without its model media (screenshots and other
// base64 payloads), compacted to runbookActionResultLimit when it is larger.
func runbookActionResult(output map[string]interface{}) map[string]interface{} {
	result := maps.Clone(output)
	delete(result, "modelMedia")
	if len(result) == 0 {
		return map[string]interface{}{}
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return map[string]interface{}{
			"available": false, "reason": "action result is not JSON: " + err.Error(),
		}
	}
	if len(encoded) <= runbookActionResultLimit {
		// The round trip is a deep copy: the engine state shares nothing
		// with the stored ActionCall.
		var copied map[string]interface{}
		if json.Unmarshal(encoded, &copied) == nil {
			return copied
		}
	}
	original := len(encoded)
	for _, level := range []runbookResultCompactLevel{
		{8, 64, 2048}, {6, 16, 512}, {4, 4, 128},
	} {
		value := map[string]interface{}{
			"available": true, "truncated": true, "originalSizeBytes": original,
			"value": runbookCompactValue(result, level.depth, level.entries, level.text),
		}
		if candidate, err := json.Marshal(value); err == nil && len(candidate) <= runbookActionResultLimit {
			return value
		}
	}
	return map[string]interface{}{
		"available": false, "truncated": true, "originalSizeBytes": original,
		"reason": "action result is larger than the runbook state allows",
	}
}

// runbookCompactValue samples a value to bounded depth, entry count and text
// length, keeping the head and tail of long arrays.
func runbookCompactValue(value interface{}, depth, entries, text int) interface{} {
	if depth <= 0 {
		return map[string]interface{}{"_opensealTruncated": "maximum depth reached"}
	}
	switch typed := value.(type) {
	case map[string]interface{}:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		result := make(map[string]interface{}, min(len(keys), entries)+1)
		for _, key := range keys[:min(len(keys), entries)] {
			result[key] = runbookCompactValue(typed[key], depth-1, entries, text)
		}
		if len(keys) > entries {
			result["_opensealOmittedFields"] = len(keys) - entries
		}
		return result
	case []interface{}:
		if len(typed) <= entries {
			result := make([]interface{}, len(typed))
			for index := range typed {
				result[index] = runbookCompactValue(typed[index], depth-1, entries, text)
			}
			return result
		}
		head := (entries + 1) / 2
		tail := entries - head
		result := make([]interface{}, 0, entries+1)
		for index := 0; index < head; index++ {
			result = append(result, runbookCompactValue(typed[index], depth-1, entries, text))
		}
		result = append(result, map[string]interface{}{"_opensealOmittedItems": len(typed) - entries})
		for index := len(typed) - tail; index < len(typed); index++ {
			result = append(result, runbookCompactValue(typed[index], depth-1, entries, text))
		}
		return result
	case string:
		if len(typed) <= text {
			return typed
		}
		return typed[:text] + "…"
	default:
		return typed
	}
}

// runbookToolResultText is the text of a terminal tool result, for failure
// messages.
func runbookToolResultText(result *modelstep.LoopToolResult) string {
	if result == nil || len(result.Content) == 0 {
		return ""
	}
	return strings.TrimSpace(control.ResultText(&ToolResult{Content: result.Content}))
}

// kernelCall is the step output of a fork or delegation node: one kernel
// control call that settles after the step (and its engine state) is applied.
func (p *RunbookStepProducer) kernelCall(checkpoint map[string]interface{}, state runbookExecutionState, name ControlToolName, traceSequence int64) (agentrun.StepOutput, error) {
	stepID := state.Current
	arguments, err := json.Marshal(runbookKernelArgs{Step: stepID, Visit: traceSequence})
	if err != nil {
		return agentrun.StepOutput{}, err
	}
	summary := "Fork concurrent branches at " + stepID
	if name == ControlToolRunbookDelegate {
		summary = "Delegate runbook step " + stepID
	}
	encodeRunbookState(checkpoint, state)
	return agentrun.StepOutput{ToolCalls: []agentrun.LoopToolCall{{
		ID: state.PendingCall, Name: string(name), Arguments: arguments,
		Directive: &agentrun.ToolCallDirective{Summary: summary},
	}}, KernelState: checkpoint}, nil
}

// unresolvedKernelCall handles a fork or delegation node whose result is not
// in Run.Output. The loop resumes open tool entries before it produces a
// step, so a recorded entry is terminal: the call ended without a durable
// result (refused, lost) and the node fails with its result. An entry that
// was never recorded is asked for again (same id).
func (p *RunbookStepProducer) unresolvedKernelCall(ctx context.Context, in agentrun.StepInput, checkpoint map[string]interface{}, state runbookExecutionState, name ControlToolName, traceSequence int64, what string, finishStep func(int64, RunbookStepTraceStatus, string, string, string, map[string]string) error) (agentrun.StepOutput, error) {
	result, _, err := modelstep.ProducerToolResult(ctx, p.calls, in.Run, in.Path, state.PendingCall)
	if err != nil {
		return agentrun.StepOutput{}, &runbookStoreError{err}
	}
	if result == nil {
		return p.kernelCall(checkpoint, state, name, traceSequence)
	}
	message := what + " " + state.Current + " ended without a durable result"
	if text := runbookToolResultText(result); text != "" {
		message += ": " + text
	}
	if err := finishStep(traceSequence, RunbookStepTraceFailed, "", "Kernel call ended without a result", message, nil); err != nil {
		return agentrun.StepOutput{}, err
	}
	return p.failed(checkpoint, state, message), nil
}

// consumeForkResult reads the joined result of the pending fork from
// Run.Output. consumed is false while no resolved group is recorded; failure
// is the node's failure message when the group did not succeed.
func (p *RunbookStepProducer) consumeForkResult(checkpoint map[string]interface{}, state *runbookExecutionState, run *AgentRun) (bool, string, error) {
	if state == nil || run == nil || state.PendingFork == "" {
		return false, "", nil
	}
	groupID := agentrun.StableForkIdentifier(run.ID, state.PendingFork, "group")
	groups, _ := run.Output["dependencyGroups"].(map[string]interface{})
	group, _ := groups[groupID].(map[string]interface{})
	status, _ := group["status"].(string)
	if status == "" || status == string(RunDependencyGroupWaiting) {
		return false, "", nil
	}
	if status != string(RunDependencyGroupSatisfied) {
		return false, "concurrent runbook fork " + state.PendingFork + " failed", nil
	}
	dependencies, _ := group["dependencies"].(map[string]interface{})
	ids := make([]string, 0, len(dependencies))
	for id := range dependencies {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	forks, _ := checkpoint["forkResults"].(map[string]interface{})
	if forks == nil {
		forks = map[string]interface{}{}
		checkpoint["forkResults"] = forks
	}
	results := map[string]interface{}{}
	for _, id := range ids {
		dependency, _ := dependencies[id].(map[string]interface{})
		if dependency["state"] != string(RunDependencyStateSatisfied) {
			continue
		}
		result, _ := dependency["result"].(map[string]interface{})
		results[id] = maps.Clone(result)
		branchCheckpoint, _ := result["checkpoint"].(map[string]interface{})
		for _, key := range []string{"state", "steps"} {
			if branchValues, ok := branchCheckpoint[key].(map[string]interface{}); ok {
				if err := mergeRunbookBranchValues(checkpoint, key, branchValues); err != nil {
					return false, "", fmt.Errorf("fork %s branch %s: %w", state.PendingFork, id, err)
				}
			}
		}
	}
	forks[state.PendingFork] = results
	forkStep := p.definition.Steps[state.PendingFork]
	joinStep := p.definition.Steps[forkStep.Fork.Join]
	state.PendingFork, state.PendingCall = "", ""
	state.Current = joinStep.Join.Next
	encodeRunbookState(checkpoint, *state)
	return true, "", nil
}

func mergeRunbookBranchValues(checkpoint map[string]interface{}, key string, branch map[string]interface{}) error {
	target, _ := checkpoint[key].(map[string]interface{})
	if target == nil {
		target = map[string]interface{}{}
		checkpoint[key] = target
	}
	for name, value := range branch {
		if current, exists := target[name]; exists && !reflect.DeepEqual(current, value) {
			return fmt.Errorf("concurrent branches produced conflicting %s value %q", key, name)
		}
		target[name] = value
	}
	return nil
}

// forkSpec is the kernel ForkSpec of the fork node state.Current: one child
// Run per branch, each seeded with the engine state at its branch entry and
// the branch budget of the definition. The source keeps checkpoint while it
// waits.
func (p *RunbookStepProducer) forkSpec(checkpoint map[string]interface{}, state runbookExecutionState, runID string) (control.ForkSpec, error) {
	step := p.definition.Steps[state.Current]
	if step.Fork == nil {
		return control.ForkSpec{}, fmt.Errorf("runbook fork %q is missing its definition", state.Current)
	}
	join, ok := p.definition.Steps[step.Fork.Join]
	if !ok || join.Join == nil || join.Join.Fork != state.Current {
		return control.ForkSpec{}, fmt.Errorf("runbook fork %q has no matching join", state.Current)
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
	source := maps.Clone(checkpoint)
	encodeRunbookState(source, state)
	groupID := agentrun.StableForkIdentifier(runID, state.Current, "group")
	branches := make([]agentrun.RunForkBranch, 0, len(names))
	for _, name := range names {
		branchCheckpoint := maps.Clone(source)
		// Each child Run owns its own node trace. Parent history remains linked
		// through the durable dependency rather than copied into every branch.
		delete(branchCheckpoint, runbookTraceCheckpointKey)
		childState := state
		childState.Current = step.Fork.Branches[name]
		childState.PendingFork, childState.PendingCall = "", ""
		childState.BranchFork = state.Current
		childState.BranchID = name
		childState.BranchJoin = step.Fork.Join
		encodeRunbookState(branchCheckpoint, childState)
		branchCheckpoint["forkChild"] = map[string]interface{}{
			"sourceRunId": runID, "groupId": groupID, "dependencyId": name,
			"forkId": state.Current, "joinId": step.Fork.Join,
		}
		branches = append(branches, agentrun.RunForkBranch{
			ID: name, Goal: fmt.Sprintf("Execute branch %s of runbook fork %s", name, state.Current), Checkpoint: branchCheckpoint,
			Budget: runbookBudgetPolicy(step.Fork.BranchBudgets, name),
		})
	}
	return control.ForkSpec{
		ForkID: state.Current, Policy: RunDependencyPolicy{Mode: mode, FailureMode: failure},
		Branches: branches, SourceCheckpoint: source,
	}, nil
}

// delegationSpec is the kernel DelegationSpec of the delegation node stepID,
// resolved from checkpoint.
func (p *RunbookStepProducer) delegationSpec(checkpoint map[string]interface{}, stepID string) (control.DelegationSpec, error) {
	step := p.definition.Steps[stepID]
	if step.Delegate == nil {
		return control.DelegationSpec{}, fmt.Errorf("runbook delegation %q is missing its definition", stepID)
	}
	agentValue, err := resolveRunbookValue(checkpoint, step.Delegate.AgentID)
	if err != nil {
		return control.DelegationSpec{}, stepError(stepID, err)
	}
	agentID, ok := agentValue.(string)
	if !ok || strings.TrimSpace(agentID) == "" {
		return control.DelegationSpec{}, fmt.Errorf("runbook delegation %q Agent ID must resolve to a string", stepID)
	}
	goalValue, err := resolveRunbookValue(checkpoint, step.Delegate.Goal)
	if err != nil {
		return control.DelegationSpec{}, stepError(stepID, err)
	}
	goal, ok := goalValue.(string)
	if !ok || strings.TrimSpace(goal) == "" {
		return control.DelegationSpec{}, fmt.Errorf("runbook delegation %q goal must resolve to a string", stepID)
	}
	delegatedContext := make(map[string]interface{}, len(step.Delegate.Context))
	for key, value := range step.Delegate.Context {
		resolved, resolveErr := resolveRunbookValue(checkpoint, value)
		if resolveErr != nil {
			return control.DelegationSpec{}, stepError(stepID, resolveErr)
		}
		delegatedContext[key] = resolved
	}
	return control.DelegationSpec{
		StepID: stepID, AgentID: strings.TrimSpace(agentID), Goal: strings.TrimSpace(goal),
		Context: delegatedContext, Mode: step.Delegate.Mode, Timeout: step.Delegate.Timeout,
		Budget: runbookBudgetPolicyValue(step.Delegate.Budget),
	}, nil
}

func runbookBudgetPolicy(budgets map[string]runbook.BudgetAllocation, name string) *BudgetPolicy {
	budget, ok := budgets[name]
	if !ok {
		return nil
	}
	return runbookBudgetPolicyValue(&budget)
}

// consumeDelegationResult reads the pending delegation's outcome from
// Run.Output (collaboration result, or its dependency group). consumed is
// false while none is recorded; failure is the node's failure message.
func (p *RunbookStepProducer) consumeDelegationResult(checkpoint map[string]interface{}, state *runbookExecutionState, run *AgentRun) (bool, string, error) {
	if state == nil || run == nil || state.PendingDelegation == "" {
		return false, "", nil
	}
	stepID := state.PendingDelegation
	groupID := agentrun.StableForkIdentifier(run.ID, stepID, "group")
	groups, _ := run.Output["dependencyGroups"].(map[string]interface{})
	group, _ := groups[groupID].(map[string]interface{})
	status, _ := group["status"].(string)
	var output map[string]interface{}
	switch {
	case status == "":
		requestID := collaboration.StableCollaborationID(run.Scope, control.DelegationIdempotencyKey(run.ID, stepID), "request")
		collaborationResults, _ := run.Output["collaborationResults"].(map[string]interface{})
		result, _ := collaborationResults[requestID].(map[string]interface{})
		if len(result) == 0 {
			return false, "", nil
		}
		if resultStatus, _ := result["status"].(string); resultStatus != "" && resultStatus != string(AgentRequestStatusCompleted) {
			message := "delegated Agent step " + stepID + " " + resultStatus
			if reason, _ := result["reason"].(string); strings.TrimSpace(reason) != "" {
				reason = strings.TrimSpace(reason)
				if len(reason) > 512 {
					reason = reason[:512]
				}
				message += ": " + reason
			}
			return false, message, nil
		}
		output, _ = result["output"].(map[string]interface{})
	case status == string(RunDependencyGroupWaiting):
		return false, "", nil
	case status != string(RunDependencyGroupSatisfied):
		return false, "delegated Agent step " + stepID + " failed", nil
	default:
		dependencies, _ := group["dependencies"].(map[string]interface{})
		dependency, _ := dependencies["delegate"].(map[string]interface{})
		result, _ := dependency["result"].(map[string]interface{})
		output, _ = result["output"].(map[string]interface{})
	}
	step := p.definition.Steps[stepID]
	if err := setRunbookPointer(checkpoint, string(step.Delegate.ResultPath), maps.Clone(output)); err != nil {
		return false, "", stepError(stepID, err)
	}
	state.PendingDelegation, state.PendingCall = "", ""
	state.Current = step.Delegate.Next
	encodeRunbookState(checkpoint, *state)
	return true, "", nil
}

// failed fails the Run with message; the engine state (and its failed node
// trace) is written with the failure.
func (p *RunbookStepProducer) failed(checkpoint map[string]interface{}, state runbookExecutionState, message string) agentrun.StepOutput {
	encodeRunbookState(checkpoint, state)
	return agentrun.StepOutput{
		Failure:     &agentrun.LoopFailure{Kind: conversation.RunFailureKindExecution, Code: "execution_failed", Message: message},
		KernelState: checkpoint,
	}
}

// ---------------------------------------------------------------------------
// Kernel control calls of runbook Runs (section 04 §12)

// runbookKernelArgs are the arguments of a runbook_fork / runbook_delegate
// entry: the node and its trace visit. The spec is rebuilt at settle from the
// applied engine state, so branch checkpoints never travel in the entry.
type runbookKernelArgs struct {
	Step  string `json:"step"`
	Visit int64  `json:"visit"`
}

type runbookForkHandler struct{}

type runbookDelegateHandler struct{}

// runbookControlTools are the kernel-only control tools of runbook Runs.
func runbookControlTools() []control.Tool {
	return []control.Tool{
		control.NewTool(ControlToolRunbookFork, runbookForkHandler{}),
		control.NewTool(ControlToolRunbookDelegate, runbookDelegateHandler{}),
	}
}

func runbookKernelSpec(description string) control.ControlToolSpec {
	return control.ControlToolSpec{
		Description: description,
		InputSchema: map[string]interface{}{
			"type": "object", "additionalProperties": false, "required": []interface{}{"step", "visit"},
			"properties": map[string]interface{}{
				"step":  map[string]interface{}{"type": "string", "minLength": 1},
				"visit": map[string]interface{}{"type": "integer", "minimum": 1},
			},
		},
	}
}

func decodeRunbookKernelArgs(raw json.RawMessage) (interface{}, error) {
	value, err := models.DecodeControlInto[runbookKernelArgs](raw)
	if err != nil {
		return nil, err
	}
	args := value.(*runbookKernelArgs)
	if strings.TrimSpace(args.Step) == "" || args.Visit < 1 {
		return nil, errors.New("runbook kernel call needs its step and visit")
	}
	return args, nil
}

// runbookKernelCall resolves the producer and the engine state a kernel call
// settles from. The Run's checkpoint is the applied state of the producing
// step, so it is waiting at exactly this node.
func runbookKernelCall(cc agentrun.ControlContext, pending func(runbookExecutionState) string) (*RunbookStepProducer, map[string]interface{}, runbookExecutionState, error) {
	args := cc.Arguments.(*runbookKernelArgs)
	var state runbookExecutionState
	if cc.Origin != control.ControlOriginKernel {
		return nil, nil, state, errors.New("runbook kernel calls are made only by the runbook engine")
	}
	var producer *RunbookStepProducer
	if cc.Binding != nil {
		producer, _ = cc.Binding.Producer.(*RunbookStepProducer)
	}
	if producer == nil {
		return nil, nil, state, errors.New("the Run is not executing a runbook")
	}
	if cc.Run == nil || cc.Entry == nil {
		return nil, nil, state, errors.New("runbook kernel calls need their Run and tool entry")
	}
	checkpoint := maps.Clone(cc.Run.Checkpoint)
	state, err := decodeRunbookState(checkpoint)
	if err != nil {
		return nil, nil, state, err
	}
	if state.Current != args.Step || pending(state) != args.Step || state.PendingCall != cc.Entry.ToolCallID {
		return nil, nil, state, fmt.Errorf("the runbook is not waiting at %s for this call", args.Step)
	}
	return producer, checkpoint, state, nil
}

func (runbookForkHandler) Spec(agentrun.ControlAuthority) control.ControlToolSpec {
	return runbookKernelSpec("Kernel only: start the concurrent branches of a runbook fork.")
}

func (runbookForkHandler) Decode(raw json.RawMessage) (interface{}, error) {
	return decodeRunbookKernelArgs(raw)
}

func (runbookForkHandler) Run(ctx context.Context, x *control.ControlExecutor, cc agentrun.ControlContext) (control.ControlResult, error) {
	producer, checkpoint, state, err := runbookKernelCall(cc, func(s runbookExecutionState) string { return s.PendingFork })
	if err != nil {
		return x.RefuseIfEntry(ctx, cc, control.ErrorResult(ToolResultRefused, "runbook_call_invalid", err.Error(), nil))
	}
	spec, err := producer.forkSpec(checkpoint, state, cc.Run.ID)
	if err != nil {
		return x.RefuseIfEntry(ctx, cc, control.ErrorResult(ToolResultRefused, "runbook_call_invalid", err.Error(), nil))
	}
	return x.Fork(ctx, cc, spec)
}

func (runbookForkHandler) Resume(ctx context.Context, x *control.ControlExecutor, cc agentrun.ControlContext) (control.ControlResume, error) {
	return x.ResumeFork(ctx, cc)
}

func (runbookDelegateHandler) Spec(agentrun.ControlAuthority) control.ControlToolSpec {
	return runbookKernelSpec("Kernel only: hand a runbook delegation node to its Agent.")
}

func (runbookDelegateHandler) Decode(raw json.RawMessage) (interface{}, error) {
	return decodeRunbookKernelArgs(raw)
}

func (runbookDelegateHandler) Run(ctx context.Context, x *control.ControlExecutor, cc agentrun.ControlContext) (control.ControlResult, error) {
	producer, checkpoint, state, err := runbookKernelCall(cc, func(s runbookExecutionState) string { return s.PendingDelegation })
	if err != nil {
		return x.RefuseIfEntry(ctx, cc, control.ErrorResult(ToolResultRefused, "runbook_call_invalid", err.Error(), nil))
	}
	spec, err := producer.delegationSpec(checkpoint, state.Current)
	if err != nil {
		return x.RefuseIfEntry(ctx, cc, control.ErrorResult(ToolResultRefused, "runbook_call_invalid", err.Error(), nil))
	}
	return x.Delegate(ctx, cc, spec)
}

func (runbookDelegateHandler) Resume(ctx context.Context, x *control.ControlExecutor, cc agentrun.ControlContext) (control.ControlResume, error) {
	return x.ResumeDelegation(ctx, cc)
}

func stepError(id string, err error) error { return fmt.Errorf("runbook step %s: %w", id, err) }

func initializeRunbookRuntime(checkpoint map[string]interface{}, run *AgentRun) {
	rootRunID := run.RootRunID
	if rootRunID == "" {
		rootRunID = run.ID
	}
	checkpoint["runtime"] = map[string]interface{}{
		"runId":       run.ID,
		"rootRunId":   rootRunID,
		"objectiveId": run.ObjectiveID,
	}
}

func initializeRunbookInput(checkpoint map[string]interface{}, run *AgentRun) {
	if _, ok := checkpoint["input"]; ok {
		return
	}
	input := maps.Clone(run.Context)
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
		return getRunbookPointer(root, string(value.Ref))
	}
	if len(value.Template) > 0 {
		var builder strings.Builder
		for _, segment := range value.Template {
			if segment.Text != "" {
				builder.WriteString(segment.Text)
				continue
			}
			resolved, err := getRunbookPointer(root, string(segment.Ref))
			if err != nil {
				// Embedded templates preserve ordinary shell/UI interpolation
				// semantics: an absent optional value contributes an empty string.
				// Exact Ref values remain strict and fail above.
				continue
			}
			switch typed := resolved.(type) {
			case string:
				builder.WriteString(typed)
			case nil:
				builder.WriteString("null")
			default:
				encoded, encodeErr := json.Marshal(typed)
				if encodeErr != nil {
					return nil, encodeErr
				}
				builder.Write(encoded)
			}
		}
		return builder.String(), nil
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
