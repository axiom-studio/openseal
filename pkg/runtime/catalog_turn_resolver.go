package runtime

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/axiom-studio/openseal/pkg/skill"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
	"github.com/axiom-studio/openseal/pkg/workspace"
	"go.uber.org/zap"
)

// AgentTurnCatalog is the portable, product-neutral catalog required to bind
// a durable Run to its immutable Agent definition and activated Skills.
type AgentTurnCatalog interface {
	GetAgentDeployment(context.Context, skill.ScopeReference, string) (*kernelagent.AgentDeployment, error)
	ListAgentDeployments(context.Context, kernelagent.AgentDeploymentFilter) ([]*kernelagent.AgentDeployment, error)
	GetAgentDefinition(context.Context, string, string) (*kernelagent.AgentDefinition, error)
	ActivateSkills(context.Context, skill.ScopeReference, string, skill.HostCapabilityState) (*skill.ActivationSnapshot, error)
	// ActionCallReader lets deterministic producers (capability invocation)
	// read the authoritative ActionCall behind their tool entry.
	ActionCallReader
}

// LoopBindingConfig wires the kernel services every catalog loop binding uses.
// One config serves every claim of a process; ResolveLoopBinding adds what
// belongs to one claim (session, chat Workspace, authority, tools).
type LoopBindingConfig struct {
	// Host runs the model steps of model Runs, their compaction and their
	// progress acknowledgements. Without it only deterministic Runs bind;
	// a model Run is ErrTurnHostUnavailable.
	Host       StepHost
	SkillHost  skill.HostCapabilityState
	SkillHosts SkillHostCapabilityResolver
	// ConversationTasks is optional canonical storage for hosted task context.
	// A model or copied Run context cannot supply task admission authority.
	ConversationTasks ConversationTaskKernelStore
	// Store is the kernel store: the session log the Run writes to, the path
	// its model steps render and the governed calls they read. Required.
	Store KernelStore
	// Conversations resolves a conversation Run's origin (ask_user, approval
	// mode). Optional: without it those flags stay off.
	Conversations ConversationStore
	// Tools builds the claim's LoopToolExecutor. Required.
	Tools *StepToolDispatcher
	// ChatWorkspaces resolves the claim's chat Workspace, whose host tools
	// the Run is offered. nil: no Run is offered host tools.
	ChatWorkspaces ChatWorkspaces
	// Media are the image sources of the path renderer; Media.ImageInput also
	// decides whether the model step system carries the media section.
	Media        ModelStepMedia
	Capabilities CapabilitySetConfig
	Logger       *zap.SugaredLogger
	Now          func() time.Time
}

// SkillHostCapabilityResolver lets an embedding runtime project the exact
// adapters available to one deployment without duplicating Agent, Skill, and
// Turn resolution. The returned state is host-authoritative and never model
// writable.
type SkillHostCapabilityResolver interface {
	ResolveSkillHostCapabilities(context.Context, skill.ScopeReference, string) (*skill.HostCapabilityState, error)
}

type SkillHostCapabilityResolverFunc func(context.Context, skill.ScopeReference, string) (*skill.HostCapabilityState, error)

func (f SkillHostCapabilityResolverFunc) ResolveSkillHostCapabilities(ctx context.Context, scope skill.ScopeReference, deploymentID string) (*skill.HostCapabilityState, error) {
	return f(ctx, scope, deploymentID)
}

