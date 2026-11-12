package daemon

import (
	"context"
	"errors"
	"fmt"
	"github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/activity"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	runtimecollaboration "github.com/axiom-studio/openseal/pkg/runtime/collaboration"
	"github.com/axiom-studio/openseal/pkg/runtime/control"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/conversationrun"
	"github.com/axiom-studio/openseal/pkg/runtime/modelstep"
	"github.com/axiom-studio/openseal/pkg/runtime/runloop"
	"github.com/axiom-studio/openseal/pkg/runtime/runworker"
	"github.com/axiom-studio/openseal/pkg/team"
	"go.uber.org/zap"
	"strings"
)

// DesktopWorkRequest constructs a local root task. Renderer-owned lifecycle
// state and policy must never be imported into the execution authority.
func DesktopWorkRequest(scope models.Scope, request models.CreateAgentRunRequest) (models.CreateAgentRunRequest, error) {
	validOwner := request.Owner.Type == models.OwnerTypeAgent && request.Owner.ID == request.AssignedAgentID || request.Owner.Type == models.OwnerTypeTeam && strings.TrimSpace(request.Owner.ID) != ""
	if scope.Kind != "local" || scope.ID == "" || request.Scope != scope || request.Kind != models.RunKindAgentWork || !validOwner || strings.TrimSpace(request.AssignedAgentID) == "" || strings.TrimSpace(request.Goal) == "" || strings.TrimSpace(request.IdempotencyKey) == "" {
		return models.CreateAgentRunRequest{}, errors.New("desktop work requires a local agent, goal, and idempotency key")
	}
	if request.ParentRunID != "" || request.ObjectiveID != "" || len(request.Context) > 0 || len(request.Checkpoint) > 0 || len(request.Plan) > 0 || len(request.Policy) > 0 || request.WakeCondition != nil || request.Budget != nil || len(request.ResourceRequirements) > 0 || request.ConcurrencyKey != "" || request.Entrypoint != "" {
		return models.CreateAgentRunRequest{}, errors.New("desktop work cannot supply host-owned execution state or policy")
	}
	return models.CreateAgentRunRequest{Scope: scope, Kind: models.RunKindAgentWork, Owner: request.Owner, AssignedAgentID: request.AssignedAgentID, ActingUser: "", Goal: strings.TrimSpace(request.Goal), Source: models.RunSource("manual"), IdempotencyKey: request.IdempotencyKey, Actor: models.ActivityActor{Type: "user", ID: "local-operator"}, Visibility: models.ActivityVisibility("scope"), Budget: DesktopTaskBudget()}, nil
}

// DesktopTaskBudget is the host-owned budget of a desktop task. MaxTurns counts
// model steps; a loop re-sends the session each step, so tokens and actions
// scale with the turns (A18). MaxAttempts counts claims.
func DesktopTaskBudget() *models.BudgetPolicy {
	return &models.BudgetPolicy{MaxTurns: 64, MaxAttempts: 24, MaxTotalTokens: 512000, MaxOutputTokens: 32000, MaxActions: 64, MaxDurationMS: 15 * 60 * 1000}
}

// DesktopConversationBudget bounds one routed channel reply (A18).
func DesktopConversationBudget() *models.BudgetPolicy {
	return &models.BudgetPolicy{MaxTurns: 12, MaxAttempts: 4, MaxTotalTokens: 1728000, MaxOutputTokens: 49152, MaxDurationMS: 180000}
}

// DesktopKernel is what the daemon's run loop reads from the kernel.
// *openseal.Engine implements it.
type DesktopKernel interface {
	conversationrun.AgentTurnCatalog
	models.AcceptedRunExecutionPreparer
	Store() models.KernelStore
	GovernedTools() *runloop.GovernedToolExecutor
	ReactionProjector() conversation.MessageReactionProjector
}

// DesktopTurnResolver resolves the loop binding of every Run the daemon's
// workers claim. Runs of Scope get the desktop provider as their step host and
// the generated-files publisher; other Runs bind without a step host, so only
// deterministic Runs execute there. Bind it to the kernel after
// the kernel is built and before it starts.
type DesktopTurnResolver struct {
	Scope     models.Scope
	Host      *ProviderStepHost // nil: no model Run executes
	Publisher *TaskArtifactPublisher

	kernel DesktopKernel
	config runworker.LoopBindingConfig
}

// Bind builds the loop services over the kernel's store.
func (r *DesktopTurnResolver) Bind(kernel DesktopKernel, content models.ArtifactContentStore, logger *zap.SugaredLogger) error {
	config, err := desktopLoopBindingConfig(kernel, content, logger)
	if err != nil {
		return err
	}
	r.kernel, r.config = kernel, config
	return nil
}

