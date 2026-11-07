package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/axiom-studio/openseal/pkg/skill"
)

// ControlAuthorityInput is what the loop binding knows about a claim when it
// resolves which control tools the Run is offered (section 04 §4).
type ControlAuthorityInput struct {
	// Run is the canonical Run being claimed (a conversation Run keeps its
	// conversation kind; it is never lowered to Agent work here).
	Run *AgentRun
	// DeploymentID is the Agent deployment that speaks for the Run.
	DeploymentID      string
	Catalog           AgentTurnCatalog
	Scope             skill.ScopeReference // zero = Run.Scope
	SkillPromptRefs   []string
	Operations        []HostedRunbookOperation // binding.RunbookOperations
	ConversationTasks ConversationTaskKernelStore
	Runs              PortfolioStore
	Conversations     ConversationStore
	// Deterministic marks runbook/outreach/capability runners: no model
	// control tools are offered (kernel-origin calls are preauthorized).
	Deterministic bool
}

// ResolveControlAuthority resolves the control authority of one claim. It is
// called once per claim so the advertised tool list stays stable.
func ResolveControlAuthority(ctx context.Context, in ControlAuthorityInput) (ControlAuthority, error) {
	run := in.Run
	if run == nil {
		return ControlAuthority{}, errors.New("control authority requires a Run")
	}
	agentID := strings.TrimSpace(in.DeploymentID)
	if agentID == "" {
		return ControlAuthority{}, errors.New("control authority requires the speaking Agent deployment")
	}
	authority := ControlAuthority{AgentID: agentID, FinalMode: controlFinalMode(run)}
	if in.Deterministic {
		return authority, nil
	}
	mode := authority.FinalMode
	if mode == FinalModeDecision || mode == FinalModeReview {
		// Decision and review Runs get only their typed final tool.
		return authority, nil
	}
	scope := in.Scope
	if scope.Kind == "" && scope.ID == "" {
		scope = skill.ScopeReference{Kind: run.Scope.Kind, ID: run.Scope.ID}
	}
	taskWork := run.Kind == RunKindAgentWork && run.Context[ConversationTaskContextKey] != nil
	conversationID, _ := run.Context[conversationRunContextConversationID].(string)
	triggerID, _ := run.Context[conversationRunContextTriggerID].(string)
	chatReply := strings.TrimSpace(conversationID) != "" && strings.TrimSpace(triggerID) != ""
	voiceCall := run.Context["voiceCallStarted"] == true

	authority.SkillPromptRefs = append([]string(nil), in.SkillPromptRefs...)
	authority.VisualAllowed = mode == FinalModeDefault || mode == FinalModeEvidence
	authority.BroadcastAllowed = run.Kind == RunKindConversation
	authority.TaskWork = taskWork
	authority.CanReportProgress = true
	authority.CanWait = !chatReply
	authority.VoiceCallStarted = voiceCall
	authority.Operations = append([]HostedRunbookOperation(nil), in.Operations...)

	targets, err := resolveHostedAgentTargets(ctx, in.Catalog, scope, run, agentID)
	if err != nil {
		return ControlAuthority{}, fmt.Errorf("resolve eligible Agent targets: %w", err)
	}
	// No nested model forks (DI-15). Targets exclude the current Agent, so a
	// task worker, which may fork only itself, gets none (self only).
	authority.ForkAllowed = run.Source != RunSourceFork
	if authority.ForkAllowed && !taskWork {
		authority.ForkTargets = targets
	}
	if !taskWork {
		authority.DelegateTargets = append([]HostedAgentTarget(nil), targets...)
	}

	tasks, err := resolveCatalogConversationTaskContext(ctx, in.Catalog, run, in.ConversationTasks)
	if err != nil {
		return ControlAuthority{}, fmt.Errorf("resolve conversation task context: %w", err)
	}
	if tasks != nil {
		// The task snapshots stay visible; only the start right is withdrawn.
		if tasks.CanStart && (voiceCall || validateConversationTaskStartAuthority(run, tasks) != nil) {
			tasks.CanStart = false
		}
		authority.TaskStart = tasks
	}

	authority.CanReact = run.Kind == RunKindConversation && chatReply && run.AssignedAgentID == agentID
	if in.Runs != nil && in.Conversations != nil {
		_, conversation, trigger, err := conversationWorkOrigin(ctx, in.Runs, in.Conversations, run)
		if err != nil {
			return ControlAuthority{}, fmt.Errorf("resolve conversation origin: %w", err)
		}
		authority.CanAskUser = conversation != nil && trigger != nil && run.AssignedAgentID == agentID &&
			run.AssignedAgentID == ConversationRunAgentID(conversation, run)
		authority.ExternalChannelReply = externalChannelContext(conversation)
		_, authority.ApprovalMode, err = RunConversationApprovalMode(ctx, in.Runs, in.Conversations, run)
		if err != nil {
			return ControlAuthority{}, fmt.Errorf("resolve conversation approval mode: %w", err)
		}
	}
	return authority, nil
}

func controlFinalMode(run *AgentRun) FinalAnswerMode {
	if _, ok := run.Context[AgentRequestInboxContextKey]; ok {
		return FinalModeDecision
	}
	if _, ok := run.Context[AgentRequestCompletionReviewContextKey]; ok {
		return FinalModeReview
	}
	if evidenceSnapshotInputContextRef(run.Context) != "" {
		return FinalModeEvidence
	}
	if run.Context["responseMode"] == "spoken" {
		return FinalModeSpoken
	}
	return FinalModeDefault
}

// validateConversationTaskStartAuthority reports whether run is a foreground
// conversation Run allowed to start independent work in taskContext's
// conversation and thread.
func validateConversationTaskStartAuthority(run *AgentRun, taskContext *HostedConversationTaskContext) error {
	if run == nil || run.Kind != RunKindConversation || strings.TrimSpace(run.ParentRunID) != "" ||
		run.Context[ConversationTaskContextKey] != nil || taskContext == nil || !taskContext.CanStart || !validOpaqueIdentifier(taskContext.ConversationID, 256) {
		return errors.New("independent task admission is not available to this Run")
	}
	conversationID, _ := run.Context[conversationRunContextConversationID].(string)
	triggerID, _ := run.Context[conversationRunContextTriggerID].(string)
	threadRootID, _ := run.Context["threadRootMessageId"].(string)
	if strings.TrimSpace(conversationID) != taskContext.ConversationID || !validOpaqueIdentifier(triggerID, 256) ||
		strings.TrimSpace(threadRootID) != strings.TrimSpace(taskContext.ThreadRootID) {
		return errors.New("task context does not match the foreground conversation")
	}
	return nil
}
