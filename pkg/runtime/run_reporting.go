package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
)

func runReportingChannelKey(owner models.ObjectiveOwner, policy *runbook.ReportingPolicy) string {
	if policy == nil {
		return ""
	}
	return "run-reporting-channel:" + string(owner.Type) + ":" + owner.ID + ":" + policy.Channel
}

func prepareRunReporting(
	ctx context.Context,
	store models.ConversationStore,
	scope models.Scope,
	owner models.ObjectiveOwner,
	policy *runbook.ReportingPolicy,
	person string,
	runID string,
	contextValues map[string]interface{},
) (*models.Conversation, string, error) {
	if policy == nil {
		return nil, "", nil
	}
	if store == nil {
		return nil, "", errors.New("run reporting requires a conversation store")
	}
	channel, err := ensureRunReportingChannel(ctx, store, scope, owner, policy, person)
	if err != nil {
		return nil, "", err
	}
	messageKey := "run-reporting-start:" + runID
	messageID := models.StableConversationID(scope, messageKey, "message")
	contextValues[models.ConversationRunConversationIDContextKey] = channel.ID
	contextValues[models.ConversationRunTriggerIDContextKey] = messageID
	contextValues[models.RunReportingRootRunIDContextKey] = runID
	milestones := make([]interface{}, len(policy.Milestones))
	for index, milestone := range policy.Milestones {
		milestones[index] = string(milestone)
	}
	contextValues[models.RunReportingMilestonesContextKey] = milestones
	return channel, messageKey, nil
}

// ensureRunReportingChannel materializes the durable work channel selected by
// an active Runbook. Activations and Runs both call this boundary so accepted
// compositions expose their channel immediately while execution safely reuses
// the same owner-scoped Conversation. person is the one the activation acts
// for; the channel is created for them.
func ensureRunReportingChannel(
	ctx context.Context,
	store models.ConversationStore,
	scope models.Scope,
	owner models.ObjectiveOwner,
	policy *runbook.ReportingPolicy,
	person string,
) (*models.Conversation, error) {
	if policy == nil {
		return nil, nil
	}
	if store == nil {
		return nil, errors.New("run reporting requires a conversation store")
	}
	channel, _, err := conversation.NewConversationService(store).CreateConversation(ctx, conversation.CreateConversationRequest{
		Scope: scope, Owner: owner, Title: strings.TrimSpace(policy.Title), CreatedBy: person,
		IdempotencyKey: runReportingChannelKey(owner, policy),
	})
	return channel, err
}

func projectRunReportingStart(
	ctx context.Context,
	store models.ConversationStore,
	channel *models.Conversation,
	messageKey string,
	run *models.AgentRun,
) error {
	if store == nil || channel == nil || run == nil || messageKey == "" || !models.RunReportsMilestone(run, runbook.ReportingStarted) {
		return nil
	}
	service := conversation.NewConversationService(store)
	_, err := service.PostChannelMessage(ctx, conversation.PostChannelMessageRequest{
		Scope: channel.Scope, ConversationID: channel.ID, ExpectedRevision: channel.Revision,
		Sender:            models.ConversationParticipant{Type: models.ConversationParticipantAgent, ID: run.AssignedAgentID},
		SenderDisplayName: "Agent", Intent: models.MessageIntentUpdate,
		Content: "Started: " + strings.TrimSpace(run.Goal), Audience: models.ConversationAudience{Kind: models.ConversationAudienceChannel},
		References:     []models.ConversationReference{{Kind: models.ConversationReferenceRun, ID: run.ID}},
		IdempotencyKey: messageKey,
	})
	if errors.Is(err, models.ErrRevisionConflict) {
		latest, getErr := service.GetConversation(ctx, channel.Scope, channel.ID)
		if getErr != nil {
			return getErr
		}
		_, err = service.PostChannelMessage(ctx, conversation.PostChannelMessageRequest{
			Scope: latest.Scope, ConversationID: latest.ID, ExpectedRevision: latest.Revision,
			Sender:            models.ConversationParticipant{Type: models.ConversationParticipantAgent, ID: run.AssignedAgentID},
			SenderDisplayName: "Agent", Intent: models.MessageIntentUpdate,
			Content: "Started: " + strings.TrimSpace(run.Goal), Audience: models.ConversationAudience{Kind: models.ConversationAudienceChannel},
			References:     []models.ConversationReference{{Kind: models.ConversationReferenceRun, ID: run.ID}},
			IdempotencyKey: messageKey,
		})
	}
	return err
}

