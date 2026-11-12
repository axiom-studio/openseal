package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"time"

	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/modelstep"
)

// Kernel control tools (section 04 §3). The model calls them like any other
// tool; the kernel executes them inline, under the claim, inside the RunLoop.
// This file owns the tool names, the advertised order, the phase rules and the
// dispatcher. Each tool's schema, settle and resume live in its own file;
// builtinControlTools lists them.

// controlToolOrder is the stable advertised order (SPEC §5.1, prompt cache):
// the final tool of the Run's mode first, then progress, interaction, task
// start and child work. Capability search/load and host tools follow in the
// harness group; they are not control tools.
var controlToolOrder = []ControlToolName{
	ControlToolFinalAnswer, ControlToolRequestDecision, ControlToolCompletionReview,
	ControlToolReportProgress,
	ControlToolWait, ControlToolAskUser, ControlToolReact,
	ControlToolStartTask,
	ControlToolFork, ControlToolRunOperation, ControlToolDelegate, ControlToolDelegateReply,
	ControlToolRunbookFork, ControlToolRunbookDelegate,
}

type ControlPhase string

const (
	// ControlPhaseInline calls run in the step's normal tool phase.
	ControlPhaseInline ControlPhase = "inline"
	// ControlPhaseSettle calls run after the step row is finished and
	// applied (DI-2). At most one per assistant entry.
	ControlPhaseSettle ControlPhase = "settle"
)

// controlToolRule is the static, authority-independent part of a control
// tool: its phase and whether it must be the only call of its assistant entry.
type controlToolRule struct {
	Phase ControlPhase
	Alone bool
}

var controlToolRules = map[ControlToolName]controlToolRule{
	ControlToolFinalAnswer:      {Phase: ControlPhaseSettle, Alone: true},
	ControlToolRequestDecision:  {Phase: ControlPhaseSettle, Alone: true},
	ControlToolCompletionReview: {Phase: ControlPhaseSettle, Alone: true},
	ControlToolReportProgress:   {Phase: ControlPhaseInline},
	ControlToolWait:             {Phase: ControlPhaseSettle},
	ControlToolAskUser:          {Phase: ControlPhaseSettle},
	ControlToolReact:            {Phase: ControlPhaseInline},
	ControlToolStartTask:        {Phase: ControlPhaseSettle, Alone: true}, // A4
	ControlToolFork:             {Phase: ControlPhaseSettle},
	ControlToolRunOperation:     {Phase: ControlPhaseSettle},
	ControlToolDelegate:         {Phase: ControlPhaseSettle},
	ControlToolDelegateReply:    {Phase: ControlPhaseSettle},
	ControlToolRunbookFork:      {Phase: ControlPhaseSettle, Alone: true},
	ControlToolRunbookDelegate:  {Phase: ControlPhaseSettle, Alone: true},
}

// IsControlTool reports whether name is a kernel control tool.
func IsControlTool(name string) bool {
	_, ok := controlToolRules[ControlToolName(name)]
	return ok
}

// ControlToolPhase returns the phase of a control tool; ok is false for any
// other tool.
func ControlToolPhase(name string) (ControlPhase, bool) {
	rule, ok := controlToolRules[ControlToolName(name)]
	return rule.Phase, ok
}

func isFinalControlTool(name ControlToolName) bool {
	return name == ControlToolFinalAnswer || name == ControlToolRequestDecision || name == ControlToolCompletionReview
}

type ControlToolSpec struct {
	Name        ControlToolName
	Description string
	InputSchema map[string]interface{}
	Phase       ControlPhase
}

// ModelStepTool is the spec as a harness tool of a model step.
func (s ControlToolSpec) ModelStepTool() ModelStepTool {
	return ModelStepTool{
		Name: string(s.Name), Description: s.Description,
		InputSchema: maps.Clone(s.InputSchema), Group: ModelStepToolHarness,
	}
}

const ()