// ResolveLoopBinding binds a catalog Agent Run to its immutable Agent
// definition and activated Skills, and gives it the run loop seams of its
// kind (SPEC §2.4):
//
//   - AgentRequest decision and completion review Runs: a model producer
//     offered only their typed final tool; no Skill is activated.
//   - Typed capability invocations: their deterministic producer,
//     executing their tool calls through the claim's executor.
//   - Pinned runbook entrypoints: the runbook runner.
//   - Every other Run (Agent work, routines, conversation task work, fork
//     branches, accepted delegation execution, runbook-delegated children,
//     conversation replies): the model producer with the Run's authorized
//     catalog.
//
// The claim's session, chat Workspace and tool executor are resolved once
// here, so the advertised tools stay stable for the claim. Final is nil: the
// loop completes the Run with the final output. The conversation runner adds
// its own Final and hooks on top for chat replies.
func ResolveLoopBinding(ctx context.Context, catalog AgentTurnCatalog, run *AgentRun, config LoopBindingConfig) (*TurnRunnerBinding, error) {
	if catalog == nil {
		return nil, errors.New("canonical Agent turn catalog is unavailable")
	}
	if config.Store == nil || config.Tools == nil {
		return nil, errors.New("loop binding requires a kernel store and a tool dispatcher")
	}
	if run == nil || (run.Kind != RunKindAgentWork && run.Kind != RunKindConversation) || strings.TrimSpace(run.AssignedAgentID) == "" {
		return nil, errors.New("Agent work requires an assigned Agent")
	}
	scope := skill.ScopeReference{Kind: run.Scope.Kind, ID: run.Scope.ID}
	deployment, err := catalog.GetAgentDeployment(ctx, scope, run.AssignedAgentID)
	if err != nil || deployment == nil {
		return nil, fmt.Errorf("resolve Agent deployment %s: %w", run.AssignedAgentID, err)
	}
	if deployment.RolloutStatus != kernelagent.RolloutActive || strings.TrimSpace(deployment.ActiveVersion) == "" {
		return nil, errors.New("assigned Agent deployment is not active")
	}
	definitionID, definitionVersion := deployment.DefinitionID, deployment.ActiveVersion
	accepted, err := AcceptedRunExecutionForRun(run)
	if err != nil {
		return nil, err
	}
	if accepted != nil {
		definitionID, definitionVersion = accepted.DefinitionID, accepted.DefinitionVersion
	}
	definition, err := catalog.GetAgentDefinition(ctx, definitionID, definitionVersion)
	if err != nil || definition == nil {
		if accepted != nil || strings.TrimSpace(run.Entrypoint) != "" {
			return nil, fmt.Errorf("%w: immutable Agent definition is unavailable", ErrAcceptedRunExecution)
		}
		return nil, fmt.Errorf("resolve active Agent definition: %w", err)
	}
	if accepted != nil && (definition.ID != accepted.DefinitionID || definition.Version != accepted.DefinitionVersion || definition.Runbook == nil || definition.Runbook.ID != accepted.RunbookID || definition.Runbook.Version != accepted.RunbookVersion) {
		return nil, ErrAcceptedRunExecution
	}
	r := &loopBindingResolution{config: config, catalog: catalog, run: run, scope: scope, deployment: deployment, definition: definition}
	if trial, ok, trialErr := agentTrialContextForRun(run); trialErr != nil {
		return nil, trialErr
	} else if ok {
		// A try_agent trial speaks as the drafted profile under the
		// builder's identity, with no tools (spec-openseal §3.3).
		return r.agentTrial(ctx, trial)
	}
	if runExplainingFailure(run) {
		// The explanation step is tool-free: no Skill is activated or
		// prepared, so a broken Skill cannot block the answer.
		r.explaining = true
		return r.model(ctx, r.identity(deployment.ID), nil, PromptContribution{}, false)
	}
	for _, structured := range []struct{ key, instruction string }{
		{AgentRequestCompletionReviewContextKey, agentRequestCompletionReviewSystemInstruction},
		{AgentRequestInboxContextKey, agentRequestDecisionSystemInstruction},
	} {
		if _, requested := run.Context[structured.key]; !requested {
			continue
		}
		// Decision and review Runs act for the request, not the Agent's
		// catalog: no Skill is activated, and they are offered only their
		// typed final tool (ResolveControlAuthority).
		binding := r.identity(deployment.ID)
		binding.InputContextRefs = []string{"run:" + structured.key}
		return r.model(ctx, binding, nil, PromptContribution{AgentInstructions: []string{structured.instruction}}, false)
	}
	skillHost := config.SkillHost
	if config.SkillHosts != nil {
		resolved, resolveErr := config.SkillHosts.ResolveSkillHostCapabilities(ctx, scope, deployment.ID)
		if resolveErr != nil {
			return nil, fmt.Errorf("resolve Skill execution host for Agent %s: %w", deployment.ID, resolveErr)
		}
		if resolved == nil {
			return nil, fmt.Errorf("resolve Skill execution host for Agent %s: empty capability state", deployment.ID)
		}
		skillHost = *resolved
	}
	// Connections belong to the run's umbrella. A 1:1 run activates its
	// Agent with the host's bindings for that Agent. A Team run activates the
	// member without any connection and the Team with the Team's own. An
	// Agent-owned run delegated to another Agent (a request, a completion
	// review) activates that Agent without any connection: it never lends
	// its own.
	actorCtx := skill.WithRunActor(ctx, RunSkillActor(run))
	memberCtx := actorCtx
	teamRun := run.Owner.Type == OwnerTypeTeam && strings.TrimSpace(run.Owner.ID) != ""
	delegatedRun := run.Owner.Type == OwnerTypeAgent && run.Owner.ID != run.AssignedAgentID
	if teamRun || delegatedRun {
		memberCtx = skill.WithoutConnections(actorCtx)
	}
	activation, err := catalog.ActivateSkills(memberCtx, scope, deployment.ID, skillHost)
	if err != nil {
		return nil, fmt.Errorf("activate bound Skills for Agent %s: %w", deployment.ID, err)
	}
	if activation == nil || strings.TrimSpace(activation.SnapshotID) == "" {
		return nil, errors.New("Skill activation returned no immutable snapshot")
	}
	activation, err = restrictActivatedSkillsToRun(activation, run.Context)
	if err != nil {
		return nil, fmt.Errorf("apply Runbook Skill authority: %w", err)
	}
	prompts, actions, prepared, contextRefs, err := projectActivatedSkills(activation)
	if err != nil {
		return nil, fmt.Errorf("project bound Skills for Agent %s: %w", deployment.ID, err)
	}
	actionDeploymentID := deployment.ID
	if teamRun {
		teamActivation, activateErr := catalog.ActivateSkills(actorCtx, scope, run.Owner.ID, skillHost)
		if activateErr != nil {
			return nil, fmt.Errorf("activate bound Skills for Team %s: %w", run.Owner.ID, activateErr)
		}
		if teamActivation == nil || strings.TrimSpace(teamActivation.SnapshotID) == "" {
			return nil, errors.New("Team Skill activation returned no immutable snapshot")
		}
		teamActivation, activateErr = restrictActivatedSkillsToRun(teamActivation, run.Context)
		if activateErr != nil {
			return nil, fmt.Errorf("apply Team Runbook Skill authority: %w", activateErr)
		}
		authorityCatalog, ok := catalog.(TeamSkillAuthorityCatalog)
		if !ok {
			return nil, errors.New("Team Skill authority resolution is unavailable")
		}
		teamActivation, activateErr = AuthorizeTeamSkillActivation(ctx, authorityCatalog, run, teamActivation)
		if activateErr != nil {
			return nil, fmt.Errorf("authorize bound Skills for Team %s: %w", run.Owner.ID, activateErr)
		}
		teamPrompts, teamActions, teamPrepared, teamContextRefs, projectErr := projectActivatedSkills(teamActivation)
		if projectErr != nil {
			return nil, fmt.Errorf("project bound Skills for Team %s: %w", run.Owner.ID, projectErr)
		}
		mergedActions, shadowedBindings, mergeErr := mergeOwnerModelActions(actions, teamActions)
		if mergeErr != nil {
			return nil, mergeErr
		}
		if len(shadowedBindings) > 0 {
			filtered := prepared[:0]
			for _, runtime := range prepared {
				if _, shadowed := shadowedBindings[ownerBindingKey(runtime.DeploymentID, runtime.BindingID)]; !shadowed {
					filtered = append(filtered, runtime)
				}
			}
			prepared = filtered
		}
		prompts = append(prompts, teamPrompts...)
		actions = mergedActions
		prepared = append(prepared, teamPrepared...)
		contextRefs = append(contextRefs, teamContextRefs...)
	}
	runbookOperations := projectCallableRunbookOperations(definition.Runbook)
	if len(runbookOperations) > 0 {
		actions = withoutRunbookActivationStart(actions)
	}
	if accepted != nil {
		allowed := make(map[string]bool, len(accepted.SkillDependencies))
		for _, dependency := range accepted.SkillDependencies {
			allowed[dependency.SkillID+"\x00"+dependency.SkillVersion] = true
		}
		filtered := make([]capability.ModelAction, 0, len(actions))
		for _, action := range actions {
			if allowed[action.SkillID+"\x00"+action.Version] {
				filtered = append(filtered, action)
			}
		}
		actions = filtered
	}
	actions = withoutUnusableTargetAgent(activation, actions)
	base := r.identity(actionDeploymentID)
	base.ModelActions, base.PreparedRuntimes, base.InputContextRefs = actions, prepared, contextRefs
	base.RunbookOperations = runbookOperations
	if delegatedFromCurrentRunbook(run, definition.Runbook) {
		base.RunbookOperations = nil
	}
	if _, requested := run.Context["capabilityInvocation"]; requested {
		producer, resolveErr := NewCapabilityInvocationStepProducer(actions, catalog)
		if resolveErr != nil {
			return nil, fmt.Errorf("resolve typed capability invocation for Agent %s: %w", deployment.ID, resolveErr)
		}
		base.Producer = producer
		base.InputContextRefs = append(base.InputContextRefs, "run:capabilityInvocation")
		return r.deterministic(ctx, base)
	}
	delegationMode, _ := run.Context[DelegationModeContextKey].(string)
	if strings.TrimSpace(run.Entrypoint) != "" && delegationMode != string(runbook.DelegateReason) {
		if definition.Runbook == nil || (accepted == nil && legacyAcceptedRunExecution(run, deployment, definition) == nil) {
			return nil, ErrAcceptedRunExecution
		}
		if err := validatePinnedRunbookPlan(definition.Runbook, run); err != nil {
			return nil, fmt.Errorf("%w: resolve governed runbook for Agent %s: %v", ErrAcceptedRunExecution, deployment.ID, err)
		}
		entrypoint := catalogRunbookEntrypoint(definition, run)
		producer, resolveErr := NewRunbookStepProducer(definition.Runbook, entrypoint, catalog)
		if resolveErr != nil {
			return nil, fmt.Errorf("%w: resolve governed runbook for Agent %s: %v", ErrAcceptedRunExecution, deployment.ID, resolveErr)
		}
		base.Producer = producer
		base.InputContextRefs = append(base.InputContextRefs, "runbook:"+definition.Runbook.ID+"@"+definition.Runbook.Version)
		return r.deterministic(ctx, base)
	}
	var prompt PromptContribution
	if run.Source == RunSourceRequest || run.Source == RunSourceHandoff || run.Source == RunSourceEscalation {
		if _, ok := run.Context["collaboration"]; ok {
			prompt.AgentInstructions = append(prompt.AgentInstructions, acceptedAgentRequestExecutionSystemInstruction)
		}
	}
	if delegated, ok := run.Context["delegatedSystemPrompt"].(string); ok && strings.TrimSpace(delegated) != "" {
		prompt.AgentInstructions = append(prompt.AgentInstructions, "Delegated execution instructions: "+strings.TrimSpace(delegated))
	}
	for _, action := range actions {
		if action.SkillID == RunbookManagementSkillID && action.Action == RunbookActionCreateTask {
			prompt.AgentInstructions = append(prompt.AgentInstructions, RoutineTimezoneInstruction, RoutineDeliveryInstruction)
			if instruction := routineBindingToolGrantInstruction(actions); instruction != "" {
				prompt.AgentInstructions = append(prompt.AgentInstructions, instruction)
			}
			break
		}
	}
	return r.model(ctx, base, prompts, prompt, true)
}

