// Package runbook defines OpenSeal's portable deterministic automation plan.
// It contains no product, UI-node, transport, or enterprise control-plane
// concepts; every side effect is an exact governed Skill action identity.
package runbook

import (
	"encoding/json"
	"errors"
	"time"
)

const APIVersion = "openseal.dev/runbook/v1alpha1"

type Definition struct {
	APIVersion  string            `json:"apiVersion"`
	ID          string            `json:"id"`
	Version     string            `json:"version"`
	Name        string            `json:"name"`
	Description string            `json:"description,omitempty"`
	Entrypoints map[string]string `json:"entrypoints"`
	Steps       map[string]Step   `json:"steps"`
}

type StepKind string

const (
	StepAction     StepKind = "action"
	StepDelegate   StepKind = "delegate"
	StepDecision   StepKind = "decision"
	StepTransform  StepKind = "transform"
	StepWait       StepKind = "wait"
	StepFork       StepKind = "fork"
	StepJoin       StepKind = "join"
	StepForEach    StepKind = "for_each"
	StepLoopReturn StepKind = "loop_return"
	StepEnd        StepKind = "end"
)

type Step struct {
	Kind       StepKind        `json:"kind"`
	Name       string          `json:"name,omitempty"`
	Action     *ActionStep     `json:"action,omitempty"`
	Delegate   *DelegateStep   `json:"delegate,omitempty"`
	Decision   *DecisionStep   `json:"decision,omitempty"`
	Transform  *TransformStep  `json:"transform,omitempty"`
	Wait       *WaitStep       `json:"wait,omitempty"`
	Fork       *ForkStep       `json:"fork,omitempty"`
	Join       *JoinStep       `json:"join,omitempty"`
	ForEach    *ForEachStep    `json:"forEach,omitempty"`
	LoopReturn *LoopReturnStep `json:"loopReturn,omitempty"`
	End        *EndStep        `json:"end,omitempty"`
}

type ActionStep struct {
	SkillID      string           `json:"skillId"`
	SkillVersion string           `json:"skillVersion"`
	Action       string           `json:"action"`
	Arguments    map[string]Value `json:"arguments,omitempty"`
	ResultPath   string           `json:"resultPath"`
	Next         string           `json:"next"`
}

// DelegateStep assigns a bounded child Run to another first-class Agent. The
// parent waits on durable child lineage and resumes with the child's terminal
// output; credentials and private memory are never copied implicitly.
type DelegateStep struct {
	AgentID    Value             `json:"agentId"`
	Goal       Value             `json:"goal"`
	Context    map[string]Value  `json:"context,omitempty"`
	Mode       DelegateMode      `json:"mode,omitempty"`
	ResultPath string            `json:"resultPath"`
	Timeout    time.Duration     `json:"timeout,omitempty"`
	Budget     *BudgetAllocation `json:"budget,omitempty"`
	Next       string            `json:"next"`
}

// BudgetAllocation is the explicit portable ceiling granted to one child Run.
// It deliberately mirrors the provider-neutral runtime dimensions while
// remaining independent of runtime orchestration packages.
type BudgetAllocation struct {
	MaxAttempts     int64 `json:"maxAttempts,omitempty"`
	MaxTurns        int64 `json:"maxTurns,omitempty"`
	MaxInputTokens  int64 `json:"maxInputTokens,omitempty"`
	MaxOutputTokens int64 `json:"maxOutputTokens,omitempty"`
	MaxTotalTokens  int64 `json:"maxTotalTokens,omitempty"`
	MaxCostMicros   int64 `json:"maxCostMicros,omitempty"`
	MaxDurationMS   int64 `json:"maxDurationMs,omitempty"`
	MaxActions      int64 `json:"maxActions,omitempty"`
	WarningPermille int64 `json:"warningPermille,omitempty"`
}

