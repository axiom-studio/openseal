package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/axiom-studio/openseal/pkg/workspace"
	"go.uber.org/zap"
)

// The kernel tool executor of the run loop (SPEC §2.4 LoopToolExecutor,
// §3.1 phases 2, 7, 8 and 10, §5.2). It plans the tool entries of a step,
// routes each call to its executor (control tools, capability tools, host
// tools, governed Skill/MCP actions), runs the inline phase in model order,
// settles the step's one settle-phase control call, resumes open entries of
// earlier steps and re-plans a park after a failed park CAS. D9 and the G13
// guards run here through loopRepeatGuard and the failure guards.

// StepToolDispatcherConfig wires the services shared by every claim.
type StepToolDispatcherConfig struct {
	Sessions AgentSessionStore
	Turns    AgentTurnStore
	// Actions reads the Run's action calls (image rejection, live browser and
	// setup guards).
	Actions ActionStore
	// Coordinator records the refused row of a governed repeat refusal.
	Coordinator *ActionCoordinator
	Governed    *GovernedToolExecutor
	// Control runs the control calls. One built without its own tools gets
	// every tool of the run loop (loopControlRegistry) here.
	Control *ControlExecutor
	// Host runs host tool calls; nil means this host runs none.
	Host *HostToolDispatcher
	// Output keeps governed results over the 64 KiB cap as chat files
	// (optional; without it they are compacted).
	Output ToolOutputKeeper
	Logger *zap.SugaredLogger
	Now    func() time.Time
}

// StepToolDispatcher builds the per-claim LoopToolExecutor.
type StepToolDispatcher struct {
	cfg StepToolDispatcherConfig
}

func NewStepToolDispatcher(cfg StepToolDispatcherConfig) (*StepToolDispatcher, error) {
	if cfg.Sessions == nil || cfg.Turns == nil || cfg.Actions == nil {
		return nil, errors.New("step tool dispatcher requires a session store, a turn store and an action store")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = zap.NewNop().Sugar()
	}
	if cfg.Control != nil {
		cfg.Control.useTools(loopControlRegistry())
	}
	return &StepToolDispatcher{cfg: cfg}, nil
}

// LoopToolClaim is what one claim adds to the shared services: the chat
// Workspace its host tools work in, the Skill prompts of its catalog, the
// capability set bounds and the routine-script command pins.
type LoopToolClaim struct {
	Workspace        *workspace.Authority // nil: no host tools
	SkillPrompts     []HostedSkillPrompt
	CapabilityConfig CapabilitySetConfig
	CommandPins      []workspace.CommandPin
}

// ForClaim returns the LoopToolExecutor of one claim (TurnRunnerBinding.Tools).
func (d *StepToolDispatcher) ForClaim(claim LoopToolClaim) LoopToolExecutor {
	return &claimToolExecutor{cfg: d.cfg, claim: claim, hostTools: HostToolSpecs(claim.Workspace)}
}

// LoopCapabilityCatalog is the authorized catalog of a claim: the binding's
// governed actions and runbook operations, the claim's Skill prompts and the
// host tools of its Workspace. The model-step preparer builds its tool list
// from the same catalog, so offered and executable tools agree.
func LoopCapabilityCatalog(run *AgentRun, binding *TurnRunnerBinding, claim LoopToolClaim) CapabilityCatalog {
	catalog := CapabilityCatalog{SkillPrompts: claim.SkillPrompts, HostTools: HostToolSpecs(claim.Workspace)}
	if run != nil {
		catalog.Scope, catalog.AgentID = run.Scope, run.AssignedAgentID
	}
	if binding != nil {
		catalog.Actions, catalog.RunbookOperations = binding.ModelActions, binding.RunbookOperations
	}
	return catalog
}

// claimToolExecutor is the LoopToolExecutor of one claim.
type claimToolExecutor struct {
	cfg       StepToolDispatcherConfig
	claim     LoopToolClaim
	hostTools []HostToolSpec
}

func (x *claimToolExecutor) now() time.Time { return x.cfg.Now().UTC() }

// ---------------------------------------------------------------------------
// Routing

type loopToolRoute string

const (
	loopRouteControl    loopToolRoute = "control"
	loopRouteCapability loopToolRoute = "capability"
	loopRouteHost       loopToolRoute = "host"
	loopRouteGoverned   loopToolRoute = "governed"
	loopRouteUnknown    loopToolRoute = "unknown"
)

// resolvedLoopTool is where one tool call goes and how D9 and the ordering
// barrier see it.
type resolvedLoopTool struct {
	Route      loopToolRoute
	Control    ControlToolName
	Phase      ControlPhase // control tools only
	Host       HostToolSpec
	Action     *capability.ModelAction
	SideEffect ToolSideEffect
	Subject    RepeatSubject
}

// settle reports a settle-phase control call.
func (t resolvedLoopTool) settle() bool {
	return t.Route == loopRouteControl && t.Phase == ControlPhaseSettle
}

// concurrent reports a call that may run beside its neighbours (§5.2):
// side effect none or read.
func (t resolvedLoopTool) concurrent() bool {
	return t.SideEffect == ToolSideEffectNone || t.SideEffect == ToolSideEffectRead
}