const (
	ControlOriginModel  agentrun.ControlOrigin = "model"
	ControlOriginKernel agentrun.ControlOrigin = "kernel"
)

// NewControlContext builds the context of one control tool entry of a step.
func NewControlContext(in agentrun.StepToolsInput, entry *SessionEntry) agentrun.ControlContext {
	cc := agentrun.ControlContext{
		Run: in.Run, WorkerID: in.WorkerID, Step: in.Step, Session: in.Session, Path: in.Path,
		Binding: in.Binding, Assistant: in.Assistant, Entry: entry, Origin: controlOriginOf(entry),
	}
	if in.Binding != nil {
		cc.Authority = in.Binding.Authority
	}
	return cc
}

// controlOriginOf: deterministic producers' calls carry a directive; model
// calls never do.
func controlOriginOf(entry *SessionEntry) agentrun.ControlOrigin {
	if entry != nil && entry.Tool != nil && len(entry.Tool.Directive) > 0 {
		return ControlOriginKernel
	}
	return ControlOriginModel
}

// ControlEntryMeta is kernel-only metadata stored in ToolEntry.Control. It is
// never rendered and never projected by any API.
type ControlEntryMeta struct {
	Kind              ControlToolName `json:"kind"`
	ForkID            string          `json:"forkId,omitempty"`
	GroupID           string          `json:"groupId,omitempty"`
	ChildRunIDs       []string        `json:"childRunIds,omitempty"`
	RequestID         string          `json:"requestId,omitempty"`
	TaskID            string          `json:"taskId,omitempty"`
	WorkRunID         string          `json:"workRunId,omitempty"`
	QuestionMessageID string          `json:"questionMessageId,omitempty"`
	WakeAt            *time.Time      `json:"wakeAt,omitempty"`
	ErrorCode         string          `json:"errorCode,omitempty"` // e.g. visual_invalid, task_completion_revise
}

// DecodeControlEntryMeta reads an entry's kernel control metadata (nil when
// the entry has none).
func DecodeControlEntryMeta(entry *SessionEntry) (*ControlEntryMeta, error) {
	if entry == nil || entry.Tool == nil || len(entry.Tool.Control) == 0 {
		return nil, nil
	}
	var meta ControlEntryMeta
	if err := json.Unmarshal(entry.Tool.Control, &meta); err != nil {
		return nil, fmt.Errorf("decode control metadata of tool entry %s: %w", entry.ID, err)
	}
	return &meta, nil
}

type ControlOutcomeKind string

const (
	ControlContinue ControlOutcomeKind = "continue" // tool entry terminal; the loop starts the next step
	ControlParked   ControlOutcomeKind = "parked"   // Park set; the loop parks (or, Committed, only releases the claim)
	ControlFinal    ControlOutcomeKind = "final"    // Final set; the loop runs the final path (SPEC §3.3)
	ControlEnded    ControlOutcomeKind = "ended"    // Run terminal in the tool's own transaction (task start, A4)
	ControlFailed   ControlOutcomeKind = "failed"   // Failure set; the loop fails the Run (SPEC §3.5)
)

// ControlResult is the outcome of one control call. There is no consumed
// intervention list: the steering cursor has one owner, consumeSteering
// (SPEC §3.4).
type ControlResult struct {
	Kind    ControlOutcomeKind
	Run     *AgentRun     // canonical Run after the operation (nil = unchanged)
	Entry   *SessionEntry // the tool entry after the operation
	Park    *agentrun.LoopPark
	Final   *agentrun.FinalAnswer
	Failure *agentrun.LoopFailure
}

func (r ControlResult) validate() error {
	switch r.Kind {
	case ControlContinue:
		if r.Entry == nil || !r.Entry.State.Terminal() {
			return errors.New("control continue result needs a terminal tool entry")
		}
	case ControlParked:
		if r.Park == nil {
			return errors.New("control parked result needs a park")
		}
		if err := r.Park.Validate(); err != nil {
			return err
		}
	case ControlFinal:
		if r.Final == nil {
			return errors.New("control final result needs a final answer")
		}
	case ControlEnded:
		if r.Run == nil || !isTerminalAgentRunStatus(r.Run.Status) {
			return errors.New("control ended result needs the terminal Run")
		}
	case ControlFailed:
		if r.Failure == nil {
			return errors.New("control failed result needs a failure")
		}
		if err := r.Failure.Validate(); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown control outcome %q", r.Kind)
	}
	return nil
}

