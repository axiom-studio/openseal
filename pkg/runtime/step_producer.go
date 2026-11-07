package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// StepProducer produces one step of a Run. The model loop implements it over
// a StepHost; deterministic runners (runbook, outreach, capability
// invocation) implement it directly and return tool calls for the same
// inline executor, or a final result.
type StepProducer interface {
	Kind() StepProducerKind
	Produce(ctx context.Context, in StepInput) (StepOutput, error)
}

// StepBudgetPlanner is implemented by producers that know what a step will
// cost before running it (the model producer). The loop adds the binding's
// reservation and sets Turns to 1. Deterministic producers do not implement
// it: their reservation is the floor with Turns 1.
type StepBudgetPlanner interface {
	PlanStepBudget(ctx context.Context, in StepInput) (BudgetUsage, error)
}

type StepMode string

const (
	StepModeNormal         StepMode = "normal"
	StepModeExplainFailure StepMode = "explain_failure"
)

func (m StepMode) Valid() bool {
	return m == StepModeNormal || m == StepModeExplainFailure
}

// StepInput is what a producer, preparer or compactor sees for one step.
// Run and Step are clones; Step is running.
type StepInput struct {
	Run      *AgentRun
	Step     *AgentTurn
	Session  *AgentSession     // nil for producers that run no tool calls (routing)
	Path     *AgentSessionPath // the session path loaded for this step
	Binding  *TurnRunnerBinding
	Mode     StepMode
	Attempt  int // 0-based; 1 after a forced compaction retry
	WorkerID string
}

// StepOutput is one produced step. A model producer returns the assistant
// entry; the loop derives the tool calls from it. A deterministic producer
// returns exactly one of ToolCalls, Final, Park and Failure.
type StepOutput struct {
	ModelProvider   string
	Model           string
	Usage           TurnUsage
	SkillSelections []HostedSkillSelection
	// Model producers only. Limits are kernel-filled from the step result.
	Assistant *AssistantEntry
	// Deterministic producers only: exactly one of the four.
	ToolCalls []LoopToolCall
	Final     *LoopFinal
	Park      *LoopPark // Committed must be false
	Failure   *LoopFailure
	// KernelState replaces Run.Checkpoint when non-nil (deterministic engine
	// state; never model input).
	KernelState map[string]interface{}
}

// LoopToolCall is one tool call a deterministic producer asks the inline
// executor to run.
type LoopToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
	// Directive is kernel-authored and only set by deterministic producers
	// (runbook pinned versions, idempotency, review context). It is stored on
	// the tool entry for resume; model tool calls never carry it.
	Directive *ToolCallDirective `json:"directive,omitempty"`
}

type ToolCallDirective struct {
	BindingID            string                     `json:"bindingId,omitempty"`
	BindingRevision      int64                      `json:"bindingRevision,omitempty"`
	ExpectedSkillVersion string                     `json:"expectedSkillVersion,omitempty"`
	IdempotencyKey       string                     `json:"idempotencyKey,omitempty"`
	Summary              string                     `json:"summary,omitempty"`
	ExternalOperation    *ExternalOperationIdentity `json:"externalOperation,omitempty"`
	ReviewContext        *ApprovalReviewContext     `json:"reviewContext,omitempty"`
}

// LoopToolResult is the terminal result of one tool entry as a deterministic
// producer reads it from the Run's session.
type LoopToolResult struct {
	ToolCallID   string
	Name         string
	State        ToolEntryState // completed | error | interrupted
	Content      []ContentPart
	ActionCallID string
	Output       map[string]interface{} // governed action output (deterministic producers)
}

// LoopFinal ends the Run without (or after) the final-answer chain: a
// deterministic producer's result or a preparer's EarlyFinal.
type LoopFinal struct {
	Answer string                 // user-visible text ("" only when Output.silent or a structured final)
	Output map[string]interface{} // Run output
	Status AgentRunStatus         // completed (default) | failed
}

// LoopPark parks the Run at step end.
type LoopPark struct {
	Status  AgentRunStatus // waiting_for_approval|waiting_for_dependency|waiting_for_agent|waiting_for_event|sleeping
	Wake    *WakeCondition // required (TransitionRun rule)
	Summary string
	// Committed is set only by the fork / run_operation / delegate / task
	// coordinators, which moved the Run in their own transaction.
	Committed bool
	WakePool  bool // children were created; the worker wakes the pool
}