// loopBindingResolution is one ResolveLoopBinding call.
type loopBindingResolution struct {
	config     LoopBindingConfig
	catalog    AgentTurnCatalog
	run        *AgentRun
	scope      skill.ScopeReference
	deployment *kernelagent.AgentDeployment
	definition *kernelagent.AgentDefinition
	// explaining is a Run whose only remaining step is the tool-free
	// failure explanation: it is offered no control tool either.
	explaining bool
	// trial is a try_agent child: offered only the final answer.
	trial bool
}

func (r *loopBindingResolution) now() time.Time {
	if r.config.Now != nil {
		return r.config.Now().UTC()
	}
	return time.Now().UTC()
}

// identity is the Agent identity every binding of the Run carries.
func (r *loopBindingResolution) identity(actionDeploymentID string) *TurnRunnerBinding {
	return &TurnRunnerBinding{
		DeploymentID: r.deployment.ID, ActionDeploymentID: actionDeploymentID,
		DefinitionID: r.definition.ID, DefinitionVersion: r.definition.Version,
		BudgetReservation: BudgetUsage{Turns: 1},
		ModelCredential:   deploymentModelCredential(r.deployment),
	}
}

// model completes binding for a model-driven Run: the claim's session, chat
// Workspace (when workspace is true), control authority, the model step
// producer over the catalog preparer, its compactor, the run-start preparer,
// the tool executor and the progress acknowledgement renderer.
func (r *loopBindingResolution) model(ctx context.Context, binding *TurnRunnerBinding, prompts []HostedSkillPrompt, prompt PromptContribution, workspaceTools bool) (*TurnRunnerBinding, error) {
	if r.config.Host == nil {
		return nil, ErrTurnHostUnavailable
	}
	session, err := r.session(ctx, true)
	if err != nil {
		return nil, err
	}
	var chat *workspace.Authority
	if workspaceTools {
		chat = r.workspace(ctx)
	}
	hostTools := HostToolSpecs(chat)
	script, err := prepareLoopRoutineScript(r.run, hostTools)
	if err != nil {
		return nil, err
	}
	refs := make([]string, 0, len(prompts))
	for _, skillPrompt := range prompts {
		refs = append(refs, skillPrompt.Reference)
	}
	binding.Authority, err = ResolveControlAuthority(ctx, ControlAuthorityInput{
		Run: r.run, DeploymentID: r.deployment.ID, Catalog: r.catalog, Scope: r.scope,
		SkillPromptRefs: refs, Operations: binding.RunbookOperations,
		ConversationTasks: r.config.ConversationTasks, Runs: r.config.Store, Conversations: r.config.Conversations,
		Deterministic: r.explaining || r.trial,
	})
	if err != nil {
		return nil, err
	}
	binding.Host = r.config.Host
	binding.Session = session
	binding.Prompt = prompt
	instructions := hostedAgentInstructions(r.definition)
	preparerConfig := catalogModelStepPreparerConfig{
		AgentInstructions: instructions, SkillPrompts: prompts, HostTools: hostTools,
		ModelCredential: deploymentModelCredential(r.deployment),
		Capabilities:    r.config.Capabilities,
		Render:          sessionRenderOptions{Artifacts: r.config.Media.Artifacts, Content: r.config.Media.Content, Actions: r.config.Store},
	}
	if script != nil {
		preparerConfig.RoutineScript = script.Section().Text
	}
	if r.config.Media.ImageInput != nil {
		preparerConfig.ImageInput = r.config.Media.ImageInput(ctx, StepInput{Run: r.run, Session: session, Binding: binding}, ModelStepRequest{
			Scope: r.run.Scope, RunID: r.run.ID, AgentID: r.deployment.ID,
			DefinitionID: binding.DefinitionID, DefinitionVersion: binding.DefinitionVersion,
			ModelCredential: preparerConfig.ModelCredential, Model: binding.Model,
		})
	}
	preparer := newCatalogModelStepPreparer(preparerConfig)
	producer, err := NewModelStepProducer(r.config.Host, preparer, r.config.Store)
	if err != nil {
		return nil, err
	}
	producer.Media = r.config.Media
	producer.Now = r.config.Now
	binding.Producer = producer
	binding.Compactor = &LoopCompactor{
		Host: r.config.Host, Sessions: r.config.Store, Requests: preparer,
		Budget: newRunActivityCompactionBudget(NewRunActivityService(r.config.Store, r.config.Store), r.config.Now),
		Now:    r.config.Now,
	}
	binding.Preparers = []StepPreparer{newRunStartPreparer(r.config.Store, r.config.Store)}
	binding.Tools = r.config.Tools.ForClaim(LoopToolClaim{
		Workspace: chat, SkillPrompts: prompts, CapabilityConfig: r.config.Capabilities, CommandPins: script.CommandPins(),
	})
	binding.ProgressAcknowledgementRenderer = &modelStepProgressAcknowledgementRenderer{
		host: r.config.Host, agentID: r.deployment.ID,
		definitionID: binding.DefinitionID, definitionVersion: binding.DefinitionVersion,
		modelCredential: preparerConfig.ModelCredential,
		system:          append(append(append([]string(nil), instructions...), prompt.AgentInstructions...), runProgressAcknowledgementSystemInstruction),
	}
	return binding, nil
}

