package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/axiom-studio/openseal/pkg/runbook"
)

const runReportingContextMilestones = "reportingMilestones"

const runReportingContextRootRunID = "reportingRootRunId"

func runReportingChannelKey(owner ObjectiveOwner, policy *runbook.ReportingPolicy) string {
	if policy == nil {
		return ""
	}
	return "run-reporting-channel:" + string(owner.Type) + ":" + owner.ID + ":" + policy.Channel
}

func prepareRunReporting(
	ctx context.Context,
	store ConversationStore,
	scope Scope,
	owner ObjectiveOwner,
	policy *runbook.ReportingPolicy,
	runID string,
	contextValues map[string]interface{},
) (*Conversation, string, error) {
	if policy == nil {
		return nil, "", nil
	}
	if store == nil {
		return nil, "", errors.New("run reporting requires a conversation store")
	}
	channel, err := ensureRunReportingChannel(ctx, store, scope, owner, policy)
	if err != nil {
		return nil, "", err
	}
	messageKey := "run-reporting-start:" + runID
	messageID := stableConversationID(scope, messageKey, "message")
	contextValues[conversationRunContextConversationID] = channel.ID
	contextValues[conversationRunContextTriggerID] = messageID
	contextValues[runReportingContextRootRunID] = runID
	milestones := make([]interface{}, len(policy.Milestones))
	for index, milestone := range policy.Milestones {
		milestones[index] = string(milestone)
	}
	contextValues[runReportingContextMilestones] = milestones
	return channel, messageKey, nil
}

// ensureRunReportingChannel materializes the durable work channel selected by
// an active Runbook. Activations and Runs both call this boundary so accepted
// compositions expose their channel immediately while execution safely reuses
// the same owner-scoped Conversation.
func ensureRunReportingChannel(
	ctx context.Context,
	store ConversationStore,
	scope Scope,
	owner ObjectiveOwner,
	policy *runbook.ReportingPolicy,
) (*Conversation, error) {
	if policy == nil {
		return nil, nil
	}
	if store == nil {
		return nil, errors.New("run reporting requires a conversation store")
	}
	channel, _, err := NewConversationService(store).CreateConversation(ctx, CreateConversationRequest{
		Scope: scope, Owner: owner, Title: strings.TrimSpace(policy.Title),
		IdempotencyKey: runReportingChannelKey(owner, policy),
	})
	return channel, err
}

func projectRunReportingStart(
	ctx context.Context,
	store ConversationStore,
	channel *Conversation,
	messageKey string,
	run *AgentRun,
) error {
	if store == nil || channel == nil || run == nil || messageKey == "" || !runReportsMilestone(run, runbook.ReportingStarted) {
		return nil
	}
	service := NewConversationService(store)
	_, err := service.PostChannelMessage(ctx, PostChannelMessageRequest{
		Scope: channel.Scope, ConversationID: channel.ID, ExpectedRevision: channel.Revision,
		Sender:            ConversationParticipant{Type: ConversationParticipantAgent, ID: run.AssignedAgentID},
		SenderDisplayName: "Agent", Intent: MessageIntentUpdate,
		Content: "Started: " + strings.TrimSpace(run.Goal), Audience: ConversationAudience{Kind: ConversationAudienceChannel},
		References:     []ConversationReference{{Kind: ConversationReferenceRun, ID: run.ID}},
		IdempotencyKey: messageKey,
	})
	if errors.Is(err, ErrRevisionConflict) {
		latest, getErr := service.GetConversation(ctx, channel.Scope, channel.ID)
		if getErr != nil {
			return getErr
		}
		_, err = service.PostChannelMessage(ctx, PostChannelMessageRequest{
			Scope: latest.Scope, ConversationID: latest.ID, ExpectedRevision: latest.Revision,
			Sender:            ConversationParticipant{Type: ConversationParticipantAgent, ID: run.AssignedAgentID},
			SenderDisplayName: "Agent", Intent: MessageIntentUpdate,
			Content: "Started: " + strings.TrimSpace(run.Goal), Audience: ConversationAudience{Kind: ConversationAudienceChannel},
			References:     []ConversationReference{{Kind: ConversationReferenceRun, ID: run.ID}},
			IdempotencyKey: messageKey,
		})
	}
	return err
}

