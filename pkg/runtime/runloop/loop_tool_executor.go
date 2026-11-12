package runloop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	runtimeaction "github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/control"
	"github.com/axiom-studio/openseal/pkg/runtime/modelstep"
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
	Sessions models.AgentSessionStore
	Turns    models.AgentTurnStore
	// Actions reads the Run's action calls (image rejection, live browser and
	// setup guards).
	Actions models.ActionStore
	// Coordinator records the refused row of a governed repeat refusal.
	Coordinator *runtimeaction.Coordinator
	Governed    *GovernedToolExecutor
	// Control runs the control calls. One built without its own tools gets
	// every tool of the run loop (loopControlRegistry) here.
	Control *control.Executor
	// Host runs host tool calls; nil means this host runs none.
	Host *modelstep.HostToolDispatcher
	// Output keeps governed results over the 64 KiB cap as chat files
	// (optional; without it they are compacted).
	Output models.ToolOutputKeeper
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
		cfg.Control.UseTools(loopControlRegistry())
	}
	return &StepToolDispatcher{cfg: cfg}, nil
}

// LoopToolClaim is what one claim adds to the shared services: the chat
// Workspace its host tools work in, the Skill prompts of its catalog, the
// capability set bounds and the routine-script command pins.
type LoopToolClaim struct {
	Workspace        *workspace.Authority // nil: no host tools
	SkillPrompts     []models.HostedSkillPrompt
	CapabilityConfig modelstep.CapabilitySetConfig
	CommandPins      []workspace.CommandPin
}

// ForClaim returns the LoopToolExecutor of one claim (TurnRunnerBinding.Tools).
func (d *StepToolDispatcher) ForClaim(claim LoopToolClaim) agentrun.LoopToolExecutor {
	return &ClaimToolExecutor{cfg: d.cfg, claim: claim, hostTools: modelstep.HostToolSpecs(claim.Workspace)}
}

// LoopCapabilityCatalog is the authorized catalog of a claim: the binding's
// governed actions and runbook operations, the claim's Skill prompts and the
// host tools of its Workspace. The model-step preparer builds its tool list
// from the same catalog, so offered and executable tools agree.
func LoopCapabilityCatalog(run *models.AgentRun, binding *agentrun.TurnRunnerBinding, claim LoopToolClaim) modelstep.CapabilityCatalog {
	catalog := modelstep.CapabilityCatalog{SkillPrompts: claim.SkillPrompts, HostTools: modelstep.HostToolSpecs(claim.Workspace)}
	if run != nil {
		catalog.Scope, catalog.AgentID = run.Scope, run.AssignedAgentID
	}
	if binding != nil {
		catalog.Actions, catalog.RunbookOperations = binding.ModelActions, binding.RunbookOperations
	}
	return catalog
}

// ClaimToolExecutor is the LoopToolExecutor of one claim.
type ClaimToolExecutor struct {
	cfg       StepToolDispatcherConfig
	claim     LoopToolClaim
	hostTools []modelstep.HostToolSpec
}

func (x *ClaimToolExecutor) now() time.Time { return x.cfg.Now().UTC() }

// control.Executor runs the claim's control calls.
func (x *ClaimToolExecutor) ControlExecutor() *control.Executor { return x.cfg.Control }

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
	Control    models.ControlToolName
	Phase      control.Phase // control tools only
	Host       modelstep.HostToolSpec
	Action     *capability.ModelAction
	SideEffect models.ToolSideEffect
	Subject    RepeatSubject
}

// settle reports a settle-phase control call.
func (t resolvedLoopTool) settle() bool {
	return t.Route == loopRouteControl && t.Phase == control.PhaseSettle
}

// concurrent reports a call that may run beside its neighbours (§5.2):
// side effect none or read.
func (t resolvedLoopTool) concurrent() bool {
	return t.SideEffect == models.ToolSideEffectNone || t.SideEffect == models.ToolSideEffectRead
}