// deterministic completes binding for a deterministic producer: a run-owned
// session that holds only its tool and note entries (A3), no model control
// tools, and the tool executor without host tools.
func (r *loopBindingResolution) deterministic(ctx context.Context, binding *TurnRunnerBinding) (*TurnRunnerBinding, error) {
	session, err := r.session(ctx, false)
	if err != nil {
		return nil, err
	}
	binding.Authority, err = ResolveControlAuthority(ctx, ControlAuthorityInput{Run: r.run, DeploymentID: r.deployment.ID, Deterministic: true})
	if err != nil {
		return nil, err
	}
	binding.Host = r.config.Host
	binding.Session = session
	binding.Tools = r.config.Tools.ForClaim(LoopToolClaim{CapabilityConfig: r.config.Capabilities})
	return binding, nil
}

// claimed reports whether the Run being resolved is a live claim. A
// resolution outside a claim (a progress acknowledgement's synthetic Run)
// writes nothing: no session is created and no Workspace prepared.
func (r *loopBindingResolution) claimed() bool {
	return r.run.Status == AgentRunStatusRunning && strings.TrimSpace(r.run.LeaseOwner) != ""
}

// session is the session the Run writes to. A Run bound to one (its
// conversation lane, or a same-Agent child created with its session) keeps
// it. Any other Run gets its parentless run session at its first claim (A3):
// a model Run's is seeded with its brief, a deterministic Run's stays empty.
func (r *loopBindingResolution) session(ctx context.Context, seeded bool) (*AgentSession, error) {
	run := r.run
	if run.SessionID != "" {
		return conversationRunSession(ctx, r.config.Store, run)
	}
	if !r.claimed() {
		return nil, nil
	}
	var initial []*SessionEntry
	if seeded {
		seed, err := loopRunSessionSeed(run)
		if err != nil {
			return nil, err
		}
		if seed != nil {
			initial = []*SessionEntry{seed}
		}
	}
	conversationID, _ := run.Context[conversationRunContextConversationID].(string)
	threadRootID, _ := run.Context["threadRootMessageId"].(string)
	now := r.now()
	opened, err := r.config.Store.CreateRunAgentSession(ctx, CreateRunAgentSessionRequest{
		Scope: run.Scope, OwnerRunID: run.ID, AgentID: r.deployment.ID,
		ConversationID: strings.TrimSpace(conversationID), ThreadRootID: strings.TrimSpace(threadRootID),
		Lease:          AgentRunLeaseGuard{WorkerID: run.LeaseOwner, Now: now},
		InitialEntries: initial, Now: now,
	})
	if err != nil {
		return nil, fmt.Errorf("create run session: %w", err)
	}
	return opened.Session, nil
}

