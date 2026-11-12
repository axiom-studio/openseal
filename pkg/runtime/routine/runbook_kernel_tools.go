package routine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/control"
)

// runbookKernelArgs are the arguments of a runbook_fork / runbook_delegate
// entry: the node and its trace visit. The spec is rebuilt at settle from the
// applied engine state, so branch checkpoints never travel in the entry.
type runbookKernelArgs struct {
	Step  string `json:"step"`
	Visit int64  `json:"visit"`
}

type runbookForkHandler struct{}

type runbookDelegateHandler struct{}

// RunbookControlTools are the kernel-only control tools of runbook Runs.
func RunbookControlTools() []control.Tool {
	return []control.Tool{
		control.NewTool(models.ControlToolRunbookFork, runbookForkHandler{}),
		control.NewTool(models.ControlToolRunbookDelegate, runbookDelegateHandler{}),
	}
}

func runbookKernelSpec(description string) control.ToolSpec {
	return control.ToolSpec{
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
	if cc.Origin != control.OriginKernel {
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

func (runbookForkHandler) Spec(agentrun.ControlAuthority) control.ToolSpec {
	return runbookKernelSpec("Kernel only: start the concurrent branches of a runbook fork.")
}

func (runbookForkHandler) Decode(raw json.RawMessage) (interface{}, error) {
	return decodeRunbookKernelArgs(raw)
}

func (runbookForkHandler) Run(ctx context.Context, x *control.Executor, cc agentrun.ControlContext) (control.Result, error) {
	producer, checkpoint, state, err := runbookKernelCall(cc, func(s runbookExecutionState) string { return s.PendingFork })
	if err != nil {
		return x.RefuseIfEntry(ctx, cc, control.ErrorResult(models.ToolResultRefused, "runbook_call_invalid", err.Error(), nil))
	}
	spec, err := producer.forkSpec(checkpoint, state, cc.Run.ID)
	if err != nil {
		return x.RefuseIfEntry(ctx, cc, control.ErrorResult(models.ToolResultRefused, "runbook_call_invalid", err.Error(), nil))
	}
	return x.Fork(ctx, cc, spec)
}

func (runbookForkHandler) Resume(ctx context.Context, x *control.Executor, cc agentrun.ControlContext) (control.Resume, error) {
	return x.ResumeFork(ctx, cc)
}

func (runbookDelegateHandler) Spec(agentrun.ControlAuthority) control.ToolSpec {
	return runbookKernelSpec("Kernel only: hand a runbook delegation node to its Agent.")
}

func (runbookDelegateHandler) Decode(raw json.RawMessage) (interface{}, error) {
	return decodeRunbookKernelArgs(raw)
}

func (runbookDelegateHandler) Run(ctx context.Context, x *control.Executor, cc agentrun.ControlContext) (control.Result, error) {
	producer, checkpoint, state, err := runbookKernelCall(cc, func(s runbookExecutionState) string { return s.PendingDelegation })
	if err != nil {
		return x.RefuseIfEntry(ctx, cc, control.ErrorResult(models.ToolResultRefused, "runbook_call_invalid", err.Error(), nil))
	}
	spec, err := producer.delegationSpec(checkpoint, state.Current)
	if err != nil {
		return x.RefuseIfEntry(ctx, cc, control.ErrorResult(models.ToolResultRefused, "runbook_call_invalid", err.Error(), nil))
	}
	return x.Delegate(ctx, cc, spec)
}

func (runbookDelegateHandler) Resume(ctx context.Context, x *control.Executor, cc agentrun.ControlContext) (control.Resume, error) {
	return x.ResumeDelegation(ctx, cc)
}