// LoopFailure fails the Run. Kind includes
// execution|tool|budget|repeat_limit|context_overflow.
type LoopFailure struct {
	Kind        string
	Code        string
	Message     string
	Explainable bool
}

// ErrInvalidStepOutput wraps every validateStepOutput failure; the loop fails
// the step with producer_failed.
var ErrInvalidStepOutput = errors.New("invalid step output")

func invalidStepOutput(format string, args ...interface{}) error {
	return fmt.Errorf("%w: %s", ErrInvalidStepOutput, fmt.Sprintf(format, args...))
}

// validateStepOutput checks a producer's output against its kind.
func validateStepOutput(kind StepProducerKind, out StepOutput) error {
	if err := out.Usage.Validate(); err != nil {
		return invalidStepOutput("%v", err)
	}
	switch kind {
	case StepProducerModel:
		if out.Assistant == nil {
			return invalidStepOutput("model producer returned no assistant entry")
		}
		if len(out.ToolCalls) > 0 || out.Final != nil || out.Park != nil || out.KernelState != nil {
			return invalidStepOutput("model producer returned deterministic output")
		}
		if err := checkProducedAssistant(out.Assistant); err != nil {
			return invalidStepOutput("%v", err)
		}
	case StepProducerDeterministic:
		if out.Assistant != nil {
			return invalidStepOutput("deterministic producer returned an assistant entry")
		}
		set := 0
		if len(out.ToolCalls) > 0 {
			set++
		}
		if out.Final != nil {
			set++
		}
		if out.Park != nil {
			set++
		}
		if out.Failure != nil {
			set++
		}
		if set != 1 {
			return invalidStepOutput("deterministic producer must return exactly one of tool calls, final, park and failure (got %d)", set)
		}
		if err := validateLoopToolCalls(out.ToolCalls); err != nil {
			return err
		}
		if out.Final != nil {
			if err := out.Final.validate(); err != nil {
				return err
			}
		}
		if out.Park != nil {
			if out.Park.Committed {
				return invalidStepOutput("a producer park cannot be committed")
			}
			if err := out.Park.validate(); err != nil {
				return err
			}
		}
	default:
		return invalidStepOutput("unknown producer kind %q", kind)
	}
	if out.Failure != nil {
		if err := out.Failure.validate(); err != nil {
			return err
		}
	}
	return nil
}

func validateLoopToolCalls(calls []LoopToolCall) error {
	seen := make(map[string]bool, len(calls))
	for i, call := range calls {
		if strings.TrimSpace(call.ID) == "" {
			return invalidStepOutput("tool call %d has no id", i)
		}
		if seen[call.ID] {
			return invalidStepOutput("duplicate tool call id %q", call.ID)
		}
		seen[call.ID] = true
		if strings.TrimSpace(call.Name) == "" {
			return invalidStepOutput("tool call %q has no name", call.ID)
		}
		if err := validateToolArguments("tool call "+call.ID+" arguments", call.Arguments); err != nil {
			return invalidStepOutput("%v", err)
		}
		if call.Directive != nil && call.Directive.BindingRevision < 0 {
			return invalidStepOutput("tool call %q directive has a negative binding revision", call.ID)
		}
	}
	return nil
}

func (f *LoopFinal) validate() error {
	switch f.Status {
	case "", AgentRunStatusCompleted, AgentRunStatusFailed:
		return nil
	}
	return invalidStepOutput("final status %q must be completed or failed", f.Status)
}

func (p *LoopPark) validate() error {
	switch p.Status {
	case AgentRunStatusWaitingForApproval, AgentRunStatusWaitingForDependency, AgentRunStatusWaitingForAgent,
		AgentRunStatusWaitingForEvent, AgentRunStatusSleeping:
	default:
		return invalidStepOutput("park status %q is not a wait status", p.Status)
	}
	if p.Wake == nil {
		return invalidStepOutput("park to %s has no wake condition", p.Status)
	}
	return nil
}

func (f *LoopFailure) validate() error {
	if strings.TrimSpace(f.Kind) == "" || strings.TrimSpace(f.Code) == "" || strings.TrimSpace(f.Message) == "" {
		return invalidStepOutput("failure needs kind, code and message")
	}
	return nil
}