// projectConversationRunbookStart acknowledges an on-demand operation as soon
// as its durable child Run exists. The conversation parent may remain blocked
// on work or approval for hours; users must not need to send the command again
// merely to learn whether it started. A Run may start several operations, so
// the message is keyed per child.
func projectConversationRunbookStart(ctx context.Context, store models.ConversationStore, source *models.AgentRun, summary string, child *models.AgentRun) error {
	summary = strings.TrimSpace(summary)
	if store == nil || source == nil || summary == "" || child == nil || source.Kind != models.RunKindConversation {
		return nil
	}
	conversationID, _ := source.Context[models.ConversationRunConversationIDContextKey].(string)
	triggerID, _ := source.Context[models.ConversationRunTriggerIDContextKey].(string)
	conversationID, triggerID = strings.TrimSpace(conversationID), strings.TrimSpace(triggerID)
	if conversationID == "" || triggerID == "" {
		return nil
	}
	service := conversation.NewConversationService(store)
	channel, err := service.GetConversation(ctx, source.Scope, conversationID)
	if err != nil {
		return err
	}
	post := func(current *models.Conversation) error {
		_, postErr := service.PostChannelMessage(ctx, conversation.PostChannelMessageRequest{
			Scope: current.Scope, ConversationID: current.ID, ExpectedRevision: current.Revision,
			Sender:            models.ConversationParticipant{Type: models.ConversationParticipantAgent, ID: source.AssignedAgentID},
			SenderDisplayName: "Agent", Intent: models.MessageIntentUpdate,
			Content:  "Started: " + summary,
			Audience: models.ConversationAudience{Kind: models.ConversationAudienceChannel}, ReplyToMessageID: triggerID,
			References:     []models.ConversationReference{{Kind: models.ConversationReferenceRun, ID: child.ID}},
			IdempotencyKey: "conversation-runbook-start:" + source.ID + ":" + child.ID,
		})
		return postErr
	}
	if err = post(channel); errors.Is(err, models.ErrRevisionConflict) {
		channel, err = service.GetConversation(ctx, source.Scope, conversationID)
		if err != nil {
			return err
		}
		err = post(channel)
	}
	return err
}

func projectTerminalRunReporting(ctx context.Context, store models.ConversationStore, run *models.AgentRun) error {
	if store == nil || run == nil || !models.IsTerminalAgentRunStatus(run.Status) {
		return nil
	}
	if models.FailedConversationReportCandidate(run) {
		return projectFailedConversationReply(ctx, store, run)
	}
	if run.Kind == models.RunKindConversation && run.Context[models.ConversationTaskContextKey] != nil {
		task, sourceReport, err := conversation.ConversationTaskForReport(ctx, store, run)
		if err != nil {
			return err
		}
		if task == nil || sourceReport || task.Mode != models.ConversationTaskModeContinuation {
			return models.ErrInvalidConversationTask
		}
		return projectConversationTaskContinuationResult(ctx, store, task, run)
	}
	rootRunID, _ := run.Context[models.RunReportingRootRunIDContextKey].(string)
	if strings.TrimSpace(rootRunID) == "" || strings.TrimSpace(rootRunID) != run.ID {
		return nil
	}
	if silentRoutineOccurrence(run) {
		// The occurrence found nothing for the person and changed nothing;
		// the UI shows it as a quiet line from the Run itself.
		return nil
	}
	conversationID, _ := run.Context[models.ConversationRunConversationIDContextKey].(string)
	rootMessageID, _ := run.Context[models.ConversationRunTriggerIDContextKey].(string)
	conversationID, rootMessageID = strings.TrimSpace(conversationID), strings.TrimSpace(rootMessageID)
	if conversationID == "" || rootMessageID == "" {
		return nil
	}
	milestone := runbook.ReportingCompleted
	content := "Completed: " + strings.TrimSpace(run.Goal)
	if run.Status != models.AgentRunStatusCompleted {
		milestone = runbook.ReportingFailed
		content = fmt.Sprintf("%s: %s", terminalRunStatusLabel(run.Status), strings.TrimSpace(run.Goal))
		if run.Status == models.AgentRunStatusFailed {
			content = conversation.TerminalRunFailureReply(ctx, store, store, run)
		}
	}
	if task, _ := run.Context[models.ScheduledTaskContextKey].(bool); task && run.Status == models.AgentRunStatusCompleted {
		if summary, _ := run.Output["summary"].(string); strings.TrimSpace(summary) != "" {
			content = strings.TrimSpace(summary)
		}
	}
	if !models.RunReportsMilestone(run, milestone) {
		return nil
	}
	task, sourceTaskReport, taskErr := conversation.ConversationTaskForReport(ctx, store, run)
	if taskErr != nil {
		return taskErr
	}
	intent := models.MessageIntentUpdate
	audience := models.ConversationAudience{Kind: models.ConversationAudienceChannel}
	broadcast := true
	resolvesMessageID := ""
	responseMode := ""
	references := []models.ConversationReference{{Kind: models.ConversationReferenceRun, ID: run.ID}}
	if task != nil {
		if !sourceTaskReport {
			portfolio, ok := store.(models.PortfolioStore)
			if !ok {
				return models.ErrInvalidConversationTask
			}
			sourceRun, err := portfolio.GetAgentRun(ctx, task.Scope, task.SourceRunID)
			if err != nil {
				return err
			}
			if sourceRun == nil || sourceRun.Status != models.AgentRunStatusCompleted {
				return models.ErrInvalidConversationTask
			}
			verified, sourceReport, err := conversation.ConversationTaskForReport(ctx, store, sourceRun)
			if err != nil || verified == nil || verified.ID != task.ID || !sourceReport {
				if err != nil {
					return err
				}
				return models.ErrInvalidConversationTask
			}
			// Source and work share the existing outbox, whose concurrent claims
			// may arrive in either order. Commit the idempotent acknowledgment
			// before the final result so a fast task cannot speak in reverse.
			if err := projectTerminalRunReporting(ctx, store, sourceRun); err != nil {
				return err
			}
		}
		source, err := store.GetChannelMessage(ctx, task.Scope, task.ConversationID, task.SourceMessageID)
		if err != nil {
			return err
		}
		if source == nil {
			return models.ErrChannelMessageNotFound
		}
		responseMode = source.ResponseMode
		// Visibility inherits the canonical source and its thread ancestors.
		// Channel here preserves the original sender's implicit access to a
		// directed question without widening access past those ancestors.
		audience = models.ConversationAudience{Kind: models.ConversationAudienceChannel}
		broadcast = source.ThreadRootID == "" || source.BroadcastToChannel
		content = conversationTaskReportContent(ctx, store, task, sourceTaskReport, run, content)
		intent = models.MessageIntentAnswer
		references = append(references, models.ConversationReference{Kind: models.ConversationReferenceTask, ID: task.ID})
		if sourceTaskReport {
			resolvesMessageID = task.SourceMessageID
			references = append(references, models.ConversationReference{Kind: models.ConversationReferenceRun, ID: task.WorkRunID})
		}
	}
	replyTo := rootMessageID
	if task == nil && routineOccurrenceRun(run) {
		// Each routine occurrence reports as a new top-level message in the
		// chat the routine was created from, not inside the creating thread.
		replyTo, broadcast = "", false
	}
	service := conversation.NewConversationService(store)
	channel, err := service.GetConversation(ctx, run.Scope, conversationID)
	if err != nil {
		return err
	}
	request := conversation.PostChannelMessageRequest{
		Scope: run.Scope, ConversationID: channel.ID, ExpectedRevision: channel.Revision,
		Sender:            models.ConversationParticipant{Type: models.ConversationParticipantAgent, ID: run.AssignedAgentID},
		SenderDisplayName: "Agent", Intent: intent, Content: content, ResolvesMessageID: resolvesMessageID,
		Audience: audience, ReplyToMessageID: replyTo, BroadcastToChannel: broadcast,
		ResponseMode:   responseMode,
		References:     references,
		IdempotencyKey: "run-reporting-terminal:" + run.ID + ":" + string(run.Status),
	}
	_, err = service.PostChannelMessage(ctx, request)
	if errors.Is(err, models.ErrRevisionConflict) {
		latest, getErr := service.GetConversation(ctx, run.Scope, conversationID)
		if getErr != nil {
			return getErr
		}
		request.ExpectedRevision = latest.Revision
		_, err = service.PostChannelMessage(ctx, request)
	}
	if errors.Is(err, models.ErrMessageConflict) {
		if saved, replayErr := service.ReplayLegacySpokenResponse(ctx, request); replayErr != nil {
			return replayErr
		} else if saved != nil {
			return nil
		}
	}
	return err
}

