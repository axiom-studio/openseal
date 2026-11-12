package control

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/conversationrun"
	"github.com/axiom-studio/openseal/pkg/runtime/skillbinding"
	"github.com/axiom-studio/openseal/pkg/skill"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
)

// AuthorityInput is what the loop binding knows about a claim when it
// resolves which control tools the Run is offered (section 04 §4).
type AuthorityInput struct {
	// Run is the canonical Run being claimed (a conversation Run keeps its
	// conversation kind; it is never lowered to Agent work here).
	Run *models.AgentRun
	// DeploymentID is the Agent deployment that speaks for the Run.
	DeploymentID      string
	Catalog           conversationrun.AgentTurnCatalog
	Scope             skill.ScopeReference // zero = Run.Scope
	SkillPromptRefs   []string
	Operations        []agentrun.HostedRunbookOperation // binding.RunbookOperations
	ConversationTasks models.ConversationTaskKernelStore
	Runs              models.PortfolioStore
	Conversations     models.ConversationStore
	// Deterministic marks runbook/capability runners: no model
	// control tools are offered (kernel-origin calls are preauthorized).
	Deterministic bool
}

// ResolveControlAuthority resolves the control authority of one claim. It is
// called once per claim so the advertised tool list stays stable.
func ResolveControlAuthority(ctx context.Context, in AuthorityInput) (agentrun.ControlAuthority, error) {
	run := in.Run
	if run == nil {
		return agentrun.ControlAuthority{}, errors.New("control authority requires a Run")
	}
	agentID := strings.TrimSpace(in.DeploymentID)
	if agentID == "" {
		return agentrun.ControlAuthority{}, errors.New("control authority requires the speaking Agent deployment")
	}
	authority := agentrun.ControlAuthority{AgentID: agentID, FinalMode: controlFinalMode(run),
		RoutineOccurrence: conversationrun.RoutineOccurrenceRun(run) && run.Kind != models.RunKindConversation}
	if in.Deterministic {
		return authority, nil
	}
	mode := authority.FinalMode
	if mode == agentrun.FinalModeDecision || mode == agentrun.FinalModeReview {
		// Decision and review Runs get only their typed final tool.
		return authority, nil
	}
	scope := in.Scope
	if scope.Kind == "" && scope.ID == "" {
		scope = skill.ScopeReference{Kind: run.Scope.Kind, ID: run.Scope.ID}
	}
	taskWork := run.Kind == models.RunKindAgentWork && run.Context[models.ConversationTaskContextKey] != nil
	conversationID, _ := run.Context[models.ConversationRunConversationIDContextKey].(string)
	triggerID, _ := run.Context[models.ConversationRunTriggerIDContextKey].(string)
	chatReply := strings.TrimSpace(conversationID) != "" && strings.TrimSpace(triggerID) != ""
	voiceCall := run.Context["voiceCallStarted"] == true

	authority.SkillPromptRefs = append([]string(nil), in.SkillPromptRefs...)
	authority.VisualAllowed = mode == agentrun.FinalModeDefault
	authority.BroadcastAllowed = run.Kind == models.RunKindConversation
	authority.TaskWork = taskWork
	authority.CanReportProgress = true
	authority.CanWait = !chatReply
	authority.VoiceCallStarted = voiceCall
	authority.Operations = append([]agentrun.HostedRunbookOperation(nil), in.Operations...)

	targets, err := resolveHostedAgentTargets(ctx, in.Catalog, scope, run, agentID)
	if err != nil {
		return agentrun.ControlAuthority{}, fmt.Errorf("resolve eligible Agent targets: %w", err)
	}
	// No nested model forks (DI-15). Targets exclude the current Agent, so a
	// task worker, which may fork only itself, gets none (self only).
	authority.ForkAllowed = run.Source != models.RunSourceFork
	if authority.ForkAllowed && !taskWork {
		authority.ForkTargets = targets
	}
	if !taskWork {
		authority.DelegateTargets = append([]agentrun.HostedAgentTarget(nil), targets...)
	}

	tasks, err := conversationrun.ResolveCatalogConversationTaskContext(ctx, in.Catalog, run, in.ConversationTasks)
	if err != nil {
		return agentrun.ControlAuthority{}, fmt.Errorf("resolve conversation task context: %w", err)
	}
	if tasks != nil {
		// The task snapshots stay visible; only the start right is withdrawn.
		if tasks.CanStart && (voiceCall || models.ValidateConversationTaskStartAuthority(run, tasks) != nil) {
			tasks.CanStart = false
		}
		authority.TaskStart = tasks
	}

	authority.CanReact = run.Kind == models.RunKindConversation && chatReply && run.AssignedAgentID == agentID
	if in.Runs != nil && in.Conversations != nil {
		_, conversation, trigger, err := runtimeconversation.WorkOrigin(ctx, in.Runs, in.Conversations, run)
		if err != nil {
			return agentrun.ControlAuthority{}, fmt.Errorf("resolve conversation origin: %w", err)
		}
		authority.CanAskUser = conversation != nil && trigger != nil && run.AssignedAgentID == agentID &&
			run.AssignedAgentID == runtimeconversation.RunAgentID(conversation, run)
		authority.ExternalChannelReply = models.IsExternalChannelConversation(conversation)
		_, authority.ApprovalMode, err = action.RunConversationApprovalMode(ctx, in.Runs, in.Conversations, run)
		if err != nil {
			return agentrun.ControlAuthority{}, fmt.Errorf("resolve conversation approval mode: %w", err)
		}
	}
	return authority, nil
}

func controlFinalMode(run *models.AgentRun) agentrun.FinalAnswerMode {
	if _, ok := run.Context[agentrun.AgentRequestInboxContextKey]; ok {
		return agentrun.FinalModeDecision
	}
	if _, ok := run.Context[agentrun.AgentRequestCompletionReviewContextKey]; ok {
		return agentrun.FinalModeReview
	}
	if run.Context["responseMode"] == "spoken" {
		return conversationrun.FinalModeSpoken
	}
	return agentrun.FinalModeDefault
}

func resolveHostedAgentTargets(
	ctx context.Context,
	catalog conversationrun.AgentTurnCatalog,
	scope skill.ScopeReference,
	run *models.AgentRun,
	currentDeploymentID string,
) ([]agentrun.HostedAgentTarget, error) {
	if run == nil || run.Owner.Type != models.OwnerTypeTeam || strings.TrimSpace(run.Owner.ID) == "" {
		return nil, nil
	}
	teamCatalog, ok := catalog.(skillbinding.TeamSkillAuthorityCatalog)
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
	targets := make([]agentrun.HostedAgentTarget, 0, len(deployments))
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
		targets = append(targets, agentrun.HostedAgentTarget{
			ID: strings.TrimSpace(candidate.ID), DisplayName: strings.TrimSpace(definition.DisplayName),
			Purpose: strings.TrimSpace(definition.Purpose),
		})
	}
	// A paused or removed member is simply not a delegation target, the same
	// way it sits out Team routing; it must not stop the rest of the Team.
	sort.Slice(targets, func(i, j int) bool { return targets[i].ID < targets[j].ID })
	return targets, nil
}