// resolveLoopTool routes a call by name: control tools, then capability
// tools, then host tools, then the binding's governed actions.
func (x *ClaimToolExecutor) resolveLoopTool(binding *agentrun.TurnRunnerBinding, name string, arguments json.RawMessage, directive *agentrun.ToolCallDirective) (resolvedLoopTool, error) {
	args := decodeLoopToolArguments(arguments)
	switch {
	case control.IsControlTool(name):
		phase, _ := control.ToolPhase(name)
		effect := repeatEffectOf(LoopToolKernel, "", false, name)
		return resolvedLoopTool{
			Route: loopRouteControl, Control: models.ControlToolName(name), Phase: phase,
			SideEffect: kernelToolSideEffect(effect),
			Subject:    RepeatSubject{Identity: name, Arguments: args, Effect: effect},
		}, nil
	case name == modelstep.LoadCapabilitiesTool || name == modelstep.SearchCapabilitiesTool:
		effect := repeatEffectOf(LoopToolKernel, "", false, name)
		return resolvedLoopTool{
			Route: loopRouteCapability, SideEffect: kernelToolSideEffect(effect),
			Subject: RepeatSubject{Identity: name, Arguments: args, Effect: effect},
		}, nil
	}
	if spec, ok := modelstep.LookupHostTool(x.hostTools, name); ok {
		side := models.ToolSideEffectWrite
		if spec.SideEffect == capability.SideEffectRead {
			side = models.ToolSideEffectRead
		}
		return resolvedLoopTool{
			Route: loopRouteHost, Host: spec, SideEffect: side,
			Subject: RepeatSubject{
				Identity: spec.Operation, Arguments: args,
				Effect: repeatEffectOf(LoopToolHost, spec.SideEffect, false, spec.Operation),
			},
		}, nil
	}
	action, err := modelstep.SelectGovernedAction(binding, name, directive)
	if err != nil {
		return resolvedLoopTool{}, err
	}
	if action == nil {
		return resolvedLoopTool{
			Route: loopRouteUnknown, SideEffect: models.ToolSideEffectNone,
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
	if input, err := modelstep.ParseGovernedToolInput(*action, raw, governedDirective(directive)); err == nil {
		subject.Arguments, subject.ExternalOperation = input.Arguments, input.ExternalOperation
	}
	side := models.ToolSideEffectWrite
	switch action.SideEffect {
	case capability.SideEffectNone:
		side = models.ToolSideEffectNone
	case capability.SideEffectRead:
		side = models.ToolSideEffectRead
	}
	return resolvedLoopTool{Route: loopRouteGoverned, Action: action, SideEffect: side, Subject: subject}, nil
}

func kernelToolSideEffect(effect RepeatEffect) models.ToolSideEffect {
	switch effect {
	case RepeatEffectRead:
		return models.ToolSideEffectRead
	case RepeatEffectControl:
		return models.ToolSideEffectNone
	}
	return models.ToolSideEffectWrite
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

func loopEntryDirective(entry *models.SessionEntry) (*agentrun.ToolCallDirective, error) {
	if entry == nil || entry.Tool == nil || len(entry.Tool.Directive) == 0 {
		return nil, nil
	}
	var directive agentrun.ToolCallDirective
	if err := json.Unmarshal(entry.Tool.Directive, &directive); err != nil {
		return nil, fmt.Errorf("decode directive of tool entry %s: %w", entry.ID, err)
	}
	return &directive, nil
}

// ---------------------------------------------------------------------------
// Planning (SPEC §3.1 phase 7)

// ---------------------------------------------------------------------------
// One step's (or one resumed step's) entries

// stepToolRun holds the entries of one step while the executor works on them.
type stepToolRun struct {
	x       *ClaimToolExecutor
	in      agentrun.StepToolsInput
	binding *agentrun.TurnRunnerBinding
	// step produced the entries; current is the claim's running step (the
	// TurnID of new governed calls and the nested VM fence of host calls).
	step    *models.AgentTurn
	current *models.AgentTurn
	// narration is the step's assistant text.
	narration string

	entries []*models.SessionEntry
	tools   []resolvedLoopTool
	guard   *loopRepeatGuard

	mu        sync.Mutex
	chain     loopRunChain
	batch     bool
	batchRuns []*models.AgentRun
	governed  []GovernedToolOutcome // awaiting governed outcomes, in call order
	held      bool                  // a call of the step reached awaiting_* (or waits for a foreign call lease)
	retryAt   *time.Time
	park      *agentrun.LoopPark // a control entry's (non-governed) park
	final     *agentrun.FinalAnswer
	failure   *agentrun.LoopFailure
	budget    *agentrun.BudgetExhaustion
	ended     bool
	hostCalls int
	searches  int
	setupIdx  []int // governed skill-management entries that may need the setup note

	setOnce  sync.Once
	set      *modelstep.CapabilitySet
	setErr   error
	setMu    sync.Mutex // ExecuteLoadCapabilities updates the set in place
	viewOnce sync.Once
	views    []control.GovernedToolView
	viewErr  error
}

func (x *ClaimToolExecutor) newStepToolRun(in agentrun.StepToolsInput, step *models.AgentTurn, entries []*models.SessionEntry) (*stepToolRun, error) {
	if in.Run == nil || in.Binding == nil {
		return nil, errors.New("step tools need the Run and its binding")
	}
	if in.Run.Status != models.AgentRunStatusRunning || in.WorkerID == "" || in.Run.LeaseOwner != in.WorkerID {
		return nil, models.ErrLeaseLost
	}
	r := &stepToolRun{
		x: x, in: in, binding: in.Binding, step: step, current: in.Step,
		entries: make([]*models.SessionEntry, len(entries)), tools: make([]resolvedLoopTool, len(entries)),
		chain: loopRunChain{known: in.Run},
	}
	if r.current == nil {
		r.current = step
	}
	if in.Assistant != nil && in.Assistant.Assistant != nil && in.Assistant.StepID == control.StepIDOf(step) {
		r.narration = in.Assistant.Assistant.Text
	}
	for i, entry := range entries {
		if entry == nil || entry.Type != models.SessionEntryTool || entry.Tool == nil || entry.RunID != in.Run.ID {
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

func (r *stepToolRun) run() *models.AgentRun {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.chain.known
}

// input is the step input with the Run as the executor's writes left it.
func (r *stepToolRun) input() agentrun.StepToolsInput {
	in := r.in
	in.Run = r.run()
	if r.step != nil {
		in.Step = r.step
	}
	return in
}

func (r *stepToolRun) lease() models.AgentRunLeaseGuard {
	return models.AgentRunLeaseGuard{WorkerID: r.in.WorkerID, Now: r.x.now()}
}

func (r *stepToolRun) sessionID() string {
	if r.in.Session != nil && r.in.Session.ID != "" {
		return r.in.Session.ID
	}
	return r.in.Run.SessionID
}

// adoptRun follows the Run through the executor's own writes. Inside a
// concurrent batch the writes are collected and adopted by endBatch.
func (r *stepToolRun) adoptRun(run *models.AgentRun) {
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
	known  *models.AgentRun
	broken bool
}

func (c *loopRunChain) adopt(run *models.AgentRun) {
	if c.broken || run == nil || c.known == nil || run.Revision <= c.known.Revision {
		return
	}
	c.known = run
}

func (c *loopRunChain) adoptBatch(runs []*models.AgentRun) {
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

func (r *stepToolRun) setEntry(i int, entry *models.SessionEntry) {
	r.mu.Lock()
	r.entries[i] = entry
	r.mu.Unlock()
	if r.guard != nil {
		r.guard.Observe(entry)
	}
}

func (r *stepToolRun) entry(i int) *models.SessionEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.entries[i]
}

// transition moves entry i to to. executed marks the transition of a call
// that ran (D9 result digest, Parked when it leaves an awaiting state).
func (r *stepToolRun) transition(ctx context.Context, i int, to models.ToolEntryState, result *models.ToolResult, wait *models.ToolEntryWait, actionCallID string, kernel *models.ToolEntryKernelPatch, executed bool) error {
	entry := r.entry(i)
	if actionCallID == entry.ActionCallID {
		actionCallID = ""
	}
	lease := r.lease()
	req := models.ToolEntryTransitionRequest{
		Scope: r.in.Run.Scope, EntryID: entry.ID, RunID: r.in.Run.ID, Lease: lease,
		From: entry.State, ExpectedRevision: entry.Revision, To: to,
		ActionCallID: actionCallID, Wait: wait, Result: result, Kernel: kernel, Now: lease.Now,
	}
	if result != nil {
		req.Tokens = modelstep.EstimateEntryTokens(control.WithToolResult(entry, to, result))
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
func (r *stepToolRun) adopted(ctx context.Context, i int, entry *models.SessionEntry) error {
	if entry == nil {
		return nil
	}
	r.setEntry(i, entry)
	return r.afterTool(ctx, entry)
}

func (r *stepToolRun) afterTool(ctx context.Context, entry *models.SessionEntry) error {
	if entry == nil || !entry.State.Terminal() || r.binding.AfterTool == nil {
		return nil
	}
	if err := r.binding.AfterTool(ctx, r.input(), entry); err != nil {
		return fmt.Errorf("after tool entry %s: %w", entry.ToolCallID, err)
	}
	return nil
}

// refuse ends a call that does not run with an is_error result.
func (r *stepToolRun) refuse(ctx context.Context, i int, result *models.ToolResult, guard *models.ToolGuardOutcome) error {
	var kernel *models.ToolEntryKernelPatch
	if guard != nil {
		kernel = &models.ToolEntryKernelPatch{Guard: guard}
	}
	return r.transition(ctx, i, models.ToolEntryError, result, nil, "", kernel, false)
}

// ---------------------------------------------------------------------------
// Pre-dispatch checks (phase rules, offered tools, ToolCheck)

// capabilities is the step's capability set: the claim's catalog with the
// loads of the path the step started from. Loads in this step are callable
// in the next step.
func (r *stepToolRun) capabilities(ctx context.Context) (*modelstep.CapabilitySet, error) {
	r.setOnce.Do(func() {
		path := r.in.Path
		if path == nil && r.x.cfg.Sessions != nil && r.sessionID() != "" {
			path, r.setErr = r.x.cfg.Sessions.LoadAgentSessionPath(ctx, r.in.Run.Scope, r.sessionID())
			if r.setErr != nil {
				return
			}
		}
		r.set = modelstep.NewPathCapabilitySet(LoopCapabilityCatalog(r.in.Run, r.binding, r.x.claim), path, r.x.claim.CapabilityConfig)
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
		return set.Loaded(models.CapabilityActionPrefix + tool.Action.Name), nil
	case loopRouteHost:
		return set.Loaded(modelstep.CapabilityWorkspacePrefix + tool.Host.Operation), nil
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
		refusals := control.CallRefusals(r.in.Assistant.Assistant.ToolCalls, authority)
		for i, entry := range r.entries {
			if result := refusals[entry.ToolCallID]; result != nil && entry.State == models.ToolEntryPending {
				if err := r.refuse(ctx, i, result, nil); err != nil {
					return err
				}
			}
		}
	}
	for i := range r.entries {
		entry := r.entry(i)
		if entry.State != models.ToolEntryPending || entry.ActionCallID != "" {
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
				result = control.ErrorResult(models.ToolResultRefused, loopToolRefusedCode, strings.TrimSpace(refusal), nil)
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
func (r *stepToolRun) offerRefusal(ctx context.Context, i int) (*models.ToolResult, error) {
	entry, tool := r.entry(i), r.tools[i]
	kernel := len(entry.Tool.Directive) > 0
	notAvailable := func() *models.ToolResult {
		return control.ErrorResult(models.ToolResultRefused, loopToolNotAvailableCode,
			"This tool is not available in this conversation.", map[string]interface{}{"tool": entry.Tool.Name})
	}
	switch tool.Route {
	case loopRouteUnknown:
		return control.ErrorResult(models.ToolResultRefused, loopToolNotAvailableCode,
			fmt.Sprintf("No tool named %q is available here.", entry.Tool.Name), map[string]interface{}{"tool": entry.Tool.Name}), nil
	case loopRouteControl:
		if !kernel && (r.x.cfg.Control == nil || !modelstep.ControlToolOffered(tool.Control, r.binding.Authority)) {
			return notAvailable(), nil
		}
		if r.x.cfg.Control == nil {
			return notAvailable(), nil
		}
	case loopRouteCapability:
		if !r.binding.Authority.OffersCapabilities() {
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
			return control.ErrorResult(models.ToolResultRefused, loopToolNotLoadedCode,
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
		WaitOffered: modelstep.ControlToolOffered(models.ControlToolWait, r.binding.Authority), Now: r.x.cfg.Now,
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
		DeploymentID: runtimeaction.DeploymentForSelection(r.binding, action),
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
	if entry.State != models.ToolEntryPending || r.tools[i].settle() {
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
	return r.refuse(ctx, i, control.ErrorResult(models.ToolResultRefused, loopToolNotAvailableCode,
		fmt.Sprintf("No tool named %q is available here.", entry.Tool.Name), nil), nil)
}

func (r *stepToolRun) runHost(ctx context.Context, i int) error {
	entry, spec := r.entry(i), r.tools[i].Host
	if r.x.cfg.Host == nil || r.x.claim.Workspace == nil {
		return r.refuse(ctx, i, modelstep.HostToolErrorResult(models.ToolResultFailed, modelstep.HostToolUnsupportedMessage), nil)
	}
	if entry.State != models.ToolEntryRunning {
		if err := r.transition(ctx, i, models.ToolEntryRunning, nil, nil, "", nil, false); err != nil {
			return err
		}
		entry = r.entry(i)
	}
	outcome, ok := r.x.cfg.Host.Run(ctx, modelstep.HostToolCall{
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
		return models.ErrLeaseLost
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
	case modelstep.LoadCapabilitiesTool:
		content, isError, loaded = modelstep.ExecuteLoadCapabilities(set, args, loopCapabilityToolName(r.binding.ModelActions))
	default:
		searched := 0
		if r.in.Path != nil {
			searched = modelstep.CapabilitySearchesThisRun(r.in.Path.Entries, r.in.Run.ID)
		}
		content, isError = modelstep.ExecuteSearchCapabilities(set, args, searched+r.searches)
		if !isError {
			r.searches++
		}
	}
	r.setMu.Unlock()
	status, to := models.ToolResultSucceeded, models.ToolEntryCompleted
	if isError {
		status, to = models.ToolResultFailed, models.ToolEntryError
	}
	var kernel *models.ToolEntryKernelPatch
	if len(loaded) > 0 {
		kernel = loadKernelPatch(set, r.binding.ModelActions, loaded)
	}
	result := &models.ToolResult{Status: status, IsError: isError, Content: []models.ContentPart{{Type: models.ContentPartJSON, Value: content}}}
	return r.transition(ctx, i, to, result, nil, "", kernel, true)
}

// loadKernelPatch freezes what a load made visible on its tool entry: the
// definition of each new tool exactly as later steps declare it, and the IDs
// of the instructions and runbooks its result carried.
func loadKernelPatch(set *modelstep.CapabilitySet, actions []capability.ModelAction, loaded []string) *models.ToolEntryKernelPatch {
	names := modelstep.LoopGovernedToolNames(actions)
	patch := &models.ToolEntryKernelPatch{}
	for _, id := range loaded {
		if action, ok := set.CatalogAction(id); ok {
			patch.SurfacedTools = append(patch.SurfacedTools, modelstep.NewSurfacedTool(id, governedModelStepTool(action, names[action.Name], models.ModelStepToolLoaded)))
			continue
		}
		if spec, ok := set.CatalogHostTool(id); ok {
			patch.SurfacedTools = append(patch.SurfacedTools, modelstep.NewSurfacedTool(id, hostModelStepTool(spec, models.ModelStepToolLoaded)))
			continue
		}
		patch.LoadedInstructions = append(patch.LoadedInstructions, id)
	}
	return patch
}

func (r *stepToolRun) controlContext(i int) agentrun.ControlContext {
	in := r.input()
	if r.in.Assistant == nil || r.in.Assistant.StepID != control.StepIDOf(r.step) {
		in.Assistant = nil
	}
	return control.NewContext(in, r.entry(i))
}

func (r *stepToolRun) runControlInline(ctx context.Context, i int) error {
	if r.x.cfg.Control == nil {
		return r.refuse(ctx, i, control.ErrorResult(models.ToolResultRefused, loopToolNotAvailableCode,
			"This tool is not available in this conversation.", nil), nil)
	}
	result, err := r.x.cfg.Control.Inline(ctx, r.controlContext(i))
	if err != nil {
		return err
	}
	return r.applyControl(ctx, i, result)
}

// applyControl records a control outcome on the step.
func (r *stepToolRun) applyControl(ctx context.Context, i int, result control.Result) error {
	r.adoptRun(result.Run)
	if err := r.adopted(ctx, i, result.Entry); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	switch result.Kind {
	case control.Parked:
		park := *result.Park
		r.park, r.held = &park, true
	case control.Final:
		r.final = result.Final
	case control.Ended:
		r.ended = true
	case control.Failed:
		r.failure = result.Failure
	}
	return nil
}

// ---------------------------------------------------------------------------
// End of a step's tool work

// finishGuard appends the step's D9 notes and setup notes once every entry is
// terminal and returns the guard's Run failure (repeat_limit, error_streak).
func (r *stepToolRun) finishGuard(ctx context.Context) (*agentrun.LoopFailure, error) {
	for i := range r.entries {
		if !r.entry(i).State.Terminal() {
			return nil, nil
		}
	}
	var failure *agentrun.LoopFailure
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
		note := control.PendingSkillSetupNote(run, r.sessionID(), entry, call, run.PendingInterventions)
		if note == nil {
			continue
		}
		note.Tokens = modelstep.EstimateEntryTokens(note)
		lease := r.lease()
		if _, err := r.x.cfg.Sessions.AppendAgentSessionEntries(ctx, models.AppendAgentSessionEntriesRequest{
			Scope: run.Scope, SessionID: r.sessionID(), RunID: run.ID, Lease: &lease,
			Entries: []*models.SessionEntry{note}, Now: lease.Now,
		}); err != nil {
			return fmt.Errorf("append setup note: %w", err)
		}
	}
	return nil
}

// outcome is the StepToolsOutcome of the work done so far.
func (r *stepToolRun) outcome() agentrun.StepToolsOutcome {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := agentrun.StepToolsOutcome{
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
func toolOutputInvocation(run *models.AgentRun, name, skillID, action, actionCallID string) models.ToolInvocation {
	invocation := models.ToolInvocation{Name: name, SkillID: skillID, Action: action, ActionCallID: actionCallID}
	if run != nil {
		invocation.RunID = run.ID
		invocation.Scope = skill.ScopeReference{Kind: run.Scope.Kind, ID: run.Scope.ID}
		invocation.DeploymentID, invocation.ExecutionDeploymentID = run.Owner.ID, run.AssignedAgentID
	}
	return invocation
}
