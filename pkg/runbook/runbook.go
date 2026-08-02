// Package runbook defines OpenSeal's portable deterministic automation plan.
// It contains no product, UI-node, transport, or enterprise control-plane
// concepts; every side effect is an exact governed Skill action identity.
package runbook

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/internal/domaincontract"
	"github.com/robfig/cron/v3"
)

const APIVersion = "openseal.dev/runbook/v1alpha1"

var reportingChannelPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

type Definition struct {
	APIVersion  string               `json:"apiVersion"`
	ID          string               `json:"id"`
	Version     string               `json:"version"`
	Name        string               `json:"name"`
	Description string               `json:"description,omitempty"`
	Entrypoints map[string]string    `json:"entrypoints"`
	Interfaces  map[string]Interface `json:"interfaces,omitempty"`
	Triggers    map[string]Trigger   `json:"triggers,omitempty"`
	Steps       map[string]Step      `json:"steps"`
}

// Interface makes one runbook entrypoint safely callable by a cognitive Agent.
// Schemas describe only credential-free invocation data and durable outputs;
// Skill bindings and approval authority remain kernel-owned.
type Interface struct {
	Description  string                 `json:"description"`
	InputSchema  map[string]interface{} `json:"inputSchema"`
	OutputSchema map[string]interface{} `json:"outputSchema,omitempty"`
}

type TriggerKind string

const TriggerEvent TriggerKind = "event"

const TriggerSchedule TriggerKind = "schedule"

func (TriggerKind) ContractValues() []string {
	return []string{string(TriggerEvent), string(TriggerSchedule)}
}

func (kind TriggerKind) Valid() bool { return domaincontract.Allows(string(kind), kind) }

// Trigger declares how an external normalized wake enters a deterministic
// Runbook. Transport configuration and credentials stay in provider Skills;
// the Runbook consumes only a canonical event type and entrypoint.
type Trigger struct {
	Kind              TriggerKind         `json:"kind"`
	EventType         string              `json:"eventType,omitempty"`
	Schedule          *Schedule           `json:"schedule,omitempty"`
	Entrypoint        string              `json:"entrypoint"`
	ObjectiveID       string              `json:"objectiveId,omitempty"`
	Input             map[string]Value    `json:"input,omitempty"`
	Budget            *BudgetAllocation   `json:"budget,omitempty"`
	Evidence          *EvidenceProjection `json:"evidence,omitempty"`
	Reporting         *ReportingPolicy    `json:"reporting,omitempty"`
	MaximumConcurrent int                 `json:"maximumConcurrent,omitempty"`
}

func (Trigger) ContractObjectVariants() []domaincontract.ObjectVariant {
	return []domaincontract.ObjectVariant{
		{Name: "event", Match: map[string]string{"kind": string(TriggerEvent)}, Required: []string{"eventType"}, Forbidden: []string{"schedule"}},
		{Name: "schedule", Match: map[string]string{"kind": string(TriggerSchedule)}, Required: []string{"schedule"}, Forbidden: []string{"eventType"}},
	}
}

type ReportingMilestone string

const (
	ReportingStarted          ReportingMilestone = "started"
	ReportingApprovalRequired ReportingMilestone = "approval_required"
	ReportingCompleted        ReportingMilestone = "completed"
	ReportingFailed           ReportingMilestone = "failed"
)

func (ReportingMilestone) ContractValues() []string {
	return []string{string(ReportingStarted), string(ReportingApprovalRequired), string(ReportingCompleted), string(ReportingFailed)}
}

func (milestone ReportingMilestone) Valid() bool {
	return domaincontract.Allows(string(milestone), milestone)
}

// ReportingPolicy declares the durable owner-channel projection for Runs
// created by this trigger. Channel is a portable logical key, not a transport
// address or database identity: activation resolves it to an existing owner
// channel or creates it idempotently. Milestones deliberately exclude raw
// tool and Turn events so normal operation remains calm while full detail
// stays available in the canonical activity log.
type ReportingPolicy struct {
	Channel    string               `json:"channel"`
	Title      string               `json:"title"`
	Milestones []ReportingMilestone `json:"milestones"`
}

