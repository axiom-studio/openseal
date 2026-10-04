package runtime

import (
	"context"
	"errors"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
)

// The conversation adapter lowers the Run kind for ordinary Agent cognition.
// Recover the source through its durable identity rather than treating that
// lowered kind or copied context as authority to create independent work.
func resolveCatalogConversationTaskContext(ctx context.Context, catalog AgentTurnCatalog, run *AgentRun, store ConversationTaskKernelStore) (*HostedConversationTaskContext, error) {
	if run == nil {
		return nil, nil
	}
	conversationID, conversationString := run.Context[conversationRunContextConversationID].(string)
	messageID, messageString := run.Context[conversationRunContextTriggerID].(string)
	if !conversationString || !messageString || !validOpaqueIdentifier(conversationID, 128) || !validOpaqueIdentifier(messageID, 128) {
		return nil, nil
	}
	if store == nil {
		store, _ = catalog.(ConversationTaskKernelStore)
		if provider, ok := catalog.(interface{ Store() KernelStore }); store == nil && ok {
			store, _ = provider.Store().(ConversationTaskKernelStore)
		}
	}
	if store == nil {
		return nil, nil
	}
	canonical, err := store.GetAgentRun(ctx, run.Scope, run.ID)
	if errors.Is(err, ErrRunNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if canonical == nil || canonical.Scope != run.Scope || canonical.Owner != run.Owner || canonical.ID != run.ID ||
		canonical.AssignedAgentID != run.AssignedAgentID || canonical.ParentRunID != run.ParentRunID || canonical.RootRunID != run.RootRunID {
		return nil, nil
	}
	for _, key := range []string{conversationRunContextConversationID, conversationRunContextTriggerID, "threadRootMessageId"} {
		original, originalString := canonical.Context[key].(string)
		provided, providedString := run.Context[key].(string)
		if originalString != providedString || original != provided || !originalString && (canonical.Context[key] != nil || run.Context[key] != nil) {
			return nil, nil
		}
	}
	foreground := canonical.Kind == RunKindConversation && canonical.Source == RunSourceChat && canonical.ParentRunID == "" && canonical.RootRunID == canonical.ID
	var origin *ConversationTask
	_, markedTask := canonical.Context[ConversationTaskContextKey]
	if !foreground || markedTask {
		if canonical.Kind != RunKindAgentWork && !foreground {
			return nil, nil
		}
		marker, marked := canonical.Context[ConversationTaskContextKey].(string)
		if !marked || !validOpaqueIdentifier(marker, 128) {
			return nil, nil
		}
		task, taskErr := persistedTaskWorkOrigin(ctx, store, canonical)
		if errors.Is(taskErr, ErrConversationTaskNotFound) || errors.Is(taskErr, ErrInvalidConversationTask) {
			return nil, nil
		}
		if taskErr != nil {
			return nil, taskErr
		}
		if task == nil {
			return nil, nil
		}
		origin = task
		foreground = false
	}
	conversation, err := store.GetConversation(ctx, canonical.Scope, conversationID)
	if err != nil {
		return nil, err
	}
	if conversation == nil || conversation.Scope != canonical.Scope || conversation.Owner != canonical.Owner || conversation.ID != conversationID {
		return nil, nil
	}
	message, err := store.GetChannelMessage(ctx, canonical.Scope, conversation.ID, messageID)
	if err != nil {
		return nil, err
	}
	if message == nil || message.Scope != canonical.Scope || message.ConversationID != conversation.ID || message.ID != messageID {
		return nil, nil
	}
	actor := message.Sender
	if actor.Type == ConversationParticipantService && message.Initiator != nil {
		actor = *message.Initiator
	}
	if actor.Type != ConversationParticipantUser || actor.Validate() != nil {
		return nil, nil
	}
	if origin != nil {
		root, err := store.GetAgentRun(ctx, canonical.Scope, origin.WorkRunID)
		if err != nil {
			return nil, err
		}
		valid, err := VerifyConversationTaskWorkRun(ctx, store, origin, root)
		if err != nil {
			return nil, err
		}
		if !valid {
			return nil, nil
		}
	}
	viewer := ConversationViewer{Participant: actor}
	if _, err := NewConversationService(store).GetVisibleChannelMessage(ctx, canonical.Scope, conversation.ID, message.ID, viewer); err != nil {
		if errors.Is(err, ErrChannelMessageNotFound) {
			return nil, nil
		}
		return nil, err
	}
	thread := externalConversationThreadRoot(conversation, message)
	if foreground {
		storedThread, _ := canonical.Context["threadRootMessageId"].(string)
		if validateConversationRun(canonical) != nil || storedThread != thread {
			return nil, nil
		}
	}
	result := &HostedConversationTaskContext{ConversationID: conversation.ID, ThreadRootID: thread, Tasks: []ConversationTaskSnapshot{}}
	result.CanStart = foreground && conversation.Status == ConversationStatusActive && canonical.Status == AgentRunStatusRunning &&
		canonical.AssignedAgentID != "" && canonical.LeaseOwner != "" && canonical.LeaseExpiresAt != nil && canonical.LeaseExpiresAt.After(time.Now()) && !voiceCallStartedMessage(message)
	if result.CanStart && canonical.Owner.Type == OwnerTypeTeam {
		result.CanStart = false
		if teams, ok := store.(collaborationTeamStore); ok {
			team, err := teams.GetTeamDeployment(ctx, capability.ScopeReference{Kind: canonical.Scope.Kind, ID: canonical.Scope.ID}, canonical.Owner.ID)
			if err != nil {
				return nil, err
			}
			if team != nil && string(team.Status) == "active" {
				for _, member := range team.Roster {
					if member.AgentDeploymentID == canonical.AssignedAgentID {
						result.CanStart = true
						break
					}
				}
			}
		}
	}
	rows, err := store.ListConversationTasks(ctx, ConversationTaskFilter{
		Scope: canonical.Scope, Owner: canonical.Owner, ConversationID: conversation.ID, ThreadRootID: thread,
		AuthenticatedActor: actor, ActiveOnly: true, Limit: 10,
	})
	if err != nil {
		return nil, err
	}
	// The executing independent root needs its own verified task identity even
	// when newer work fills the bounded active-task page. Forks and foreground
	// conversations retain their ordinary status view and ordering.
	if origin != nil && origin.Mode == ConversationTaskModeIndependent && canonical.ID == origin.WorkRunID {
		rows = append([]*ConversationTaskResult{{Task: origin, WorkRun: canonical}}, rows...)
	}
	proof := &ConversationChangeService{conversations: NewConversationService(store), portfolio: store}
	seen := make(map[string]bool, len(rows))
	for _, row := range rows {
		if row == nil || row.Task == nil || row.WorkRun == nil || row.Task.AuthenticatedActor != actor ||
			seen[row.Task.ID] || thread != "" && row.Task.ThreadRootID != thread || isTerminalAgentRunStatus(row.WorkRun.Status) {
			continue
		}
		source, err := proof.proveConversationTaskRun(ctx, conversation, row.Task, row.WorkRun, &viewer)
		if err != nil {
			return nil, err
		}
		if source == nil {
			continue
		}
		seen[row.Task.ID] = true
		result.Tasks = append(result.Tasks, ConversationTaskSnapshot{
			TaskID: row.Task.ID, Mode: row.Task.Mode, WorkRunID: row.WorkRun.ID, Goal: ConversationTaskGoalSummary(row.Task.Goal),
			Status: row.WorkRun.Status, Revision: row.WorkRun.Revision, AvailableControls: applicableAgentRunCommands(row.WorkRun), CreatedAt: row.Task.CreatedAt,
		})
		if len(result.Tasks) == 10 {
			break
		}
	}
	if err := result.Validate(); err != nil {
		return nil, err
	}
	return result, nil
}
