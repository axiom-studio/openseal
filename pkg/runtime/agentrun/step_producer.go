package agentrun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/activity"
)

// StepProducer produces one step of a Run. The model loop implements it over
// a StepHost; deterministic runners (runbook, capability
// invocation) implement it directly and return tool calls for the same
// inline executor, or a final result.
type StepProducer interface {
	Kind() models.StepProducerKind
	Produce(ctx context.Context, in StepInput) (StepOutput, error)
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
	Run      *models.AgentRun
	Step     *models.AgentTurn
	Session  *models.AgentSession     // nil for producers that run no tool calls (routing)
	Path     *models.AgentSessionPath // the session path loaded for this step
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
	Usage           models.TurnUsage
	SkillSelections []models.HostedSkillSelection
	// Model producers only. Limits are kernel-filled from the step result.
	Assistant *models.AssistantEntry
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
	BindingID            string                              `json:"bindingId,omitempty"`
	BindingRevision      int64                               `json:"bindingRevision,omitempty"`
	ExpectedSkillVersion string                              `json:"expectedSkillVersion,omitempty"`
	IdempotencyKey       string                              `json:"idempotencyKey,omitempty"`
	Summary              string                              `json:"summary,omitempty"`
	ExternalOperation    *activity.ExternalOperationIdentity `json:"externalOperation,omitempty"`
	ReviewContext        *activity.ApprovalReviewContext     `json:"reviewContext,omitempty"`
	EvidenceRefs         []string                            `json:"evidenceRefs,omitempty"`
}

// LoopFinal ends the Run without (or after) the final-answer chain: a
// deterministic producer's result or a preparer's EarlyFinal.
type LoopFinal struct {
	Answer string                 // user-visible text ("" only when Output.silent or a structured final)
	Output map[string]interface{} // Run output
	Status models.AgentRunStatus  // completed (default) | failed
}

// LoopPark parks the Run at step end.
type LoopPark struct {
	Status  models.AgentRunStatus // waiting_for_approval|waiting_for_dependency|waiting_for_agent|waiting_for_event|sleeping
	Wake    *models.WakeCondition // required (TransitionRun rule)
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

func InvalidStepOutputf(format string, args ...interface{}) error {
	return fmt.Errorf("%w: %s", ErrInvalidStepOutput, fmt.Sprintf(format, args...))
}

func (f *LoopFinal) Validate() error {
	switch f.Status {
	case "", models.AgentRunStatusCompleted, models.AgentRunStatusFailed:
		return nil
	}
	return InvalidStepOutputf("final status %q must be completed or failed", f.Status)
}

func (p *LoopPark) Validate() error {
	switch p.Status {
	case models.AgentRunStatusWaitingForApproval, models.AgentRunStatusWaitingForDependency, models.AgentRunStatusWaitingForAgent,
		models.AgentRunStatusWaitingForEvent, models.AgentRunStatusSleeping:
	default:
		return InvalidStepOutputf("park status %q is not a wait status", p.Status)
	}
	if p.Wake == nil {
		return InvalidStepOutputf("park to %s has no wake condition", p.Status)
	}
	return nil
}

func (f *LoopFailure) Validate() error {
	if strings.TrimSpace(f.Kind) == "" || strings.TrimSpace(f.Code) == "" || strings.TrimSpace(f.Message) == "" {
		return InvalidStepOutputf("failure needs kind, code and message")
	}
	return nil
}