// ControlResume is the outcome of resuming a parked control entry.
type ControlResume struct {
	Resolved bool               // false: still waiting
	Park     *agentrun.LoopPark // when !Resolved: the park to re-apply
	Result   ControlResult
}

// controlHandler implements one control tool. Spec is called only when the
// authority offers the tool; Decode is the strict argument decoder (use
// models.DecodeControlInto); Run is the inline or settle execution, by the
// tool's phase.
type controlHandler interface {
	Spec(a agentrun.ControlAuthority) ControlToolSpec
	Decode(raw json.RawMessage) (interface{}, error)
	Run(ctx context.Context, x *ControlExecutor, cc agentrun.ControlContext) (ControlResult, error)
}

// controlResumer is implemented by parking tools (awaiting_child /
// awaiting_event entries).
type controlResumer interface {
	Resume(ctx context.Context, x *ControlExecutor, cc agentrun.ControlContext) (ControlResume, error)
}

type controlRegistry map[ControlToolName]controlHandler

// controlTool names the handler of one control tool.
type controlTool struct {
	name    ControlToolName
	handler controlHandler
}

// newControlTool names handler as the tool name.
func newControlTool(name ControlToolName, handler controlHandler) controlTool {
	return controlTool{name: name, handler: handler}
}

// newControlRegistry builds the registry of tools. It panics on an unknown
// tool name or a tool listed twice: the lists are fixed in code.
func newControlRegistry(tools ...controlTool) controlRegistry {
	r := controlRegistry{}
	for _, tool := range tools {
		r.register(tool.name, tool.handler)
	}
	return r
}

// builtinControlTools are the control tools every Run kind shares. The
// kernel-only runbook tools are added by the run loop (loopControlRegistry).
func builtinControlTools() []controlTool {
	return []controlTool{
		{name: ControlToolAskUser, handler: askUserHandler{}},
		{name: ControlToolDelegate, handler: delegateHandler{}},
		{name: ControlToolDelegateReply, handler: delegateReplyHandler{}},
		{name: ControlToolFinalAnswer, handler: finalAnswerHandler{}},
		{name: ControlToolFork, handler: forkHandler{}},
		{name: ControlToolReact, handler: reactControlHandler{}},
		{name: ControlToolWait, handler: waitControlHandler{}},
		{name: ControlToolRunOperation, handler: operationHandler{}},
		{name: ControlToolReportProgress, handler: reportProgressHandler{}},
		{name: ControlToolRequestDecision, handler: requestDecisionHandler{}},
		{name: ControlToolCompletionReview, handler: completionReviewHandler{}},
		{name: ControlToolStartTask, handler: startTaskHandler{}},
	}
}

func (r controlRegistry) register(name ControlToolName, h controlHandler) {
	if _, ok := controlToolRules[name]; !ok {
		panic(fmt.Sprintf("register unknown control tool %q", name))
	}
	if h == nil {
		panic(fmt.Sprintf("register nil control handler for %q", name))
	}
	if _, dup := r[name]; dup {
		panic(fmt.Sprintf("control tool %q registered twice", name))
	}
	r[name] = h
}

// specs lists the offered tools in the stable order. A tool is offered when
// the authority allows it and a handler is registered.
func (r controlRegistry) specs(a agentrun.ControlAuthority) []ControlToolSpec {
	var specs []ControlToolSpec
	for _, name := range controlToolOrder {
		h, ok := r[name]
		if !ok || !modelstep.ControlToolOffered(name, a) {
			continue
		}
		spec := h.Spec(a)
		spec.Name = name
		spec.Phase = controlToolRules[name].Phase
		specs = append(specs, spec)
	}
	return specs
}