// resolveLoopTool routes a call by name: control tools, then capability
// tools, then host tools, then the binding's governed actions.
func (x *claimToolExecutor) resolveLoopTool(binding *TurnRunnerBinding, name string, arguments json.RawMessage, directive *ToolCallDirective) (resolvedLoopTool, error) {
	args := decodeLoopToolArguments(arguments)
	switch {
	case IsControlTool(name):
		phase, _ := ControlToolPhase(name)
		effect := repeatEffectOf(LoopToolKernel, "", false, name)
		return resolvedLoopTool{
			Route: loopRouteControl, Control: ControlToolName(name), Phase: phase,
			SideEffect: kernelToolSideEffect(effect),
			Subject:    RepeatSubject{Identity: name, Arguments: args, Effect: effect},
		}, nil
	case name == LoadCapabilitiesTool || name == SearchCapabilitiesTool:
		effect := repeatEffectOf(LoopToolKernel, "", false, name)
		return resolvedLoopTool{
			Route: loopRouteCapability, SideEffect: kernelToolSideEffect(effect),
			Subject: RepeatSubject{Identity: name, Arguments: args, Effect: effect},
		}, nil
	}
	if spec, ok := LookupHostTool(x.hostTools, name); ok {
		side := ToolSideEffectWrite
		if spec.SideEffect == capability.SideEffectRead {
			side = ToolSideEffectRead
		}
		return resolvedLoopTool{
			Route: loopRouteHost, Host: spec, SideEffect: side,
			Subject: RepeatSubject{
				Identity: spec.Operation, Arguments: args,
				Effect: repeatEffectOf(LoopToolHost, spec.SideEffect, false, spec.Operation),
			},
		}, nil
	}
	action, err := selectGovernedAction(binding, name, directive)
	if err != nil {
		return resolvedLoopTool{}, err
	}
	if action == nil {
		return resolvedLoopTool{
			Route: loopRouteUnknown, SideEffect: ToolSideEffectNone,
			Subject: RepeatSubject{Identity: "unknown:" + name, Arguments: args, Effect: RepeatEffectControl},
		}, nil
	}
	subject := RepeatSubject{
		Identity:  repeatGovernedIdentity(action.SkillID, action.Action, action.BindingID),
		Arguments: args,
		Effect:    repeatEffectOf(LoopToolGoverned, action.SideEffect, action.Repeatable, repeatGovernedOperation(action.SkillID, action.Action)),
	}
	raw := arguments
	if len(strings.TrimSpace(string(raw))) == 0 {
		raw = json.RawMessage(`{}`)
	}
	if input, err := ParseGovernedToolInput(*action, raw, governedDirective(directive)); err == nil {
		subject.Arguments, subject.ExternalOperation = input.Arguments, input.ExternalOperation
	}
	side := ToolSideEffectWrite
	switch action.SideEffect {
	case capability.SideEffectNone:
		side = ToolSideEffectNone
	case capability.SideEffectRead:
		side = ToolSideEffectRead
	}
	return resolvedLoopTool{Route: loopRouteGoverned, Action: action, SideEffect: side, Subject: subject}, nil
}

func kernelToolSideEffect(effect RepeatEffect) ToolSideEffect {
	switch effect {
	case RepeatEffectRead:
		return ToolSideEffectRead
	case RepeatEffectControl:
		return ToolSideEffectNone
	}
	return ToolSideEffectWrite
}

// decodeLoopToolArguments decodes a call's JSON object arguments; anything
// else is an empty object (the store only keeps objects).
func decodeLoopToolArguments(raw json.RawMessage) map[string]interface{} {
	var args map[string]interface{}
	if len(strings.TrimSpace(string(raw))) > 0 {
		_ = json.Unmarshal(raw, &args)
	}
	if args == nil {
		args = map[string]interface{}{}
	}
	return args
}

func loopEntryDirective(entry *SessionEntry) (*ToolCallDirective, error) {
	if entry == nil || entry.Tool == nil || len(entry.Tool.Directive) == 0 {
		return nil, nil
	}
	var directive ToolCallDirective
	if err := json.Unmarshal(entry.Tool.Directive, &directive); err != nil {
		return nil, fmt.Errorf("decode directive of tool entry %s: %w", entry.ID, err)
	}
	return &directive, nil
}

// ---------------------------------------------------------------------------
// Planning (SPEC §3.1 phase 7)

// toolEntryPlanner is implemented by the claim executor.
type toolEntryPlanner interface {
	planToolEntries(binding *TurnRunnerBinding, assistant *SessionEntry) ([]*SessionEntry, error)
	planToolCallEntries(binding *TurnRunnerBinding, run *AgentRun, step *AgentTurn, sessionID string, calls []LoopToolCall) ([]*SessionEntry, error)
}

// Plan-time refusal code and texts.
const (
	loopToolArgumentsTooLargeCode = "tool_arguments_too_large"
	loopToolNotAvailableCode      = "tool_not_available"
	loopToolNotLoadedCode         = "tool_not_loaded"
	loopToolRefusedCode           = "tool_refused"
	loopToolNotRunCode            = "not_run"
)

// PlanToolEntries builds the tool entries of a model step's assistant entry,
// one per tool call in call order, for the loop's single
// [assistant, tool×N] append. Each entry carries SideEffect, RepeatEffect and
// ArgumentsDigest. A call with ArgumentsError or oversize arguments is
// appended already error/refused. assistant must carry its session, Run and
// step identity; binding.Tools must be a claim executor from
// StepToolDispatcher.ForClaim.
func PlanToolEntries(binding *TurnRunnerBinding, assistant *SessionEntry) ([]*SessionEntry, error) {
	planner, err := bindingToolPlanner(binding)
	if err != nil {
		return nil, err
	}
	return planner.planToolEntries(binding, assistant)
}

// PlanToolCallEntries builds the tool entries of a deterministic producer's
// tool calls (StepOutput.ToolCalls). Each entry stores its directive.
func PlanToolCallEntries(binding *TurnRunnerBinding, run *AgentRun, step *AgentTurn, sessionID string, calls []LoopToolCall) ([]*SessionEntry, error) {
	planner, err := bindingToolPlanner(binding)
	if err != nil {
		return nil, err
	}
	return planner.planToolCallEntries(binding, run, step, sessionID, calls)
}

func bindingToolPlanner(binding *TurnRunnerBinding) (toolEntryPlanner, error) {
	if binding == nil || binding.Tools == nil {
		return nil, errors.New("tool entries need a binding with a tool executor")
	}
	planner, ok := binding.Tools.(toolEntryPlanner)
	if !ok {
		return nil, fmt.Errorf("tool executor %T cannot plan tool entries", binding.Tools)
	}
	return planner, nil
}