// loopRunSessionSeed is the first entry of a model Run's fresh run session:
// a control-tool child's brief (task, fork branch, decision, review,
// delegation), otherwise the Run's goal as a routine brief. nil when the Run
// has no goal.
func loopRunSessionSeed(run *AgentRun) (*SessionEntry, error) {
	if _, trial := run.Context[AgentTrialContextKey]; trial {
		return agentTrialSessionSeed(run)
	}
	seed, err := controlChildSeed(run)
	if err != nil || seed != nil {
		return seed, err
	}
	if strings.TrimSpace(run.Goal) == "" {
		return nil, nil
	}
	return childSessionSeed(run.Scope, run.ID, "routine", run.Goal)
}

// workspace is the claim's chat Workspace, nil outside a claim, without a
// resolver or when the Run works in no chat. A resolver error runs the claim
// without host tools.
func (r *loopBindingResolution) workspace(ctx context.Context) *workspace.Authority {
	if !r.claimed() {
		return nil
	}
	authority, err := prepareChatWorkspace(ctx, r.config.ChatWorkspaces, r.run)
	if err != nil {
		if r.config.Logger != nil {
			r.config.Logger.Warnw("chat workspace is unavailable; the claim runs without host tools", "runId", r.run.ID, "error", err)
		}
		return nil
	}
	return authority
}

const authorizedSkillCatalogIDsContextKey = "authorizedSkillCatalogIds"

// restrictActivatedSkillsToRun enforces the immutable capability envelope
// compiled onto a delegated Runbook step. Conversation providers used for
// approval delivery remain active for the runtime transport worker without
// becoming model-callable message or polling tools.
func restrictActivatedSkillsToRun(activation *skill.ActivationSnapshot, context map[string]interface{}) (*skill.ActivationSnapshot, error) {
	if activation == nil {
		return nil, errors.New("Skill activation is required")
	}
	raw, present := context[authorizedSkillCatalogIDsContextKey]
	if !present {
		return activation, nil
	}
	allowed := map[string]bool{}
	switch values := raw.(type) {
	case []string:
		for _, value := range values {
			value = strings.TrimSpace(value)
			if value == "" {
				return nil, errors.New("authorized Skill catalog IDs must be non-empty")
			}
			allowed[value] = true
		}
	case []interface{}:
		for _, item := range values {
			value, ok := item.(string)
			value = strings.TrimSpace(value)
			if !ok || value == "" {
				return nil, errors.New("authorized Skill catalog IDs must be strings")
			}
			allowed[value] = true
		}
	default:
		return nil, errors.New("authorized Skill catalog IDs must be an array")
	}
	filtered := make([]skill.ActivatedSkill, 0, len(activation.Skills))
	for _, activated := range activation.Skills {
		if allowed[activated.SkillID] || (strings.TrimSpace(activated.SourceIdentity) != "" && allowed[activated.SourceIdentity]) {
			filtered = append(filtered, activated)
		}
	}
	copy := *activation
	copy.Skills = filtered
	return &copy, nil
}