// ControlExecutor executes control tool entries. Constructed once per worker
// pool; the coordinators are used by the tool files.
type ControlExecutor struct {
	Portfolio     PortfolioStore
	Sessions      AgentSessionStore
	Activity      *RunActivityService
	Forks         *agentrun.RunForkCoordinator
	Collaboration *CollaborationService
	Tasks         *ConversationTaskService
	Conversations *ConversationService
	Reporting     ConversationStore // projectConversationRunbookStart / runbookSourceMessage
	Now           func() time.Time

	// handlers are the tools x runs; nil runs builtinControlTools.
	// NewStepToolDispatcher gives an executor without its own the run loop's
	// tools (useTools). Tests set their own.
	handlers controlRegistry
	// finalChecks run on every accepted final answer, in order; nil runs
	// builtinFinalAnswerChecks. Tests set their own.
	finalChecks []finalAnswerCheck
}

func (x *ControlExecutor) registry() controlRegistry {
	if x.handlers != nil {
		return x.handlers
	}
	return newControlRegistry(builtinControlTools()...)
}

// useTools makes x run tools when it was built without its own.
func (x *ControlExecutor) useTools(tools controlRegistry) {
	if x.handlers == nil {
		x.handlers = tools
	}
}

func (x *ControlExecutor) finalAnswerChecks() []finalAnswerCheck {
	if x.finalChecks != nil {
		return x.finalChecks
	}
	return builtinFinalAnswerChecks()
}

func (x *ControlExecutor) now() time.Time {
	if x.Now != nil {
		return x.Now().UTC()
	}
	return time.Now().UTC()
}

// ErrControlEntryState is returned when the dispatcher is asked to run a
// control entry that is not in a state its phase can act on (a loop bug, not
// a model error).
var ErrControlEntryState = errors.New("control tool entry is not in a runnable state")

// Inline executes an inline-phase control call (react, turn_report_progress).
func (x *ControlExecutor) Inline(ctx context.Context, cc agentrun.ControlContext) (ControlResult, error) {
	return x.dispatch(ctx, cc, ControlPhaseInline)
}

// Settle executes the step's single settle-phase control call, after
// FinishTurn + applyStep. It is called again after a crash for an entry still
// pending or running (all settles are idempotent by stable keys, §3.4).
//
// cc.Step must be the step that produced the entry (entry.StepID), finished
// and applied. When ResumeOpenTools settles an entry left open by an earlier
// step (after a park or a crash after the model call), pass that earlier
// step, not the claim's current running step.
func (x *ControlExecutor) Settle(ctx context.Context, cc agentrun.ControlContext) (ControlResult, error) {
	return x.dispatch(ctx, cc, ControlPhaseSettle)
}