// projectConversationRunbookStart acknowledges an on-demand operation as soon
// as its durable child Run exists. The conversation parent may remain blocked
// on work or approval for hours; users must not need to send the command again
// merely to learn whether it started.
func projectConversationRunbookStart(
	ctx context.Context,
	store ConversationStore,
	source *AgentRun,
	proposal *TurnRunbookProposal,
	child *AgentRun,
) error {
	if store == nil || source == nil || proposal == nil || child == nil || source.Kind != RunKindConversation {
		return nil
	}
	conversationID, _ := source.Context[conversationRunContextConversationID].(string)
	triggerID, _ := source.Context[conversationRunContextTriggerID].(string)
	conversationID, triggerID = strings.TrimSpace(conversationID), strings.TrimSpace(triggerID)
	if conversationID == "" || triggerID == "" {
		return nil
	}
	service := NewConversationService(store)
	channel, err := service.GetConversation(ctx, source.Scope, conversationID)
	if err != nil {
		return err
	}
	post := func(current *Conversation) error {
		_, postErr := service.PostChannelMessage(ctx, PostChannelMessageRequest{
			Scope: current.Scope, ConversationID: current.ID, ExpectedRevision: current.Revision,
			Sender:            ConversationParticipant{Type: ConversationParticipantAgent, ID: source.AssignedAgentID},
			SenderDisplayName: "Agent", Intent: MessageIntentUpdate,
			Content:  "Started: " + strings.TrimSpace(proposal.Summary),
			Audience: ConversationAudience{Kind: ConversationAudienceChannel}, ReplyToMessageID: triggerID,
			References:     []ConversationReference{{Kind: ConversationReferenceRun, ID: child.ID}},
			IdempotencyKey: "conversation-runbook-start:" + source.ID,
		})
		return postErr
	}
	if err = post(channel); errors.Is(err, ErrRevisionConflict) {
		channel, err = service.GetConversation(ctx, source.Scope, conversationID)
		if err != nil {
			return err
		}
		err = post(channel)
	}
	return err
}

