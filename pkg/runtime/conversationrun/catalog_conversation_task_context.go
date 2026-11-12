package conversationrun

import (
	"context"
	"errors"
	"time"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// ResolveCatalogConversationTaskContext is the conversation task context of a
// catalog Agent's Run.
//
// The conversation adapter lowers the Run kind for ordinary Agent cognition.
// Recover the source through its durable identity rather than treating that
// lowered kind or copied context as authority to create independent work.
func ResolveCatalogConversationTaskContext(ctx context.Context, catalog AgentTurnCatalog, run *models.AgentRun, store models.ConversationTaskKernelStore) (*models.HostedConversationTaskContext, error) {
	if run == nil {
		return nil, nil
	}
	conversationID, conversationString := run.Context[models.ConversationRunConversationIDContextKey].(string)
	messageID, messageString := run.Context[models.ConversationRunTriggerIDContextKey].(string)
	if !conversationString || !messageString || !models.IsValidOpaqueIdentifier(conversationID, 128) || !models.IsValidOpaqueIdentifier(messageID, 128) {
		return nil, nil
	}
	if store == nil {
		store, _ = catalog.(models.ConversationTaskKernelStore)
		if provider, ok := catalog.(interface{ Store() models.KernelStore }); store == nil && ok {
			store, _ = provider.Store().(models.ConversationTaskKernelStore)
		}
	}
	if store == nil {
		return nil, nil
	}
	canonical, err := store.GetAgentRun(ctx, run.Scope, run.ID)
	if errors.Is(err, models.ErrRunNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if canonical == nil || canonical.Scope != run.Scope || canonical.Owner != run.Owner || canonical.ID != run.ID ||
		canonical.AssignedAgentID != run.AssignedAgentID || canonical.ParentRunID != run.ParentRunID || canonical.RootRunID != run.RootRunID {
		return nil, nil
	}
	for _, key := range []string{models.ConversationRunConversationIDContextKey, models.ConversationRunTriggerIDContextKey, "threadRootMessageId"} {
		original, originalString := canonical.Context[key].(string)
		provided, providedString := run.Context[key].(string)
		if originalString != providedString || original != provided || !originalString && (canonical.Context[key] != nil || run.Context[key] != nil) {
			return nil, nil
		}
	}
	foreground := canonical.Kind == models.RunKindConversation && canonical.Source == models.RunSourceChat && canonical.ParentRunID == "" && canonical.RootRunID == canonical.ID
	var origin *models.ConversationTask
	_, markedTask := canonical.Context[models.ConversationTaskContextKey]
	if !foreground || markedTask {
		if canonical.Kind != models.RunKindAgentWork && !foreground {
			return nil, nil
		}
		marker, marked := canonical.Context[models.ConversationTaskContextKey].(string)
		if !marked || !models.IsValidOpaqueIdentifier(marker, 128) {
			return nil, nil
		}
		task, taskErr := agentrun.PersistedTaskWorkOrigin(ctx, store, canonical)
		if errors.Is(taskErr, agentrun.ErrConversationTaskNotFound) || errors.Is(taskErr, models.ErrInvalidConversationTask) {
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
	if actor.Type == models.ConversationParticipantService && message.Initiator != nil {
		actor = *message.Initiator
	}
	if actor.Type != models.ConversationParticipantUser || actor.Validate() != nil {
		return nil, nil
	}
	if origin != nil {
		root, err := store.GetAgentRun(ctx, canonical.Scope, origin.WorkRunID)
		if err != nil {
			return nil, err
		}
		valid, err := agentrun.VerifyConversationTaskWorkRun(ctx, store, origin, root)
		if err != nil {
			return nil, err
		}
		if !valid {
			return nil, nil
		}
	}
	viewer := models.ConversationViewer{Participant: actor}
	if _, err := runtimeconversation.NewConversationService(store).GetVisibleChannelMessage(ctx, canonical.Scope, conversation.ID, message.ID, viewer); err != nil {
		if errors.Is(err, models.ErrChannelMessageNotFound) {
			return nil, nil
		}
		return nil, err
	}
	thread := models.ExternalConversationThreadRoot(conversation, message)
	if foreground {
		storedThread, _ := canonical.Context["threadRootMessageId"].(string)
		if models.ValidateConversationRun(canonical) != nil || storedThread != thread {
			return nil, nil
		}
	}
	result := &models.HostedConversationTaskContext{ConversationID: conversation.ID, ThreadRootID: thread, Tasks: []models.ConversationTaskSnapshot{}}
	result.CanStart = foreground && conversation.Status == models.ConversationStatusActive && canonical.Status == models.AgentRunStatusRunning &&
		canonical.AssignedAgentID != "" && canonical.LeaseOwner != "" && canonical.LeaseExpiresAt != nil && canonical.LeaseExpiresAt.After(time.Now()) && !models.VoiceCallStartedMessage(message)
	if result.CanStart {
		onRoster, err := models.AgentOnChatRoster(ctx, models.TeamRosterReaderOf(store), conversation.Scope, conversation.Owner, runtimeconversation.ConversationRunAgentID(conversation, canonical))
		if err != nil {
			return nil, err
		}
		result.CanStart = onRoster
	}
	rows, err := store.ListConversationTasks(ctx, models.ConversationTaskFilter{
		Scope: canonical.Scope, Owner: canonical.Owner, ConversationID: conversation.ID, ThreadRootID: thread,
		AuthenticatedActor: actor, ActiveOnly: true, Limit: 10,
	})
	if err != nil {
		return nil, err
	}
	// The executing independent root needs its own verified task identity even
	// when newer work fills the bounded active-task page. Forks and foreground
	// conversations retain their ordinary status view and ordering.
	if origin != nil && origin.Mode == models.ConversationTaskModeIndependent && canonical.ID == origin.WorkRunID {
		rows = append([]*models.ConversationTaskResult{{Task: origin, WorkRun: canonical}}, rows...)
	}
	proof, err := runtimeconversation.NewConversationChangeService(store, store)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(rows))
	for _, row := range rows {
		if row == nil || row.Task == nil || row.WorkRun == nil || row.Task.AuthenticatedActor != actor ||
			seen[row.Task.ID] || thread != "" && row.Task.ThreadRootID != thread || models.IsTerminalAgentRunStatus(row.WorkRun.Status) {
			continue
		}
		source, err := proof.ProveConversationTaskRun(ctx, conversation, row.Task, row.WorkRun, &viewer)
		if err != nil {
			return nil, err
		}
		if source == nil {
			continue
		}
		seen[row.Task.ID] = true
		result.Tasks = append(result.Tasks, models.ConversationTaskSnapshot{
			TaskID: row.Task.ID, Mode: row.Task.Mode, WorkRunID: row.WorkRun.ID, Goal: ConversationTaskGoalSummary(row.Task.Goal),
			Status: row.WorkRun.Status, Revision: row.WorkRun.Revision, AvailableControls: agentrun.ApplicableAgentRunCommands(row.WorkRun), CreatedAt: row.Task.CreatedAt,
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

// AgentTurnCatalog is the portable, product-neutral catalog required to bind
// a durable Run to its immutable Agent definition and activated Skills.
type AgentTurnCatalog interface {
	GetAgentDeployment(context.Context, skill.ScopeReference, string) (*kernelagent.AgentDeployment, error)
	ListAgentDeployments(context.Context, kernelagent.AgentDeploymentFilter) ([]*kernelagent.AgentDeployment, error)
	GetAgentDefinition(context.Context, string, string) (*kernelagent.AgentDefinition, error)
	ActivateSkills(context.Context, skill.ScopeReference, string, skill.HostCapabilityState) (*skill.ActivationSnapshot, error)
	// ActionCallReader lets deterministic producers (capability invocation)
	// read the authoritative ActionCall behind their tool entry.
	models.ActionCallReader
}