func (r *DesktopTurnResolver) ResolveTurnRunner(ctx context.Context, run *models.AgentRun) (*agentrun.TurnRunnerBinding, error) {
	if r == nil || r.kernel == nil || run == nil {
		return nil, modelstep.ErrTurnHostUnavailable
	}
	config := r.config
	local := r.Host != nil && run.Scope == r.Scope
	if local {
		// Assigned only when set, so a nil host never becomes a non-nil
		// interface.
		config.Host = r.Host
	}
	binding, err := runworker.ResolveLoopBinding(ctx, r.kernel, run, config)
	if err == nil && binding != nil && local && r.Publisher != nil {
		binding.FinalOutput = r.Publisher
	}
	return binding, err
}

// desktopLoopBindingConfig builds the run loop services of the daemon:
// governed and control tools, no host tools (the desktop step host has none)
// and no image input (the provider is text only).
func desktopLoopBindingConfig(engine DesktopKernel, content models.ArtifactContentStore, logger *zap.SugaredLogger) (runworker.LoopBindingConfig, error) {
	if engine == nil || engine.Store() == nil {
		return runworker.LoopBindingConfig{}, errors.New("desktop loop wiring requires the kernel store")
	}
	store := engine.Store()
	control := &control.ControlExecutor{Portfolio: store, Sessions: store, Activity: activity.NewRunActivityService(store, store)}
	if forks, ok := store.(models.RunForkStore); ok {
		control.Forks = agentrun.NewRunForkCoordinator(forks)
		control.Forks.SetAcceptedRunExecutionPreparer(engine)
	}
	if collaboration, ok := store.(models.CollaborationKernelStore); ok {
		control.Collaboration = runtimecollaboration.NewCollaborationService(collaboration)
		control.Collaboration.SetAcceptedRunExecutionPreparer(engine)
	}
	if tasks, ok := store.(models.ConversationTaskKernelStore); ok {
		control.Tasks = conversationrun.NewConversationTaskService(tasks)
		control.Tasks.SetAcceptedRunExecutionPreparer(engine)
	}
	var conversations models.ConversationStore
	if reporting, ok := store.(models.ConversationStore); ok {
		conversations = reporting
		control.Reporting = reporting
		control.Conversations = conversation.NewConversationService(reporting)
		control.Conversations.SetReactionProjector(engine.ReactionProjector())
	}
	tools, err := runloop.NewStepToolDispatcher(runloop.StepToolDispatcherConfig{
		Sessions: store, Turns: store, Actions: store,
		// Only the refused row of a repeat refusal goes through this
		// coordinator; proposals go through the engine's.
		Coordinator: action.NewActionCoordinator(store, store, nil, nil, nil),
		Governed:    engine.GovernedTools(), Control: control, Logger: logger,
	})
	if err != nil {
		return runworker.LoopBindingConfig{}, err
	}
	media := modelstep.ModelStepMedia{Content: content}
	if artifacts, ok := store.(models.ArtifactStore); ok {
		media.Artifacts = artifacts
	}
	return runworker.LoopBindingConfig{Store: store, Conversations: conversations, Tools: tools, Media: media, Logger: logger}, nil
}

type DesktopWorkCatalog interface {
	GetAgentDeployment(context.Context, capability.ScopeReference, string) (*agent.AgentDeployment, error)
	GetTeamDeployment(context.Context, capability.ScopeReference, string) (*team.Deployment, error)
}

// ResolveDesktopWork validates current host-owned membership before creating work.
// The canonical turn resolver rechecks team authority when work executes.
func ResolveDesktopWork(ctx context.Context, catalog DesktopWorkCatalog, scope models.Scope, request models.CreateAgentRunRequest) (models.CreateAgentRunRequest, error) {
	canonical, err := DesktopWorkRequest(scope, request)
	if err != nil {
		return canonical, err
	}
	ref := capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}
	ids := []string{canonical.AssignedAgentID}
	if canonical.Owner.Type == models.OwnerTypeTeam {
		deployment, err := catalog.GetTeamDeployment(ctx, ref, canonical.Owner.ID)
		if err != nil {
			return canonical, err
		}
		if deployment == nil || deployment.Status != team.DeploymentActive {
			return canonical, errors.New("the team must be active before starting work")
		}
		ids = nil
		member := false
		for _, assignment := range deployment.Roster {
			ids = append(ids, assignment.AgentDeploymentID)
			member = member || assignment.AgentDeploymentID == canonical.AssignedAgentID
		}
		if !member {
			return canonical, errors.New("the lead agent must belong to this team")
		}
	}
	for _, id := range ids {
		deployment, err := catalog.GetAgentDeployment(ctx, ref, id)
		if err != nil {
			return canonical, err
		}
		if deployment == nil || deployment.RolloutStatus != agent.RolloutActive {
			return canonical, fmt.Errorf("agent %s must be active before starting work", id)
		}
	}
	return canonical, nil
}