func (p *ReportingPolicy) Validate() error {
	if p == nil {
		return nil
	}
	if !reportingChannelPattern.MatchString(p.Channel) {
		return errors.New("reporting channel must be a lowercase portable key of 1-64 characters")
	}
	if title := strings.TrimSpace(p.Title); title == "" || len(title) > 240 {
		return errors.New("reporting title must be 1-240 characters")
	}
	if len(p.Milestones) == 0 {
		return errors.New("reporting must select at least one milestone")
	}
	seen := make(map[ReportingMilestone]bool, len(p.Milestones))
	for _, milestone := range p.Milestones {
		if !milestone.Valid() {
			return fmt.Errorf("reporting milestone %q is unsupported", milestone)
		}
		if seen[milestone] {
			return fmt.Errorf("reporting milestone %q is duplicated", milestone)
		}
		seen[milestone] = true
	}
	return nil
}

// EvidenceProjection bounds the immutable, credential-free evidence snapshot
// captured when a Runbook trigger creates a Run. It belongs to the execution
// method, not to the Objective that describes the desired outcome.
type EvidenceProjection struct {
	Disabled            bool `json:"disabled,omitempty"`
	MaximumObservations int  `json:"maximumObservations,omitempty"`
	MaximumSummaryRunes int  `json:"maximumSummaryRunes,omitempty"`
	MaximumTotalRunes   int  `json:"maximumTotalRunes,omitempty"`
}

func (p *EvidenceProjection) Validate() error {
	if p == nil {
		return nil
	}
	if p.MaximumObservations < 0 || p.MaximumObservations > 99 || p.MaximumSummaryRunes < 0 || p.MaximumSummaryRunes > 4000 || p.MaximumTotalRunes < 0 || p.MaximumTotalRunes > 100000 {
		return errors.New("evidence projection bounds are invalid")
	}
	return nil
}

// Schedule is the portable timing contract of a Runbook trigger. Authoring
// surfaces may accept friendly phrases such as "daily" or "every weekday",
// but persist one explicit six-field cron expression. Jitter is part of the
// trigger itself and selects one restart-stable instant after each cron
// occurrence; it is never a detached Objective setting or a worker timer.
type Schedule struct {
	Cron               string `json:"cron"`
	Timezone           string `json:"timezone"`
	JitterSeconds      int64  `json:"jitterSeconds,omitempty"`
	MaximumOccurrences int64  `json:"maximumOccurrences,omitempty"`
}

const maximumScheduleJitterSeconds = 31 * 24 * 60 * 60

func (s *Schedule) Validate() error {
	if s == nil {
		return errors.New("schedule is required")
	}
	if s.JitterSeconds < 0 || s.JitterSeconds > maximumScheduleJitterSeconds {
		return fmt.Errorf("schedule jitterSeconds must be between 0 and %d", maximumScheduleJitterSeconds)
	}
	if s.MaximumOccurrences < 0 || s.MaximumOccurrences > 1_000_000 {
		return errors.New("schedule maximumOccurrences must be between 0 and 1000000")
	}
	if strings.TrimSpace(s.Timezone) == "" {
		return errors.New("schedule timezone is required")
	}
	if _, err := time.LoadLocation(strings.TrimSpace(s.Timezone)); err != nil {
		return fmt.Errorf("schedule timezone: %w", err)
	}
	_, err := parseScheduleCron(s.Cron)
	return err
}

// NextBase returns the next unjittered cron occurrence. Durable schedulers
// persist this cursor so replicas never derive timing from process-local state.
func (s *Schedule) NextBase(after time.Time) (time.Time, error) {
	if err := s.Validate(); err != nil {
		return time.Time{}, err
	}
	location, _ := time.LoadLocation(strings.TrimSpace(s.Timezone))
	parsed, _ := parseScheduleCron(s.Cron)
	return parsed.Next(after.In(location)).UTC(), nil
}