func (x *ControlExecutor) dispatch(ctx context.Context, cc agentrun.ControlContext, phase ControlPhase) (ControlResult, error) {
	if err := x.checkContext(cc); err != nil {
		return ControlResult{}, err
	}
	name := cc.ToolName()
	rule, known := controlToolRules[name]
	if !known {
		return ControlResult{}, fmt.Errorf("tool %q is not a control tool", name)
	}
	if rule.Phase != phase {
		return ControlResult{}, fmt.Errorf("control tool %s runs in the %s phase, not %s", name, rule.Phase, phase)
	}
	if cc.Entry.State != ToolEntryPending && cc.Entry.State != ToolEntryRunning {
		return ControlResult{}, fmt.Errorf("%w: %s is %s", ErrControlEntryState, cc.Entry.ID, cc.Entry.State)
	}
	if phase == ControlPhaseSettle {
		if cc.Step == nil || cc.Step.Status != AgentTurnStatusCompleted || cc.Step.Sequence > cc.Run.LastAppliedTurn ||
			cc.Step.ID != cc.Entry.StepID {
			return ControlResult{}, fmt.Errorf("%w: settle needs the finished and applied step", ErrControlEntryState)
		}
	}
	h, registered := x.registry()[name]
	// Rule 1: a tool the claim does not offer. Kernel-origin calls are
	// preauthorized by their producer.
	if !registered || (cc.Origin != ControlOriginKernel && !modelstep.ControlToolOffered(name, cc.Authority)) {
		return x.refuse(ctx, cc, controlErrorResult(ToolResultRefused, "tool_not_available",
			"This tool is not available in this conversation.", map[string]interface{}{"tool": string(name)}))
	}
	// Rule 4: strict argument decoding.
	if cc.Entry.Tool.ArgumentsError != "" {
		return x.refuse(ctx, cc, controlArgumentsInvalid(cc.Entry.Tool.ArgumentsError))
	}
	args, err := h.Decode(cc.Entry.Tool.Arguments)
	if err != nil {
		return x.refuse(ctx, cc, controlArgumentsInvalid(err.Error()))
	}
	cc.Arguments = args
	result, err := h.Run(ctx, x, cc)
	if err != nil {
		return ControlResult{}, err
	}
	if err := result.validate(); err != nil {
		return ControlResult{}, fmt.Errorf("control tool %s: %w", name, err)
	}
	return result, nil
}

// Resume turns the awaited outcome of a parked control entry
// (awaiting_child / awaiting_event) into its tool result.
func (x *ControlExecutor) Resume(ctx context.Context, cc agentrun.ControlContext) (ControlResume, error) {
	if err := x.checkContext(cc); err != nil {
		return ControlResume{}, err
	}
	name := cc.ToolName()
	if !IsControlTool(string(name)) {
		return ControlResume{}, fmt.Errorf("tool %q is not a control tool", name)
	}
	if cc.Entry.State != ToolEntryAwaitingChild && cc.Entry.State != ToolEntryAwaitingEvent {
		return ControlResume{}, fmt.Errorf("%w: resume of %s in state %s", ErrControlEntryState, cc.Entry.ID, cc.Entry.State)
	}
	h, ok := x.registry()[name]
	if !ok {
		return ControlResume{}, fmt.Errorf("control tool %s has no handler", name)
	}
	resumer, ok := h.(controlResumer)
	if !ok {
		return ControlResume{}, fmt.Errorf("control tool %s does not park", name)
	}
	args, err := h.Decode(cc.Entry.Tool.Arguments)
	if err != nil {
		return ControlResume{}, fmt.Errorf("decode arguments of parked control entry %s: %w", cc.Entry.ID, err)
	}
	cc.Arguments = args
	resumed, err := resumer.Resume(ctx, x, cc)
	if err != nil {
		return ControlResume{}, err
	}
	if !resumed.Resolved {
		if resumed.Park == nil {
			return ControlResume{}, fmt.Errorf("control tool %s: unresolved resume needs a park", name)
		}
		if err := resumed.Park.Validate(); err != nil {
			return ControlResume{}, fmt.Errorf("control tool %s: %w", name, err)
		}
		return resumed, nil
	}
	if err := resumed.Result.validate(); err != nil {
		return ControlResume{}, fmt.Errorf("control tool %s: %w", name, err)
	}
	return resumed, nil
}

func (x *ControlExecutor) checkContext(cc agentrun.ControlContext) error {
	if x == nil || x.Sessions == nil {
		return errors.New("control executor has no session store")
	}
	if cc.Run == nil || cc.Entry == nil || cc.Entry.Type != SessionEntryTool || cc.Entry.Tool == nil {
		return errors.New("control call needs its Run and tool entry")
	}
	if cc.Entry.RunID != cc.Run.ID {
		return fmt.Errorf("control tool entry %s belongs to run %s, not %s", cc.Entry.ID, cc.Entry.RunID, cc.Run.ID)
	}
	// Rule 5: lease errors abort the claim.
	if cc.Run.Status != AgentRunStatusRunning || cc.WorkerID == "" || cc.Run.LeaseOwner != cc.WorkerID {
		return ErrLeaseLost
	}
	return nil
}