// routineOccurrenceRun reports whether run is an occurrence of a routine.
func routineOccurrenceRun(run *models.AgentRun) bool {
	return run != nil && run.Context[models.ScheduledTaskContextKey] == true
}

// silentRoutineOccurrence reports a completed routine occurrence that
// finished silent: it posts nothing in the chat or any mirror of it.
func silentRoutineOccurrence(run *models.AgentRun) bool {
	if !routineOccurrenceRun(run) || run.Kind == models.RunKindConversation || run.Status != models.AgentRunStatusCompleted {
		return false
	}
	silent, _ := run.Output[agentrun.FinalOutputSilentKey].(bool)
	return silent
}

func terminalRunStatusLabel(status models.AgentRunStatus) string {
	label := strings.ReplaceAll(strings.TrimSpace(string(status)), "_", " ")
	if label == "" {
		return "Stopped"
	}
	runes := []rune(label)
	runes[0] = []rune(strings.ToUpper(string(runes[0])))[0]
	return string(runes)
}

func projectRunReportingStartForRun(ctx context.Context, store models.ConversationStore, run *models.AgentRun) error {
	if store == nil || run == nil || !models.RunReportsMilestone(run, runbook.ReportingStarted) {
		return nil
	}
	rootRunID, _ := run.Context[models.RunReportingRootRunIDContextKey].(string)
	conversationID, _ := run.Context[models.ConversationRunConversationIDContextKey].(string)
	if strings.TrimSpace(rootRunID) != run.ID || strings.TrimSpace(conversationID) == "" {
		return nil
	}
	channel, err := conversation.NewConversationService(store).GetConversation(ctx, run.Scope, conversationID)
	if err != nil {
		return err
	}
	return projectRunReportingStart(ctx, store, channel, "run-reporting-start:"+run.ID, run)
}