func withoutRunbookActivationStart(actions []capability.ModelAction) []capability.ModelAction {
	filtered := make([]capability.ModelAction, 0, len(actions))
	for _, action := range actions {
		if action.SkillID == RunbookManagementSkillID && action.Action == RunbookActionStart {
			continue
		}
		filtered = append(filtered, action)
	}
	return filtered
}

func validatePinnedRunbookPlan(definition *runbook.Definition, run *AgentRun) error {
	if definition == nil || run == nil || len(run.Plan) == 0 {
		return nil
	}
	raw, present := run.Plan["runbook"]
	if !present {
		return nil
	}
	pin, ok := raw.(map[string]interface{})
	if !ok {
		return errors.New("pinned Runbook plan is malformed")
	}
	id, idOK := pin["id"].(string)
	version, versionOK := pin["version"].(string)
	triggerID, triggerOK := pin["trigger"].(string)
	if !idOK || !versionOK || !triggerOK || strings.TrimSpace(triggerID) == "" ||
		id != definition.ID || version != definition.Version {
		return errors.New("pinned Runbook identity is unavailable or stale")
	}
	trigger, ok := definition.Triggers[triggerID]
	if !ok || trigger.Entrypoint != strings.TrimSpace(run.Entrypoint) {
		return errors.New("pinned Runbook trigger is unavailable or stale")
	}
	return nil
}

func projectCallableRunbookOperations(definition *runbook.Definition) []HostedRunbookOperation {
	if definition == nil || len(definition.Interfaces) == 0 {
		return nil
	}
	entrypoints := make([]string, 0, len(definition.Interfaces))
	for entrypoint := range definition.Interfaces {
		entrypoints = append(entrypoints, entrypoint)
	}
	sort.Strings(entrypoints)
	operations := make([]HostedRunbookOperation, 0, len(entrypoints))
	for _, entrypoint := range entrypoints {
		contract := definition.Interfaces[entrypoint]
		operations = append(operations, HostedRunbookOperation{
			DefinitionID: definition.ID, DefinitionVersion: definition.Version,
			Entrypoint: entrypoint, Name: definition.Name + " · " + entrypoint,
			Description: contract.Description, InputSchema: cloneMap(contract.InputSchema), OutputSchema: cloneMap(contract.OutputSchema),
		})
	}
	return operations
}

func delegatedFromCurrentRunbook(run *AgentRun, definition *runbook.Definition) bool {
	if run == nil || definition == nil || run.Source != RunSourceRequest {
		return false
	}
	triggerInput, ok := run.Context["triggerInput"].(map[string]interface{})
	if !ok {
		return false
	}
	definitionID, _ := triggerInput["runbookDefinitionId"].(string)
	definitionVersion, _ := triggerInput["runbookDefinitionVersion"].(string)
	return strings.TrimSpace(definitionID) == definition.ID && strings.TrimSpace(definitionVersion) == definition.Version
}

func resolveHostedAgentTargets(
	ctx context.Context,
	catalog AgentTurnCatalog,
	scope skill.ScopeReference,
	run *AgentRun,
	currentDeploymentID string,
) ([]HostedAgentTarget, error) {
	if run == nil || run.Owner.Type != OwnerTypeTeam || strings.TrimSpace(run.Owner.ID) == "" {
		return nil, nil
	}
	teamCatalog, ok := catalog.(TeamSkillAuthorityCatalog)
	if !ok {
		return nil, errors.New("Team delegation authority resolution is unavailable")
	}
	teamDeployment, err := teamCatalog.GetTeamDeployment(ctx, scope, run.Owner.ID)
	if err != nil {
		return nil, fmt.Errorf("resolve Team delegation roster: %w", err)
	}
	if teamDeployment == nil || teamDeployment.Status != kernelteam.DeploymentActive {
		return nil, errors.New("Team delegation roster is not active")
	}
	allowed := make(map[string]struct{}, len(teamDeployment.Roster))
	for _, assignment := range teamDeployment.Roster {
		if id := strings.TrimSpace(assignment.AgentDeploymentID); id != "" && id != currentDeploymentID {
			allowed[id] = struct{}{}
		}
	}
	if len(allowed) == 0 {
		return nil, nil
	}
	deployments, err := catalog.ListAgentDeployments(ctx, kernelagent.AgentDeploymentFilter{Scope: scope})
	if err != nil {
		return nil, err
	}
	targets := make([]HostedAgentTarget, 0, len(deployments))
	for _, candidate := range deployments {
		if candidate == nil || candidate.ID == currentDeploymentID ||
			candidate.RolloutStatus != kernelagent.RolloutActive || strings.TrimSpace(candidate.ActiveVersion) == "" {
			continue
		}
		if _, authorized := allowed[candidate.ID]; !authorized {
			continue
		}
		definition, err := catalog.GetAgentDefinition(ctx, candidate.DefinitionID, candidate.ActiveVersion)
		if err != nil {
			return nil, fmt.Errorf("resolve Agent %s definition: %w", candidate.ID, err)
		}
		if definition == nil {
			return nil, fmt.Errorf("resolve Agent %s definition: definition is unavailable", candidate.ID)
		}
		targets = append(targets, HostedAgentTarget{
			ID: strings.TrimSpace(candidate.ID), DisplayName: strings.TrimSpace(definition.DisplayName),
			Purpose: strings.TrimSpace(definition.Purpose),
		})
	}
	// A paused or removed member is simply not a delegation target, the same
	// way it sits out Team routing; it must not stop the rest of the Team.
	sort.Slice(targets, func(i, j int) bool { return targets[i].ID < targets[j].ID })
	return targets, nil
}