// refuse ends the entry with an is_error result; the loop continues (D5/D6).
func (x *ControlExecutor) refuse(ctx context.Context, cc agentrun.ControlContext, result *ToolResult) (ControlResult, error) {
	entry, err := x.transitionControlEntry(ctx, cc, ToolEntryError, nil, result, nil)
	if err != nil {
		return ControlResult{}, err
	}
	return ControlResult{Kind: ControlContinue, Entry: entry}, nil
}

// complete ends the entry with result and returns a continue outcome.
func (x *ControlExecutor) complete(ctx context.Context, cc agentrun.ControlContext, result *ToolResult, meta *ControlEntryMeta) (ControlResult, error) {
	to := ToolEntryCompleted
	if result != nil && result.IsError {
		to = ToolEntryError
	}
	entry, err := x.transitionControlEntry(ctx, cc, to, nil, result, meta)
	if err != nil {
		return ControlResult{}, err
	}
	return ControlResult{Kind: ControlContinue, Entry: entry}, nil
}

// transitionControlEntry moves the call's entry from its current state to
// to, fenced by the Run lease. A stale revision (the stores report it as
// ErrToolEntryStateConflict) is retried once after re-reading the entry,
// while it is still in the state the caller saw.
func (x *ControlExecutor) transitionControlEntry(ctx context.Context, cc agentrun.ControlContext, to ToolEntryState, wait *ToolEntryWait, result *ToolResult, meta *ControlEntryMeta) (*SessionEntry, error) {
	var kernel *ToolEntryKernelPatch
	if meta != nil {
		stored := *meta // the handler's value is left as it was
		if stored.Kind == "" {
			stored.Kind = cc.ToolName()
		}
		encoded, err := json.Marshal(&stored)
		if err != nil {
			return nil, fmt.Errorf("encode control metadata: %w", err)
		}
		kernel = &ToolEntryKernelPatch{Control: encoded}
	}
	current := cc.Entry
	for attempt := 0; ; attempt++ {
		now := x.now()
		req := ToolEntryTransitionRequest{
			Scope: cc.Run.Scope, EntryID: current.ID, RunID: cc.Run.ID, Lease: cc.LeaseGuard(now),
			From: current.State, ExpectedRevision: current.Revision, To: to,
			Wait: wait, Result: result, Kernel: kernel, Now: now,
		}
		if result != nil {
			req.Tokens = modelstep.EstimateEntryTokens(withToolResult(current, to, result))
		}
		transitioned, err := x.Sessions.TransitionToolEntry(ctx, req)
		if err == nil {
			return transitioned.Entry, nil
		}
		if attempt > 0 || !errors.Is(err, ErrToolEntryStateConflict) {
			return nil, err
		}
		reread, getErr := x.Sessions.GetSessionEntry(ctx, cc.Run.Scope, current.ID)
		if getErr != nil {
			return nil, getErr
		}
		if reread.State != current.State {
			return nil, err
		}
		current = reread
	}
}

// withToolResult is the entry as it will be stored, for the token estimate.
func withToolResult(entry *SessionEntry, to ToolEntryState, result *ToolResult) *SessionEntry {
	clone := *entry
	tool := *entry.Tool
	tool.Result = result
	clone.Tool = &tool
	clone.State = to
	return &clone
}

// ---------------------------------------------------------------------------
// Phase rules applied by the loop before the inline phase (§3.2 rules 2, 3)

const (
	controlOneSettleCallCode    = "control_one_settle_call"
	controlFinalNotAloneCode    = "final_answer_not_alone"
	controlTaskNotAloneCode     = "task_not_alone"
	controlArgumentsInvalidCode = "tool_arguments_invalid"
)