func (x *claimToolExecutor) planToolEntries(binding *TurnRunnerBinding, assistant *SessionEntry) ([]*SessionEntry, error) {
	if assistant == nil || assistant.Type != SessionEntryAssistant || assistant.Assistant == nil {
		return nil, errors.New("plan tool entries needs an assistant entry")
	}
	if assistant.SessionID == "" || assistant.StepID == "" {
		return nil, errors.New("plan tool entries needs the assistant entry's session and step")
	}
	entries := make([]*SessionEntry, 0, len(assistant.Assistant.ToolCalls))
	for _, call := range assistant.Assistant.ToolCalls {
		entry := &SessionEntry{
			ID:    SessionEntryID(assistant.SessionID, ToolEntryKey(assistant.StepID, call.ID)),
			Scope: assistant.Scope, SessionID: assistant.SessionID, RunID: assistant.RunID,
			StepID: assistant.StepID, Step: assistant.Step,
			Type: SessionEntryTool, State: ToolEntryPending, ToolCallID: call.ID,
			Tool: &ToolEntry{Name: call.Name, Arguments: call.Arguments, ArgumentsError: call.ArgumentsError, SideEffect: ToolSideEffectNone},
		}
		if err := x.planEntry(binding, entry, nil); err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

func (x *claimToolExecutor) planToolCallEntries(binding *TurnRunnerBinding, run *AgentRun, step *AgentTurn, sessionID string, calls []LoopToolCall) ([]*SessionEntry, error) {
	if run == nil || step == nil || sessionID == "" {
		return nil, errors.New("plan tool call entries needs the Run, its step and its session")
	}
	entries := make([]*SessionEntry, 0, len(calls))
	for _, call := range calls {
		entry := &SessionEntry{
			ID:    SessionEntryID(sessionID, ToolEntryKey(step.ID, call.ID)),
			Scope: run.Scope, SessionID: sessionID, RunID: run.ID, StepID: step.ID, Step: step.Sequence,
			Type: SessionEntryTool, State: ToolEntryPending, ToolCallID: call.ID,
			Tool: &ToolEntry{Name: call.Name, Arguments: call.Arguments, SideEffect: ToolSideEffectNone},
		}
		if call.Directive != nil {
			encoded, err := json.Marshal(call.Directive)
			if err != nil {
				return nil, fmt.Errorf("encode directive of tool call %s: %w", call.ID, err)
			}
			entry.Tool.Directive = encoded
		}
		if err := x.planEntry(binding, entry, call.Directive); err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// planEntry stamps one pending entry, or ends it error/refused when its
// arguments cannot be used.
func (x *claimToolExecutor) planEntry(binding *TurnRunnerBinding, entry *SessionEntry, directive *ToolCallDirective) error {
	tool := entry.Tool
	if len(tool.Arguments) > MaxToolArgumentsBytes {
		tool.ArgumentsError = fmt.Sprintf("The tool arguments are %d bytes; the limit is %d.", len(tool.Arguments), MaxToolArgumentsBytes)
		tool.Arguments = nil
		entry.State = ToolEntryError
		tool.Result = controlErrorResult(ToolResultRefused, loopToolArgumentsTooLargeCode, tool.ArgumentsError, nil)
		return nil
	}
	if tool.ArgumentsError != "" {
		entry.State = ToolEntryError
		tool.Result = controlErrorResult(ToolResultRefused, controlArgumentsInvalidCode, tool.ArgumentsError, nil)
		return nil
	}
	resolved, err := x.resolveLoopTool(binding, tool.Name, tool.Arguments, directive)
	if err != nil {
		return err
	}
	tool.SideEffect = resolved.SideEffect
	return stampRepeatFields(tool, resolved.Subject)
}

// ---------------------------------------------------------------------------
// One step's (or one resumed step's) entries

// stepToolRun holds the entries of one step while the executor works on them.
type stepToolRun struct {
	x       *claimToolExecutor
	in      StepToolsInput
	binding *TurnRunnerBinding
	// step produced the entries; current is the claim's running step (the
	// TurnID of new governed calls and the nested VM fence of host calls).
	step    *AgentTurn
	current *AgentTurn
	// narration is the step's assistant text.
	narration string

	entries []*SessionEntry
	tools   []resolvedLoopTool
	guard   *loopRepeatGuard

	mu        sync.Mutex
	chain     loopRunChain
	batch     bool
	batchRuns []*AgentRun
	governed  []GovernedToolOutcome // awaiting governed outcomes, in call order
	held      bool                  // a call of the step reached awaiting_* (or waits for a foreign call lease)
	retryAt   *time.Time
	park      *LoopPark // a control entry's (non-governed) park
	final     *FinalAnswer
	failure   *LoopFailure
	budget    *BudgetExhaustion
	ended     bool
	hostCalls int
	searches  int
	setupIdx  []int // governed skill-management entries that may need the setup note

	setOnce  sync.Once
	set      *CapabilitySet
	setErr   error
	setMu    sync.Mutex // ExecuteLoadCapabilities updates the set in place
	viewOnce sync.Once
	views    []governedToolView
	viewErr  error
}

func (x *claimToolExecutor) newStepToolRun(in StepToolsInput, step *AgentTurn, entries []*SessionEntry) (*stepToolRun, error) {
	if in.Run == nil || in.Binding == nil {
		return nil, errors.New("step tools need the Run and its binding")
	}
	if in.Run.Status != AgentRunStatusRunning || in.WorkerID == "" || in.Run.LeaseOwner != in.WorkerID {
		return nil, ErrLeaseLost
	}
	r := &stepToolRun{
		x: x, in: in, binding: in.Binding, step: step, current: in.Step,
		entries: make([]*SessionEntry, len(entries)), tools: make([]resolvedLoopTool, len(entries)),
		chain: loopRunChain{known: in.Run},
	}
	if r.current == nil {
		r.current = step
	}
	if in.Assistant != nil && in.Assistant.Assistant != nil && in.Assistant.StepID == stepIDOf(step) {
		r.narration = in.Assistant.Assistant.Text
	}
	for i, entry := range entries {
		if entry == nil || entry.Type != SessionEntryTool || entry.Tool == nil || entry.RunID != in.Run.ID {
			return nil, fmt.Errorf("step tool entry %d is not a tool entry of Run %s", i, in.Run.ID)
		}
		r.entries[i] = entry
		directive, err := loopEntryDirective(entry)
		if err != nil {
			return nil, err
		}
		resolved, err := x.resolveLoopTool(in.Binding, entry.Tool.Name, entry.Tool.Arguments, directive)
		if err != nil {
			return nil, err
		}
		r.tools[i] = resolved
	}
	return r, nil
}

func stepIDOf(step *AgentTurn) string {
	if step == nil {
		return ""
	}
	return step.ID
}

func (r *stepToolRun) run() *AgentRun {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.chain.known
}

// input is the step input with the Run as the executor's writes left it.
func (r *stepToolRun) input() StepToolsInput {
	in := r.in
	in.Run = r.run()
	if r.step != nil {
		in.Step = r.step
	}
	return in
}

func (r *stepToolRun) lease() AgentRunLeaseGuard {
	return AgentRunLeaseGuard{WorkerID: r.in.WorkerID, Now: r.x.now()}
}

func (r *stepToolRun) sessionID() string {
	if r.in.Session != nil && r.in.Session.ID != "" {
		return r.in.Session.ID
	}
	return r.in.Run.SessionID
}

// adoptRun follows the Run through the executor's own writes. Inside a
// concurrent batch the writes are collected and adopted by endBatch.
func (r *stepToolRun) adoptRun(run *AgentRun) {
	if run == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.batch {
		r.batchRuns = append(r.batchRuns, run)
		return
	}
	r.chain.adopt(run)
}

// loopRunChain is the Run as the step's own writes left it. A write is
// adopted while it extends the known revision; concurrent writers cannot be
// told apart from a foreign write, so two of them break the chain: the loop's
// park CAS then fails on the stale revision and re-plans (SPEC §3.2).
type loopRunChain struct {
	known  *AgentRun
	broken bool
}

func (c *loopRunChain) adopt(run *AgentRun) {
	if c.broken || run == nil || c.known == nil || run.Revision <= c.known.Revision {
		return
	}
	c.known = run
}

func (c *loopRunChain) adoptBatch(runs []*AgentRun) {
	switch len(runs) {
	case 0:
	case 1:
		c.adopt(runs[0])
	default:
		c.broken = true
	}
}

func (r *stepToolRun) stopped() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.failure != nil || r.budget != nil || r.final != nil || r.ended
}

func (r *stepToolRun) isHeld() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.held
}

func (r *stepToolRun) setEntry(i int, entry *SessionEntry) {
	r.mu.Lock()
	r.entries[i] = entry
	r.mu.Unlock()
	if r.guard != nil {
		r.guard.Observe(entry)
	}
}

func (r *stepToolRun) entry(i int) *SessionEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.entries[i]
}

// transition moves entry i to to. executed marks the transition of a call
// that ran (D9 result digest, Parked when it leaves an awaiting state).
func (r *stepToolRun) transition(ctx context.Context, i int, to ToolEntryState, result *ToolResult, wait *ToolEntryWait, actionCallID string, kernel *ToolEntryKernelPatch, executed bool) error {
	entry := r.entry(i)
	if actionCallID == entry.ActionCallID {
		actionCallID = ""
	}
	lease := r.lease()
	req := ToolEntryTransitionRequest{
		Scope: r.in.Run.Scope, EntryID: entry.ID, RunID: r.in.Run.ID, Lease: lease,
		From: entry.State, ExpectedRevision: entry.Revision, To: to,
		ActionCallID: actionCallID, Wait: wait, Result: result, Kernel: kernel, Now: lease.Now,
	}
	if result != nil {
		req.Tokens = estimateEntryTokens(withToolResult(entry, to, result))
	}
	if executed {
		req = withRepeatKernelFields(req)
	}
	transitioned, err := r.x.cfg.Sessions.TransitionToolEntry(ctx, req)
	if err != nil {
		return fmt.Errorf("transition tool entry %s %s -> %s: %w", entry.ToolCallID, entry.State, to, err)
	}
	r.setEntry(i, transitioned.Entry)
	return r.afterTool(ctx, transitioned.Entry)
}

// adopted records an entry another executor (control, guard) transitioned.
func (r *stepToolRun) adopted(ctx context.Context, i int, entry *SessionEntry) error {
	if entry == nil {
		return nil
	}
	r.setEntry(i, entry)
	return r.afterTool(ctx, entry)
}

func (r *stepToolRun) afterTool(ctx context.Context, entry *SessionEntry) error {
	if entry == nil || !entry.State.Terminal() || r.binding.AfterTool == nil {
		return nil
	}
	if err := r.binding.AfterTool(ctx, r.input(), entry); err != nil {
		return fmt.Errorf("after tool entry %s: %w", entry.ToolCallID, err)
	}
	return nil
}

// refuse ends a call that does not run with an is_error result.
func (r *stepToolRun) refuse(ctx context.Context, i int, result *ToolResult, guard *ToolGuardOutcome) error {
	var kernel *ToolEntryKernelPatch
	if guard != nil {
		kernel = &ToolEntryKernelPatch{Guard: guard}
	}
	return r.transition(ctx, i, ToolEntryError, result, nil, "", kernel, false)
}

// ---------------------------------------------------------------------------
// Pre-dispatch checks (phase rules, offered tools, ToolCheck)

// capabilities is the step's capability set: the claim's catalog with the
// loads of the path the step started from. Loads in this step are callable
// in the next step.
func (r *stepToolRun) capabilities(ctx context.Context) (*CapabilitySet, error) {
	r.setOnce.Do(func() {
		path := r.in.Path
		if path == nil && r.x.cfg.Sessions != nil && r.sessionID() != "" {
			path, r.setErr = r.x.cfg.Sessions.LoadAgentSessionPath(ctx, r.in.Run.Scope, r.sessionID())
			if r.setErr != nil {
				return
			}
		}
		r.set = NewPathCapabilitySet(LoopCapabilityCatalog(r.in.Run, r.binding, r.x.claim), path, r.x.claim.CapabilityConfig)
	})
	return r.set, r.setErr
}

// visible reports whether a governed or host tool is callable in this step:
// when it is pinned or loaded.
func (r *stepToolRun) visible(ctx context.Context, tool resolvedLoopTool) (bool, error) {
	set, err := r.capabilities(ctx)
	if err != nil {
		return false, err
	}
	r.setMu.Lock()
	defer r.setMu.Unlock()
	switch tool.Route {
	case loopRouteGoverned:
		return set.Loaded(capabilityActionPrefix + tool.Action.Name), nil
	case loopRouteHost:
		return set.Loaded(capabilityWorkspacePrefix + tool.Host.Operation), nil
	}
	return true, nil
}

// precheck refuses, in call order, every pending call of a fresh step that
// must not run: the control phase rules (a second settle call, a lone tool
// with others), tools the claim does not offer, tools not loaded yet, and the
// binding's ToolCheck.
func (r *stepToolRun) precheck(ctx context.Context) error {
	authority := r.binding.Authority
	if r.in.Assistant != nil && r.in.Assistant.Assistant != nil {
		refusals := controlCallRefusals(r.in.Assistant.Assistant.ToolCalls, authority)
		for i, entry := range r.entries {
			if result := refusals[entry.ToolCallID]; result != nil && entry.State == ToolEntryPending {
				if err := r.refuse(ctx, i, result, nil); err != nil {
					return err
				}
			}
		}
	}
	for i := range r.entries {
		entry := r.entry(i)
		if entry.State != ToolEntryPending || entry.ActionCallID != "" {
			continue
		}
		result, err := r.offerRefusal(ctx, i)
		if err != nil {
			return err
		}
		if result == nil && r.binding.ToolCheck != nil {
			refusal, err := r.binding.ToolCheck(ctx, r.input(), entry)
			if err != nil {
				return fmt.Errorf("check tool call %s: %w", entry.ToolCallID, err)
			}
			if strings.TrimSpace(refusal) != "" {
				result = controlErrorResult(ToolResultRefused, loopToolRefusedCode, strings.TrimSpace(refusal), nil)
			}
		}
		if result != nil {
			if err := r.refuse(ctx, i, result, nil); err != nil {
				return err
			}
		}
	}
	return nil
}

// offerRefusal is the is_error result of a call the claim does not offer,
// nil when it may run. Kernel-origin (directive) calls are preauthorized by
// their producer.
func (r *stepToolRun) offerRefusal(ctx context.Context, i int) (*ToolResult, error) {
	entry, tool := r.entry(i), r.tools[i]
	kernel := len(entry.Tool.Directive) > 0
	notAvailable := func() *ToolResult {
		return controlErrorResult(ToolResultRefused, loopToolNotAvailableCode,
			"This tool is not available in this conversation.", map[string]interface{}{"tool": entry.Tool.Name})
	}
	switch tool.Route {
	case loopRouteUnknown:
		return controlErrorResult(ToolResultRefused, loopToolNotAvailableCode,
			fmt.Sprintf("No tool named %q is available here.", entry.Tool.Name), map[string]interface{}{"tool": entry.Tool.Name}), nil
	case loopRouteControl:
		if !kernel && (r.x.cfg.Control == nil || !controlToolOffered(tool.Control, r.binding.Authority)) {
			return notAvailable(), nil
		}
		if r.x.cfg.Control == nil {
			return notAvailable(), nil
		}
	case loopRouteCapability:
		if !r.binding.Authority.offersCapabilities() {
			return notAvailable(), nil
		}
		set, err := r.capabilities(ctx)
		if err != nil {
			return nil, err
		}
		if !set.Offered() {
			return notAvailable(), nil
		}
	case loopRouteHost, loopRouteGoverned:
		if kernel {
			return nil, nil
		}
		if tool.Route == loopRouteGoverned && r.x.cfg.Governed == nil {
			return notAvailable(), nil
		}
		ok, err := r.visible(ctx, tool)
		if err != nil {
			return nil, err
		}
		if !ok {
			return controlErrorResult(ToolResultRefused, loopToolNotLoadedCode,
				"This tool is not loaded yet; load its capability with load_capabilities, then call it in your next step.",
				map[string]interface{}{"tool": entry.Tool.Name}), nil
		}
	}
	return nil, nil
}

// startGuard opens the D9 guard over the step's entries (pending ones are
// evaluated in call order) and applies its refusals before anything runs.
func (r *stepToolRun) startGuard(ctx context.Context) error {
	calls := make([]loopRepeatCall, len(r.entries))
	for i := range r.entries {
		calls[i] = loopRepeatCall{Entry: r.entry(i), Governed: r.repeatGoverned(i)}
	}
	run := r.run()
	guard, err := startLoopRepeatGuard(ctx, loopRepeatGuardConfig{
		Sessions: r.x.cfg.Sessions, Refusals: r.refusals(), Run: run, WorkerID: r.in.WorkerID,
		WaitOffered: controlToolOffered(ControlToolWait, r.binding.Authority), Now: r.x.cfg.Now,
	}, calls)
	if err != nil {
		return err
	}
	r.guard = guard
	refused, err := guard.ApplyRefusals(ctx)
	if err != nil {
		return err
	}
	for _, entry := range refused {
		for i := range r.entries {
			if r.entry(i).ID == entry.ID {
				if err := r.adopted(ctx, i, entry); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (r *stepToolRun) refusals() refusedLoopCallRecorder {
	if r.x.cfg.Coordinator == nil {
		return nil
	}
	return r.x.cfg.Coordinator
}

// repeatGoverned is what a governed refusal by count needs for its refused
// action_calls row.
func (r *stepToolRun) repeatGoverned(i int) *loopRepeatGovernedCall {
	tool := r.tools[i]
	if tool.Route != loopRouteGoverned || tool.Action == nil {
		return nil
	}
	action := tool.Action
	return &loopRepeatGovernedCall{
		DeploymentID: actionDeploymentForSelection(r.binding, action),
		SkillID:      action.SkillID, SkillVersion: action.Version, Action: action.Action,
		BindingID: action.BindingID, BindingRevision: action.BindingRevision,
		Risk: skill.RiskLevel(action.Risk), SideEffect: skill.SideEffect(action.SideEffect),
		InputSchema: action.InputSchema, Arguments: tool.Subject.Arguments,
	}
}

// dispatchable reports a pending call of the inline phase that the guard
// lets run.
func (r *stepToolRun) dispatchable(i int) bool {
	entry := r.entry(i)
	if entry.State != ToolEntryPending || r.tools[i].settle() {
		return false
	}
	return r.guard == nil || r.guard.Dispatch(entry.ID)
}

// ---------------------------------------------------------------------------
// Executing one call

// runCall executes the pending (or, on resume, open) entry i by its route.
func (r *stepToolRun) runCall(ctx context.Context, i int) error {
	switch r.tools[i].Route {
	case loopRouteHost:
		return r.runHost(ctx, i)
	case loopRouteCapability:
		return r.runCapability(ctx, i)
	case loopRouteGoverned:
		return r.runGoverned(ctx, i)
	case loopRouteControl:
		return r.runControlInline(ctx, i)
	}
	entry := r.entry(i)
	return r.refuse(ctx, i, controlErrorResult(ToolResultRefused, loopToolNotAvailableCode,
		fmt.Sprintf("No tool named %q is available here.", entry.Tool.Name), nil), nil)
}

func (r *stepToolRun) runHost(ctx context.Context, i int) error {
	entry, spec := r.entry(i), r.tools[i].Host
	if r.x.cfg.Host == nil || r.x.claim.Workspace == nil {
		return r.refuse(ctx, i, hostToolErrorResult(ToolResultFailed, hostToolUnsupportedMessage), nil)
	}
	if entry.State != ToolEntryRunning {
		if err := r.transition(ctx, i, ToolEntryRunning, nil, nil, "", nil, false); err != nil {
			return err
		}
		entry = r.entry(i)
	}
	outcome, ok := r.x.cfg.Host.Run(ctx, HostToolCall{
		Run: r.run(), Step: r.current, Binding: r.binding, SessionID: r.sessionID(),
		ToolEntryID: entry.ID, ToolCallID: entry.ToolCallID, Spec: spec,
		Arguments: decodeLoopToolArguments(entry.Tool.Arguments), Workspace: *r.x.claim.Workspace,
		CommandPins: r.x.claim.CommandPins,
	})
	r.mu.Lock()
	r.hostCalls++
	r.mu.Unlock()
	if !ok {
		// Lease lost or canceled: the entry stays running for D14 resume.
		if err := ctx.Err(); err != nil {
			return err
		}
		return ErrLeaseLost
	}
	kernel, err := outcome.KernelPatch()
	if err != nil {
		return fmt.Errorf("encode host tool outcome of %s: %w", entry.ToolCallID, err)
	}
	return r.transition(ctx, i, outcome.State, outcome.Result, nil, "", kernel, true)
}

func (r *stepToolRun) runCapability(ctx context.Context, i int) error {
	entry := r.entry(i)
	set, err := r.capabilities(ctx)
	if err != nil {
		return err
	}
	args := decodeLoopToolArguments(entry.Tool.Arguments)
	var content json.RawMessage
	var isError bool
	var loaded []string
	r.setMu.Lock()
	switch entry.Tool.Name {
	case LoadCapabilitiesTool:
		content, isError, loaded = ExecuteLoadCapabilities(set, args, loopCapabilityToolName(r.binding.ModelActions))
	default:
		searched := 0
		if r.in.Path != nil {
			searched = CapabilitySearchesThisRun(r.in.Path.Entries, r.in.Run.ID)
		}
		content, isError = ExecuteSearchCapabilities(set, args, searched+r.searches)
		if !isError {
			r.searches++
		}
	}
	r.setMu.Unlock()
	status, to := ToolResultSucceeded, ToolEntryCompleted
	if isError {
		status, to = ToolResultFailed, ToolEntryError
	}
	var kernel *ToolEntryKernelPatch
	if len(loaded) > 0 {
		kernel = loadKernelPatch(set, r.binding.ModelActions, loaded)
	}
	result := &ToolResult{Status: status, IsError: isError, Content: []ContentPart{{Type: ContentPartJSON, Value: content}}}
	return r.transition(ctx, i, to, result, nil, "", kernel, true)
}

// loadKernelPatch freezes what a load made visible on its tool entry: the
// definition of each new tool exactly as later steps declare it, and the IDs
// of the instructions and runbooks its result carried.
func loadKernelPatch(set *CapabilitySet, actions []capability.ModelAction, loaded []string) *ToolEntryKernelPatch {
	names := LoopGovernedToolNames(actions)
	patch := &ToolEntryKernelPatch{}
	for _, id := range loaded {
		if action, ok := set.actions[id]; ok {
			patch.SurfacedTools = append(patch.SurfacedTools, surfacedTool(id, governedModelStepTool(action, names[action.Name], ModelStepToolLoaded)))
			continue
		}
		if spec, ok := set.hostTools[id]; ok {
			patch.SurfacedTools = append(patch.SurfacedTools, surfacedTool(id, hostModelStepTool(spec, ModelStepToolLoaded)))
			continue
		}
		patch.LoadedInstructions = append(patch.LoadedInstructions, id)
	}
	return patch
}

func (r *stepToolRun) controlContext(i int) ControlContext {
	in := r.input()
	if r.in.Assistant == nil || r.in.Assistant.StepID != stepIDOf(r.step) {
		in.Assistant = nil
	}
	return NewControlContext(in, r.entry(i))
}

func (r *stepToolRun) runControlInline(ctx context.Context, i int) error {
	if r.x.cfg.Control == nil {
		return r.refuse(ctx, i, controlErrorResult(ToolResultRefused, loopToolNotAvailableCode,
			"This tool is not available in this conversation.", nil), nil)
	}
	result, err := r.x.cfg.Control.Inline(ctx, r.controlContext(i))
	if err != nil {
		return err
	}
	return r.applyControl(ctx, i, result)
}

// applyControl records a control outcome on the step.
func (r *stepToolRun) applyControl(ctx context.Context, i int, result ControlResult) error {
	r.adoptRun(result.Run)
	if err := r.adopted(ctx, i, result.Entry); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	switch result.Kind {
	case ControlParked:
		park := *result.Park
		r.park, r.held = &park, true
	case ControlFinal:
		r.final = result.Final
	case ControlEnded:
		r.ended = true
	case ControlFailed:
		r.failure = result.Failure
	}
	return nil
}

func (r *stepToolRun) governedCall(i int) (GovernedToolCall, error) {
	entry := r.entry(i)
	directive, err := loopEntryDirective(entry)
	if err != nil {
		return GovernedToolCall{}, err
	}
	run := r.run()
	return GovernedToolCall{
		Run: run, Step: r.current, WorkerID: r.in.WorkerID, Binding: r.binding,
		ToolCallID: entry.ToolCallID, Name: entry.Tool.Name, Arguments: entry.Tool.Arguments,
		Directive: directive, Narration: r.narration,
	}, nil
}

// runGoverned proposes and executes a governed call that has no action call
// yet, after the image rejection and live browser guards.
func (r *stepToolRun) runGoverned(ctx context.Context, i int) error {
	entry, action := r.entry(i), r.tools[i].Action
	if entry.State == ToolEntryPending && entry.ActionCallID == "" {
		prior, err := priorImageProviderRejection(ctx, r.x.cfg.Actions, r.in.Run.Scope, r.in.Run.ID, imageGenerationTarget{
			SkillID: action.SkillID, Action: action.Action, BindingID: action.BindingID, BindingRevision: action.BindingRevision,
		})
		if err != nil {
			return err
		}
		if prior != nil {
			result, guard := imageProviderRejectionRefusal(prior)
			return r.refuse(ctx, i, result, guard)
		}
		if strings.HasPrefix(action.Name, liveBrowserActionPrefix) {
			views, err := r.governedViews(ctx)
			if err != nil {
				return err
			}
			if result, refused := refuseLiveBrowserWhileRateLimited(views, action.Name); refused {
				return r.refuse(ctx, i, result, nil)
			}
		}
	}
	if entry.State == ToolEntryPending {
		if err := r.transition(ctx, i, ToolEntryRunning, nil, nil, "", nil, false); err != nil {
			return err
		}
	}
	call, err := r.governedCall(i)
	if err != nil {
		return err
	}
	var outcome GovernedToolOutcome
	if r.entry(i).ActionCallID == "" && entry.State == ToolEntryPending {
		outcome, err = r.x.cfg.Governed.Execute(ctx, call)
	} else {
		outcome, err = r.x.cfg.Governed.Resume(ctx, call, r.entry(i), r.run().PendingInterventions)
	}
	if err != nil {
		return err
	}
	return r.applyGoverned(ctx, i, outcome)
}

// resumeGoverned settles an open governed entry that already has (or may
// have) an action call.
func (r *stepToolRun) resumeGoverned(ctx context.Context, i int) error {
	call, err := r.governedCall(i)
	if err != nil {
		return err
	}
	outcome, err := r.x.cfg.Governed.Resume(ctx, call, r.entry(i), r.run().PendingInterventions)
	if err != nil {
		return err
	}
	return r.applyGoverned(ctx, i, outcome)
}

func (r *stepToolRun) governedViews(ctx context.Context) ([]governedToolView, error) {
	r.viewOnce.Do(func() {
		path := r.in.Path
		if path == nil && r.sessionID() != "" {
			path, r.viewErr = r.x.cfg.Sessions.LoadAgentSessionPath(ctx, r.in.Run.Scope, r.sessionID())
			if r.viewErr != nil {
				return
			}
		}
		r.views, r.viewErr = loadGovernedToolViews(ctx, r.x.cfg.Actions, r.in.Run.Scope, path, r.in.Run.ID)
	})
	return r.views, r.viewErr
}

// applyGoverned applies a governed outcome to its tool entry with one
// transition (through running when the edge needs it).
func (r *stepToolRun) applyGoverned(ctx context.Context, i int, outcome GovernedToolOutcome) error {
	r.adoptRun(outcome.Run)
	if exhausted := GovernedBudgetExhaustion(outcome); exhausted != nil {
		r.mu.Lock()
		r.budget = exhausted
		r.mu.Unlock()
	}
	entry := r.entry(i)
	state := outcome.State
	if !state.Valid() {
		return fmt.Errorf("governed outcome of %s has state %q", entry.ToolCallID, state)
	}
	if state == ToolEntryRunning {
		if entry.State != ToolEntryRunning {
			if err := r.transition(ctx, i, ToolEntryRunning, nil, nil, outcome.ActionCallID, nil, false); err != nil {
				return err
			}
		}
		r.mu.Lock()
		r.held = true
		if outcome.RetryAt != nil && (r.retryAt == nil || outcome.RetryAt.Before(*r.retryAt)) {
			at := outcome.RetryAt.UTC()
			r.retryAt = &at
		}
		r.mu.Unlock()
		return nil
	}
	if entry.State == state && state.Awaiting() {
		// Still waiting on the same target: nothing to write.
		r.recordAwaiting(outcome)
		return nil
	}
	if !ValidToolEntryTransition(entry.State, state) {
		if err := r.transition(ctx, i, ToolEntryRunning, nil, nil, outcome.ActionCallID, nil, true); err != nil {
			return err
		}
	}
	result := outcome.Result
	if result != nil {
		bounded, err := BoundToolResult(ctx, r.x.cfg.Output, toolOutputInvocation(r.in.Run, entry.Tool.Name, r.tools[i].Action.SkillID, r.tools[i].Action.Action, outcome.ActionCallID), *result)
		if err != nil {
			return err
		}
		result = &bounded
	}
	if err := r.transition(ctx, i, state, result, outcome.Wait, outcome.ActionCallID, nil, true); err != nil {
		return err
	}
	if state.Awaiting() {
		r.recordAwaiting(outcome)
		return nil
	}
	if r.tools[i].Action.SkillID == SkillManagementSkillID && state == ToolEntryCompleted {
		r.mu.Lock()
		r.setupIdx = append(r.setupIdx, i)
		r.mu.Unlock()
	}
	return nil
}

func (r *stepToolRun) recordAwaiting(outcome GovernedToolOutcome) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.governed = append(r.governed, outcome)
	r.held = true
}

// ---------------------------------------------------------------------------
// End of a step's tool work

// planPark is the step-end park: the governed park plan wins (approvals over
// cards), then a control park, then a wait for a foreign call lease. nil
// with settled == true means a wait target was decided meanwhile: do not
// park; the next step resumes the entries.
func (r *stepToolRun) planPark(ctx context.Context) (park *LoopPark, settled bool, err error) {
	r.mu.Lock()
	governed := append([]GovernedToolOutcome(nil), r.governed...)
	controlPark, retryAt := r.park, r.retryAt
	r.mu.Unlock()
	if len(governed) > 0 {
		plan, err := r.x.cfg.Governed.PlanPark(ctx, r.run(), governed)
		if err != nil {
			return nil, false, err
		}
		if err := r.applyReplaced(ctx, plan); err != nil {
			return nil, false, err
		}
		if plan.Settled {
			return nil, true, nil
		}
		if plan.Park != nil {
			return &LoopPark{Status: plan.Park.Status, Wake: plan.Park.Wake, Summary: plan.Park.Summary}, false, nil
		}
	}
	if controlPark != nil {
		return controlPark, false, nil
	}
	if retryAt != nil {
		at := *retryAt
		return &LoopPark{
			Status: AgentRunStatusSleeping, Wake: &WakeCondition{Type: "timer", WakeAt: &at},
			Summary: "Waiting for a tool call that is still running elsewhere",
		}, false, nil
	}
	return nil, false, nil
}

// applyReplaced writes the outcomes a park plan replaced (held approvals
// superseded by a newer message) to their entries.
func (r *stepToolRun) applyReplaced(ctx context.Context, plan GovernedParkPlan) error {
	r.adoptRun(plan.Run)
	for _, replaced := range plan.Replaced {
		for i := range r.entries {
			if r.entry(i).ToolCallID == replaced.ToolCallID && r.tools[i].Route == loopRouteGoverned {
				if err := r.applyGoverned(ctx, i, replaced); err != nil {
					return err
				}
			}
		}
	}
	// A replaced outcome is no longer awaiting.
	r.mu.Lock()
	kept := r.governed[:0]
	for _, outcome := range r.governed {
		replaced := false
		for _, other := range plan.Replaced {
			if other.ToolCallID == outcome.ToolCallID {
				replaced = true
			}
		}
		if !replaced {
			kept = append(kept, outcome)
		}
	}
	r.governed = kept
	r.mu.Unlock()
	return nil
}

// finishGuard appends the step's D9 notes and setup notes once every entry is
// terminal and returns the guard's Run failure (repeat_limit, error_streak).
func (r *stepToolRun) finishGuard(ctx context.Context) (*LoopFailure, error) {
	for i := range r.entries {
		if !r.entry(i).State.Terminal() {
			return nil, nil
		}
	}
	var failure *LoopFailure
	if r.guard != nil {
		var err error
		if failure, err = r.guard.Finish(ctx); err != nil {
			return nil, err
		}
	}
	if err := r.appendSetupNotes(ctx); err != nil {
		return nil, err
	}
	return failure, nil
}

// appendSetupNotes appends the foreground pending-setup note after the
// step's entries (never between tool results).
func (r *stepToolRun) appendSetupNotes(ctx context.Context) error {
	r.mu.Lock()
	indexes := append([]int(nil), r.setupIdx...)
	r.mu.Unlock()
	for _, i := range indexes {
		entry := r.entry(i)
		call, err := r.x.cfg.Actions.GetActionCall(ctx, r.in.Run.Scope, entry.ActionCallID)
		if err != nil {
			return err
		}
		run := r.run()
		note := pendingSkillSetupNote(run, r.sessionID(), entry, call, run.PendingInterventions)
		if note == nil {
			continue
		}
		note.Tokens = estimateEntryTokens(note)
		lease := r.lease()
		if _, err := r.x.cfg.Sessions.AppendAgentSessionEntries(ctx, AppendAgentSessionEntriesRequest{
			Scope: run.Scope, SessionID: r.sessionID(), RunID: run.ID, Lease: &lease,
			Entries: []*SessionEntry{note}, Now: lease.Now,
		}); err != nil {
			return fmt.Errorf("append setup note: %w", err)
		}
	}
	return nil
}

// outcome is the StepToolsOutcome of the work done so far.
func (r *stepToolRun) outcome() StepToolsOutcome {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := StepToolsOutcome{
		Final: r.final, Failure: r.failure, BudgetExhausted: r.budget, HostToolCalls: r.hostCalls,
	}
	if r.chain.known != r.in.Run {
		out.Run = r.chain.known
	}
	return out
}

// toolOutputInvocation is the call a long tool output is kept for. The keeper
// finds the Run, and so its chat, by scope and id: without the scope every
// output over the inline cap failed with "chat files are unavailable".
func toolOutputInvocation(run *AgentRun, name, skillID, action, actionCallID string) ToolInvocation {
	invocation := ToolInvocation{Name: name, SkillID: skillID, Action: action, ActionCallID: actionCallID}
	if run != nil {
		invocation.RunID = run.ID
		invocation.Scope = skill.ScopeReference{Kind: run.Scope.Kind, ID: run.Scope.ID}
		invocation.DeploymentID, invocation.ExecutionDeploymentID = run.Owner.ID, run.AssignedAgentID
	}
	return invocation
}