func hostedAgentInstructions(definition *kernelagent.AgentDefinition) []string {
	if definition == nil {
		return nil
	}
	instructions := []string{definition.SystemPrompt, "Purpose: " + definition.Purpose, agentResourcefulnessGuidance}
	if definition.Personality != "" {
		instructions = append(instructions, "Personality: "+definition.Personality)
	}
	return append(instructions, definition.OperatingPrinciples...)
}

func mergeOwnerModelActions(existing, owner []capability.ModelAction) ([]capability.ModelAction, map[string]struct{}, error) {
	merged := append([]capability.ModelAction(nil), existing...)
	indexByName := make(map[string]int, len(merged)+len(owner))
	for index, action := range merged {
		if _, duplicate := indexByName[action.Name]; duplicate {
			return nil, nil, fmt.Errorf("Agent Skill activation exposes ambiguous action %q", action.Name)
		}
		indexByName[action.Name] = index
	}
	shadowedBindings := make(map[string]struct{})
	for _, action := range owner {
		if index, duplicate := indexByName[action.Name]; duplicate {
			current := merged[index]
			// A Team binding may shadow the Agent's binding of the same
			// definition action. Binding tools are specific to their binding,
			// so two owners exposing one tool name is never a shadow.
			if current.SkillID != action.SkillID || current.Version != action.Version || current.Action != action.Action ||
				current.BindingTool != nil || action.BindingTool != nil {
				return nil, nil, fmt.Errorf("Team and Agent Skill activations expose conflicting action %q", action.Name)
			}
			shadowedBindings[ownerBindingKey(current.DeploymentID, current.BindingID)] = struct{}{}
			merged[index] = action
			continue
		}
		indexByName[action.Name] = len(merged)
		merged = append(merged, action)
	}
	return merged, shadowedBindings, nil
}

func ownerBindingKey(deploymentID, bindingID string) string { return deploymentID + "\x00" + bindingID }

func deploymentModelCredential(deployment *kernelagent.AgentDeployment) *capability.CredentialReference {
	if deployment == nil {
		return nil
	}
	reference, ok := deployment.Credentials["MODEL_PROVIDER"]
	if !ok {
		return nil
	}
	cloned := reference
	return &cloned
}