func projectTerminalRunReporting(ctx context.Context, store ConversationStore, run *AgentRun) error {
	if store == nil || run == nil || !isTerminalAgentRunStatus(run.Status) {
		return nil
	}
	rootRunID, _ := run.Context[runReportingContextRootRunID].(string)
	if strings.TrimSpace(rootRunID) == "" || strings.TrimSpace(rootRunID) != run.ID {
		return nil
	}
	conversationID, _ := run.Context[conversationRunContextConversationID].(string)
	rootMessageID, _ := run.Context[conversationRunContextTriggerID].(string)
	conversationID, rootMessageID = strings.TrimSpace(conversationID), strings.TrimSpace(rootMessageID)
	if conversationID == "" || rootMessageID == "" {
		return nil
	}
	milestone := runbook.ReportingCompleted
	content := "Completed: " + strings.TrimSpace(run.Goal)
	if run.Status != AgentRunStatusCompleted {
		milestone = runbook.ReportingFailed
		content = fmt.Sprintf("%s: %s", terminalRunStatusLabel(run.Status), strings.TrimSpace(run.Goal))
	}
	if task, _ := run.Context[scheduledTaskContextKey].(bool); task && run.Status == AgentRunStatusCompleted {
		if summary, _ := run.Output["summary"].(string); strings.TrimSpace(summary) != "" {
			content = strings.TrimSpace(summary)
		}
	}
	if !runReportsMilestone(run, milestone) {
		return nil
	}
	task, sourceTaskReport, taskErr := conversationTaskForReport(ctx, store, run)
	if taskErr != nil {
		return taskErr
	}
	intent := MessageIntentUpdate
	audience := ConversationAudience{Kind: ConversationAudienceChannel}
	broadcast := true
	resolvesMessageID := ""
	responseMode := ""
	references := []ConversationReference{{Kind: ConversationReferenceRun, ID: run.ID}}
	if task != nil {
		if !sourceTaskReport {
			portfolio, ok := store.(PortfolioStore)
			if !ok {
				return ErrInvalidConversationTask
			}
			sourceRun, err := portfolio.GetAgentRun(ctx, task.Scope, task.SourceRunID)
			if err != nil {
				return err
			}
			if sourceRun == nil || sourceRun.Status != AgentRunStatusCompleted {
				return ErrInvalidConversationTask
			}
			verified, sourceReport, err := conversationTaskForReport(ctx, store, sourceRun)
			if err != nil || verified == nil || verified.ID != task.ID || !sourceReport {
				if err != nil {
					return err
				}
				return ErrInvalidConversationTask
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
			return ErrChannelMessageNotFound
		}
		responseMode = source.ResponseMode
		// Visibility inherits the canonical source and its thread ancestors.
		// Channel here preserves the original sender's implicit access to a
		// directed question without widening access past those ancestors.
		audience = ConversationAudience{Kind: ConversationAudienceChannel}
		broadcast = source.ThreadRootID == "" || source.BroadcastToChannel
		content = conversationTaskReportContent(task, sourceTaskReport, run, content)
		intent = MessageIntentAnswer
		references = append(references, ConversationReference{Kind: ConversationReferenceTask, ID: task.ID})
		if sourceTaskReport {
			resolvesMessageID = task.SourceMessageID
			references = append(references, ConversationReference{Kind: ConversationReferenceRun, ID: task.WorkRunID})
		}
	}
	service := NewConversationService(store)
	channel, err := service.GetConversation(ctx, run.Scope, conversationID)
	if err != nil {
		return err
	}
	request := PostChannelMessageRequest{
		Scope: run.Scope, ConversationID: channel.ID, ExpectedRevision: channel.Revision,
		Sender:            ConversationParticipant{Type: ConversationParticipantAgent, ID: run.AssignedAgentID},
		SenderDisplayName: "Agent", Intent: intent, Content: content, ResolvesMessageID: resolvesMessageID,
		Audience: audience, ReplyToMessageID: rootMessageID, BroadcastToChannel: broadcast,
		ResponseMode:   responseMode,
		References:     references,
		IdempotencyKey: "run-reporting-terminal:" + run.ID + ":" + string(run.Status),
	}
	_, err = service.PostChannelMessage(ctx, request)
	if errors.Is(err, ErrRevisionConflict) {
		latest, getErr := service.GetConversation(ctx, run.Scope, conversationID)
		if getErr != nil {
			return getErr
		}
		request.ExpectedRevision = latest.Revision
		_, err = service.PostChannelMessage(ctx, request)
	}
	if errors.Is(err, ErrMessageConflict) {
		if saved, replayErr := service.replayLegacySpokenResponse(ctx, request); replayErr != nil {
			return replayErr
		} else if saved != nil {
			return nil
		}
	}
	return err
}

func runReportsMilestone(run *AgentRun, milestone runbook.ReportingMilestone) bool {
	if run == nil {
		return false
	}
	switch values := run.Context[runReportingContextMilestones].(type) {
	case []interface{}:
		for _, value := range values {
			if fmt.Sprint(value) == string(milestone) {
				return true
			}
		}
	case []string:
		for _, value := range values {
			if value == string(milestone) {
				return true
			}
		}
	}
	return false
}

func terminalRunStatusLabel(status AgentRunStatus) string {
	label := strings.ReplaceAll(strings.TrimSpace(string(status)), "_", " ")
	if label == "" {
		return "Stopped"
	}
	runes := []rune(label)
	runes[0] = []rune(strings.ToUpper(string(runes[0])))[0]
	return string(runes)
}

func projectRunReportingStartForRun(ctx context.Context, store ConversationStore, run *AgentRun) error {
	if store == nil || run == nil || !runReportsMilestone(run, runbook.ReportingStarted) {
		return nil
	}
	rootRunID, _ := run.Context[runReportingContextRootRunID].(string)
	conversationID, _ := run.Context[conversationRunContextConversationID].(string)
	if strings.TrimSpace(rootRunID) != run.ID || strings.TrimSpace(conversationID) == "" {
		return nil
	}
	channel, err := NewConversationService(store).GetConversation(ctx, run.Scope, conversationID)
	if err != nil {
		return err
	}
	return projectRunReportingStart(ctx, store, channel, "run-reporting-start:"+run.ID, run)
}