// DueAt deterministically selects the instant inside one cron occurrence's
// jitter window. triggerKey must identify the activated Runbook trigger, not
// merely its reusable definition, so independently configured activations do
// not converge on the same offset.
func (s *Schedule) DueAt(triggerKey string, base time.Time) (time.Time, error) {
	if err := s.Validate(); err != nil {
		return time.Time{}, err
	}
	key := strings.TrimSpace(triggerKey)
	if key == "" {
		return time.Time{}, errors.New("scheduled Runbook trigger requires a stable trigger key")
	}
	base = base.UTC()
	if s.JitterSeconds == 0 {
		return base, nil
	}
	seed := strings.Join([]string{key, strings.TrimSpace(s.Cron), strings.TrimSpace(s.Timezone), base.Format(time.RFC3339Nano)}, "\x00")
	digest := sha256.Sum256([]byte(seed))
	offset := binary.BigEndian.Uint64(digest[:8]) % uint64(s.JitterSeconds+1)
	return base.Add(time.Duration(offset) * time.Second), nil
}

func (s *Schedule) Window(base time.Time) (time.Time, time.Time, error) {
	if err := s.Validate(); err != nil {
		return time.Time{}, time.Time{}, err
	}
	start := base.UTC()
	return start, start.Add(time.Duration(s.JitterSeconds) * time.Second), nil
}