// controlCallRefusals applies rules 2 and 3 to the tool calls of one
// assistant entry, in call order. It returns the is_error result for every
// call that must not execute, keyed by tool call id:
//   - a tool that must run alone (final tools, conversation_start_task)
//     together with any other call is refused; the other calls run;
//   - of the remaining settle-phase calls, the first offered one settles and
//     the others are refused. A call to a tool the claim does not offer never
//     takes the settle slot (the dispatcher refuses it with
//     tool_not_available), so it cannot block an offered settle call.
func controlCallRefusals(calls []ModelToolCall, authority agentrun.ControlAuthority) map[string]*ToolResult {
	refusals := map[string]*ToolResult{}
	for _, call := range calls {
		name := ControlToolName(call.Name)
		if len(calls) > 1 && controlToolRules[name].Alone {
			if isFinalControlTool(name) {
				refusals[call.ID] = controlErrorResult(ToolResultRefused, controlFinalNotAloneCode,
					fmt.Sprintf("Call %s alone after your other tools have returned.", name), nil)
			} else {
				refusals[call.ID] = controlErrorResult(ToolResultRefused, controlTaskNotAloneCode,
					"Call conversation_start_task alone; other work belongs inside the task.", nil)
			}
		}
	}
	for _, index := range controlSettleConflicts(calls, authority) {
		call := calls[index]
		if refusals[call.ID] != nil {
			continue
		}
		refusals[call.ID] = controlErrorResult(ToolResultRefused, controlOneSettleCallCode,
			fmt.Sprintf("Call %s alone in its own message after this one returns.", call.Name), nil)
	}
	return refusals
}

// controlSettleConflicts returns the indexes of settle-phase calls after the
// first one that may settle. Calls refused by the alone rule and calls to
// tools the authority does not offer do not count.
func controlSettleConflicts(calls []ModelToolCall, authority agentrun.ControlAuthority) []int {
	var conflicts []int
	settled := false
	for i, call := range calls {
		name := ControlToolName(call.Name)
		rule, ok := controlToolRules[name]
		if !ok || rule.Phase != ControlPhaseSettle || !modelstep.ControlToolOffered(name, authority) {
			continue
		}
		if rule.Alone && len(calls) > 1 {
			continue
		}
		if settled {
			conflicts = append(conflicts, i)
			continue
		}
		settled = true
	}
	return conflicts
}

// controlSettleEntry returns the step's one open settle-phase control entry,
// if any (the loop's SettlePending).
func controlSettleEntry(entries []*SessionEntry) *SessionEntry {
	for _, entry := range entries {
		if entry == nil || entry.Type != SessionEntryTool || entry.Tool == nil || !entry.State.Open() {
			continue
		}
		if phase, ok := ControlToolPhase(entry.Tool.Name); ok && phase == ControlPhaseSettle {
			return entry
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Arguments and results on the wire (§3.3)

func controlArgumentsInvalid(message string) *ToolResult {
	return controlErrorResult(ToolResultFailed, controlArgumentsInvalidCode, message, nil)
}

// controlErrorResult is an is_error control result with a stable code and a
// one-sentence message for the model. extra adds fields (never overriding
// status, error or message).
func controlErrorResult(status ToolResultStatus, code, message string, extra map[string]interface{}) *ToolResult {
	if status == ToolResultSucceeded || status == "" {
		status = ToolResultFailed
	}
	value := map[string]interface{}{}
	for key, v := range extra {
		value[key] = v
	}
	value["status"] = string(status)
	value["error"] = code
	value["message"] = message
	return controlJSONResult(status, value)
}

// maximumControlTextBytes caps each child text inside a control result.
const maximumControlTextBytes = 16 << 10

// truncateControlText caps a child text at 16 KiB on a rune boundary,
// pointing the model at the Run that holds the full text.
func truncateControlText(text, runID string) string {
	if len(text) <= maximumControlTextBytes {
		return text
	}
	suffix := "…[truncated; full result is in run " + runID + "]"
	return cutUTF8(text, maximumControlTextBytes-len(suffix)) + suffix
}