// projectActivatedSkills projects one activation into the Turn's prompts and
// model actions. After the deliberate kernel and chat-default preferences
// below, a model action name exposed twice is a hard error: the model names an
// action, so a duplicate could only be routed to one binding by guesswork.
func projectActivatedSkills(activation *skill.ActivationSnapshot) ([]HostedSkillPrompt, []capability.ModelAction, []PreparedSkillRuntime, []string, error) {
	prompts := make([]HostedSkillPrompt, 0, len(activation.Skills))
	actions := make([]capability.ModelAction, 0)
	prepared := make([]PreparedSkillRuntime, 0)
	refs := []string{"skill-snapshot:" + activation.SnapshotID}
	promptSeen := make(map[string]bool)
	// Kernel management Skills have one well-known binding identity. Historical
	// authoring versions could leave an equivalent alias beside it; never expose
	// both aliases as indistinguishable tools while asynchronous reconciliation
	// catches up. Non-kernel Skills retain exact-binding ambiguity because their
	// bindings may select different credentials, configuration, or authority.
	canonicalManagement := make(map[string]string)
	for _, activated := range activation.Skills {
		canonicalID := kernelManagementBindingID(activated.SkillID, activated.SkillVersion)
		if canonicalID == "" || activated.BindingID != canonicalID {
			continue
		}
		canonicalManagement[activated.SkillID+"@"+activated.SkillVersion] = canonicalID
	}
	// An explicit Agent binding can pin an older version of a Skill also
	// supplied by chat defaults. Prefer that exact binding for overlapping
	// actions; retain default-only actions and ambiguity between explicit
	// bindings, which may carry different credentials or authority.
	explicitActions := make(map[string]bool)
	for _, activated := range activation.Skills {
		if strings.HasPrefix(activated.BindingID, "bundled:") {
			continue
		}
		if canonicalID := canonicalManagement[activated.SkillID+"@"+activated.SkillVersion]; canonicalID != "" {
			continue
		}
		for _, action := range activated.Actions {
			explicitActions[action.SkillID+"\x00"+action.Name] = true
		}
	}
	for _, activated := range activation.Skills {
		if canonicalID := canonicalManagement[activated.SkillID+"@"+activated.SkillVersion]; canonicalID != "" && activated.BindingID != canonicalID {
			continue
		}
		// A Skill's instructions go into the turn once per version: the
		// built-in Skill alone has about ten bindings with identical text.
		// Every binding still contributes its evidence reference and actions.
		promptKey := activated.SkillID + "@" + activated.SkillVersion
		if activated.Prompt != nil && strings.TrimSpace(activated.Prompt.Instructions) != "" && !promptSeen[promptKey] {
			promptSeen[promptKey] = true
			prompt := HostedSkillPrompt{
				SkillID: activated.SkillID, Version: activated.SkillVersion, BindingID: activated.BindingID, BindingRevision: activated.BindingRevision,
				Name: activated.Name, Description: activated.Description, Instructions: activated.Prompt.Instructions,
			}
			prompt.Reference = skillPromptReference(prompt)
			prompts = append(prompts, prompt)
		}
		if activated.Prompt != nil && strings.TrimSpace(activated.Prompt.Instructions) != "" {
			refs = append(refs, fmt.Sprintf("skill:%s@%s#binding:%s@%d", activated.SkillID, activated.SkillVersion, activated.BindingID, activated.BindingRevision))
		}
		for _, action := range activated.Actions {
			if strings.HasPrefix(activated.BindingID, "bundled:") && explicitActions[action.SkillID+"\x00"+action.Name] {
				continue
			}
			if strings.TrimSpace(action.DeploymentID) == "" {
				action.DeploymentID = activation.DeploymentID
			}
			actions = append(actions, action)
		}
		if activated.PreparedRuntime != nil && len(activated.Actions) > 0 {
			preparedRuntime := *activated.PreparedRuntime
			preparedRuntime.Executables = append([]string(nil), activated.PreparedRuntime.Executables...)
			prepared = append(prepared, PreparedSkillRuntime{
				DeploymentID: activation.DeploymentID, BindingID: activated.BindingID, BindingRevision: activated.BindingRevision,
				SkillID: activated.SkillID, SkillVersion: activated.SkillVersion, Runtime: preparedRuntime,
			})
		}
	}
	exposed := make(map[string]string, len(actions))
	for _, action := range actions {
		if previous, duplicate := exposed[action.Name]; duplicate {
			return nil, nil, nil, nil, fmt.Errorf("%w: model action %q is exposed by bindings %s and %s", skill.ErrBindingAmbiguous, action.Name, previous, action.BindingID)
		}
		exposed[action.Name] = action.BindingID
	}
	return prompts, actions, prepared, refs, nil
}

// skillPromptReference is the appliedSkillRefs handle of a Skill prompt:
// skill:<id>@<version>, plus #binding:<id>@<revision> for a bound prompt.
func skillPromptReference(prompt HostedSkillPrompt) string {
	reference := "skill:" + strings.TrimSpace(prompt.SkillID) + "@" + strings.TrimSpace(prompt.Version)
	if strings.TrimSpace(prompt.BindingID) != "" && prompt.BindingRevision > 0 {
		reference += fmt.Sprintf("#binding:%s@%d", strings.TrimSpace(prompt.BindingID), prompt.BindingRevision)
	}
	return reference
}

func catalogRunbookEntrypoint(definition *kernelagent.AgentDefinition, run *AgentRun) string {
	if entrypoint := strings.TrimSpace(run.Entrypoint); entrypoint != "" {
		return entrypoint
	}
	base := "manual"
	switch run.Source {
	case RunSourceSchedule:
		base = "cron"
	case RunSourceWebhook:
		base = "webhook"
	case RunSourceEvent:
		base = "event"
		if trigger, ok := run.Checkpoint["trigger"].(map[string]interface{}); ok {
			for _, key := range []string{"kind", "type"} {
				if value, ok := trigger[key].(string); ok && (value == "k8s-event" || value == "k8s-watch" || value == "event") {
					base = value
				}
			}
		}
	}
	if definition != nil && definition.Runbook != nil {
		nodeID := ""
		if trigger, ok := run.Checkpoint["trigger"].(map[string]interface{}); ok {
			nodeID, _ = trigger["triggerNodeId"].(string)
		}
		if nodeID == "" {
			nodeID, _ = run.Context["triggerNodeId"].(string)
		}
		candidate := base + ":" + strings.TrimSpace(nodeID)
		if nodeID != "" {
			if _, ok := definition.Runbook.Entrypoints[candidate]; ok {
				return candidate
			}
		}
	}
	return base
}

// kernelManagementBindingID is the single well-known binding identity of a
// kernel-owned management Skill, reconciled for every deployment by the
// embedding host. Other Skills have no canonical identity ("").
func kernelManagementBindingID(skillID, version string) string {
	switch strings.TrimSpace(skillID) {
	case AgentManagementSkillID:
		return "bundled:agents"
	case ObjectiveManagementSkillID:
		return "bundled:objectives"
	case RunbookManagementSkillID:
		return "bundled:runbooks"
	case RunManagementSkillID:
		return "bundled:runs"
	case SkillManagementSkillID:
		return "bundled:skills@" + strings.TrimSpace(version)
	default:
		return ""
	}
}