func (b BudgetAllocation) Validate() error {
	if b.MaxAttempts < 0 || b.MaxTurns < 0 || b.MaxInputTokens < 0 || b.MaxOutputTokens < 0 || b.MaxTotalTokens < 0 || b.MaxCostMicros < 0 || b.MaxDurationMS < 0 || b.MaxActions < 0 {
		return errors.New("budget allocation limits cannot be negative")
	}
	if b.WarningPermille < 0 || b.WarningPermille > 1000 {
		return errors.New("budget allocation warning threshold must be between 0 and 1000 permille")
	}
	if b.MaxAttempts == 0 && b.MaxTurns == 0 && b.MaxInputTokens == 0 && b.MaxOutputTokens == 0 && b.MaxTotalTokens == 0 && b.MaxCostMicros == 0 && b.MaxDurationMS == 0 && b.MaxActions == 0 {
		return errors.New("budget allocation requires at least one finite limit")
	}
	return nil
}

type DelegateMode string

const (
	DelegateBehavior DelegateMode = "behavior"
	DelegateReason   DelegateMode = "reason"
)

type DecisionStep struct {
	Cases   []DecisionCase `json:"cases"`
	Default string         `json:"default,omitempty"`
}

type DecisionCase struct {
	When Predicate `json:"when"`
	Next string    `json:"next"`
}

type TransformStep struct {
	Assignments map[string]Value `json:"assignments"`
	Next        string           `json:"next"`
}

type WaitStep struct {
	Duration time.Duration `json:"duration,omitempty"`
	Event    string        `json:"event,omitempty"`
	Next     string        `json:"next"`
}

type ForkStep struct {
	Branches      map[string]string           `json:"branches"`
	BranchBudgets map[string]BudgetAllocation `json:"branchBudgets,omitempty"`
	Join          string                      `json:"join"`
}

type JoinMode string

const (
	JoinAll JoinMode = "all"
	JoinAny JoinMode = "any"
)

type JoinStep struct {
	Fork string   `json:"fork"`
	Mode JoinMode `json:"mode"`
	Next string   `json:"next"`
}

type ForEachStep struct {
	Items         Value  `json:"items"`
	ItemName      string `json:"itemName"`
	MaxIterations int    `json:"maxIterations"`
	Body          string `json:"body"`
	Next          string `json:"next"`
}

type LoopReturnStep struct {
	ForEach string `json:"forEach"`
}

type EndStep struct {
	Outputs map[string]Value `json:"outputs,omitempty"`
}

// Value is either a literal JSON value or a JSON Pointer into the durable
// runbook context. Exactly one source must be present.
type Value struct {
	Literal  json.RawMessage   `json:"literal,omitempty"`
	Ref      string            `json:"ref,omitempty"`
	Template []TemplateSegment `json:"template,omitempty"`
}

type TemplateSegment struct {
	Text string `json:"text,omitempty"`
	Ref  string `json:"ref,omitempty"`
}

type PredicateOperator string

const (
	PredicateEqual    PredicateOperator = "equal"
	PredicateNotEqual PredicateOperator = "not_equal"
	PredicateExists   PredicateOperator = "exists"
	PredicateTruthy   PredicateOperator = "truthy"
	PredicateGreater  PredicateOperator = "greater"
	PredicateAtLeast  PredicateOperator = "at_least"
	PredicateLess     PredicateOperator = "less"
	PredicateAtMost   PredicateOperator = "at_most"
	PredicateContains PredicateOperator = "contains"
	PredicateAll      PredicateOperator = "all"
	PredicateAny      PredicateOperator = "any"
	PredicateNot      PredicateOperator = "not"
)

type Predicate struct {
	Operator PredicateOperator `json:"operator"`
	Left     *Value            `json:"left,omitempty"`
	Right    *Value            `json:"right,omitempty"`
	Operands []Predicate       `json:"operands,omitempty"`
}

type Diagnostic struct {
	Path    string `json:"path"`
	Code    string `json:"code"`
	Message string `json:"message"`
}