func parseScheduleCron(value string) (cron.Schedule, error) {
	expression := strings.TrimSpace(value)
	if expression == "" {
		return nil, errors.New("schedule cron is required")
	}
	if len(expression) > 128 {
		return nil, errors.New("schedule cron cannot exceed 128 characters")
	}
	parser := cron.NewParser(cron.Second | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	parsed, err := parser.Parse(expression)
	if err != nil {
		return nil, fmt.Errorf("schedule cron must contain six valid fields: %w", err)
	}
	return parsed, nil
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

func (StepKind) ContractValues() []string {
	return []string{string(StepAction), string(StepDelegate), string(StepDecision), string(StepTransform), string(StepWait), string(StepFork), string(StepJoin), string(StepForEach), string(StepLoopReturn), string(StepEnd)}
}

func (kind StepKind) Valid() bool { return domaincontract.Allows(string(kind), kind) }

// JSONPointer is the canonical scalar used for references into durable
// Runbook context. Its structural contract is consumed by both schema
// generation and runtime validation.
type JSONPointer string

func (JSONPointer) ContractPattern() string   { return "^/" }
func (JSONPointer) ContractMinLength() uint64 { return 1 }
func (pointer JSONPointer) Valid() bool       { return strings.HasPrefix(string(pointer), "/") }

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

func (Step) ContractObjectVariants() []domaincontract.ObjectVariant {
	variants := []struct {
		kind    StepKind
		payload string
	}{
		{StepAction, "action"}, {StepDelegate, "delegate"}, {StepDecision, "decision"}, {StepTransform, "transform"},
		{StepWait, "wait"}, {StepFork, "fork"}, {StepJoin, "join"}, {StepForEach, "forEach"},
		{StepLoopReturn, "loopReturn"}, {StepEnd, "end"},
	}
	payloads := make([]string, 0, len(variants))
	for _, variant := range variants {
		payloads = append(payloads, variant.payload)
	}
	result := make([]domaincontract.ObjectVariant, 0, len(variants))
	for _, variant := range variants {
		forbidden := make([]string, 0, len(payloads)-1)
		for _, payload := range payloads {
			if payload != variant.payload {
				forbidden = append(forbidden, payload)
			}
		}
		result = append(result, domaincontract.ObjectVariant{
			Name: string(variant.kind), Match: map[string]string{"kind": string(variant.kind)},
			Required: []string{variant.payload}, Forbidden: forbidden,
		})
	}
	return result
}

type ActionStep struct {
	SkillID      string           `json:"skillId"`
	SkillVersion string           `json:"skillVersion"`
	Action       string           `json:"action"`
	Arguments    map[string]Value `json:"arguments,omitempty"`
	ResultPath   JSONPointer      `json:"resultPath"`
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
	ResultPath JSONPointer       `json:"resultPath"`
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

const DefaultRunTotalTokenBudget int64 = 1_000_000

// DefaultBudgetAllocation returns the portable token ceiling used when work
// has no explicitly reviewed budget. Individual token dimensions remain
// unset so providers can use the total flexibly across input and output.
func DefaultBudgetAllocation() *BudgetAllocation {
	return &BudgetAllocation{MaxTotalTokens: DefaultRunTotalTokenBudget}
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

func (DelegateMode) ContractValues() []string {
	return []string{string(DelegateBehavior), string(DelegateReason)}
}

func (mode DelegateMode) Valid() bool { return domaincontract.Allows(string(mode), mode) }

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

func (JoinMode) ContractValues() []string { return []string{string(JoinAll), string(JoinAny)} }
func (mode JoinMode) Valid() bool         { return domaincontract.Allows(string(mode), mode) }

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
	Ref      JSONPointer       `json:"ref,omitempty"`
	Template []TemplateSegment `json:"template,omitempty"`
}

func (Value) ContractObjectVariants() []domaincontract.ObjectVariant {
	return []domaincontract.ObjectVariant{
		{Name: "literal", Required: []string{"literal"}, Forbidden: []string{"ref", "template"}},
		{Name: "ref", Required: []string{"ref"}, Forbidden: []string{"literal", "template"}},
		{Name: "template", Required: []string{"template"}, Forbidden: []string{"literal", "ref"}, MinItems: map[string]uint64{"template": 1}},
	}
}

type TemplateSegment struct {
	Text string      `json:"text,omitempty"`
	Ref  JSONPointer `json:"ref,omitempty"`
}

func (TemplateSegment) ContractObjectVariants() []domaincontract.ObjectVariant {
	return []domaincontract.ObjectVariant{
		{Name: "text", Required: []string{"text"}, Forbidden: []string{"ref"}},
		{Name: "ref", Required: []string{"ref"}, Forbidden: []string{"text"}},
	}
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

func (PredicateOperator) ContractValues() []string {
	return []string{string(PredicateEqual), string(PredicateNotEqual), string(PredicateExists), string(PredicateTruthy), string(PredicateGreater), string(PredicateAtLeast), string(PredicateLess), string(PredicateAtMost), string(PredicateContains), string(PredicateAll), string(PredicateAny), string(PredicateNot)}
}

func (operator PredicateOperator) Valid() bool {
	return domaincontract.Allows(string(operator), operator)
}

type Predicate struct {
	Operator PredicateOperator `json:"operator"`
	Left     *Value            `json:"left,omitempty"`
	Right    *Value            `json:"right,omitempty"`
	Operands []Predicate       `json:"operands,omitempty"`
}

func (Predicate) ContractObjectVariants() []domaincontract.ObjectVariant {
	binary := []PredicateOperator{PredicateEqual, PredicateNotEqual, PredicateGreater, PredicateAtLeast, PredicateLess, PredicateAtMost, PredicateContains}
	unary := []PredicateOperator{PredicateExists, PredicateTruthy}
	result := make([]domaincontract.ObjectVariant, 0, len(binary)+len(unary)+3)
	for _, operator := range binary {
		result = append(result, domaincontract.ObjectVariant{Name: string(operator), Match: map[string]string{"operator": string(operator)}, Required: []string{"left", "right"}, Forbidden: []string{"operands"}})
	}
	for _, operator := range unary {
		result = append(result, domaincontract.ObjectVariant{Name: string(operator), Match: map[string]string{"operator": string(operator)}, Required: []string{"left"}, Forbidden: []string{"right", "operands"}})
	}
	for _, operator := range []PredicateOperator{PredicateAll, PredicateAny} {
		result = append(result, domaincontract.ObjectVariant{Name: string(operator), Match: map[string]string{"operator": string(operator)}, Required: []string{"operands"}, Forbidden: []string{"left", "right"}, MinItems: map[string]uint64{"operands": 2}})
	}
	result = append(result, domaincontract.ObjectVariant{Name: string(PredicateNot), Match: map[string]string{"operator": string(PredicateNot)}, Required: []string{"operands"}, Forbidden: []string{"left", "right"}, MinItems: map[string]uint64{"operands": 1}, MaxItems: map[string]uint64{"operands": 1}})
	return result
}

type Diagnostic struct {
	Path    string `json:"path"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ValidationError preserves typed Runbook diagnostics across package
// boundaries. Callers may render the message for humans or retain exact paths
// and codes for bounded machine repair.
type ValidationError struct {
	Diagnostics []Diagnostic
}

func (e *ValidationError) Error() string {
	if e == nil || len(e.Diagnostics) == 0 {
		return "runbook validation failed"
	}
	first := e.Diagnostics[0]
	return fmt.Sprintf("runbook %s: %s", first.Path, first.Message)
}
