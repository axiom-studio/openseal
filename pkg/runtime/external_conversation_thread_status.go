package runtime

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/axiom-studio/openseal/pkg/capability"
)

// Native working state is independent of public chat updates. Providers map
// the lifecycle enum to their own UI; the optional wording comes from the
// Agent's existing public commentary, never a second model call or a template.
func (w *RunProgressAcknowledgementWorker) projectThreadStatus(ctx context.Context, item *ExternalConversationInboxItem) (*ExternalConversationDelivery, error) {
	if item == nil || item.RunID == "" || item.Status != ExternalConversationInboxApplied {
		return nil, nil
	}
	run, err := w.store.GetAgentRun(ctx, item.Scope, item.RunID)
	if err != nil || run == nil || run.Status == AgentRunStatusCompleted || run.Status == AgentRunStatusFailed || externalInboxItemIsFollowUp(item, run) {
		return nil, err // Finished replies clear state after their answer or failure notice is delivered.
	}
	endpoint, adapter, err := w.resolveEndpoint(ctx, item)
	if err != nil || endpoint == nil {
		return nil, err
	}
	thread := strings.TrimSpace(item.Event.ExternalThreadID)
	if thread == "" && endpoint.Policy.ReplyMode == ExternalConversationReplyThread {
		thread = strings.TrimSpace(item.Event.ExternalMessageID)
	}
	if thread == "" || !containsConversationDeliveryOperation(adapter.Adapter.Delivery.Operations, capability.ConversationDeliveryTypingIndicator) {
		return nil, nil
	}
	state := "processing"
	if isTerminalAgentRunStatus(run.Status) {
		busy, err := externalThreadHasActiveWork(ctx, w.store, run)
		if err != nil || busy {
			return nil, err
		}
		state = "active"
	} else if run.Status == AgentRunStatusWaitingForApproval {
		state = "suspended"
	}
	text, eventID := "", ""
	if state != "active" {
		events, err := w.store.ListActivity(ctx, ActivityFilter{Scope: item.Scope, RunID: run.ID, EventTypes: []string{"turn.commentary"}, Visibilities: []ActivityVisibility{ActivityVisibilityScope, ActivityVisibilityTeam}, Descending: true, Limit: 1})
		if err != nil {
			return nil, err
		}
		if len(events) > 0 {
			text, eventID = strings.TrimSpace(events[0].Summary), events[0].ID
			if len(text) > 100 {
				text = text[:100]
				for !utf8.ValidString(text) {
					text = text[:len(text)-1]
				}
			}
		}
	}
	result, err := w.transport.Enqueue(ctx, EnqueueExternalConversationDeliveryRequest{
		Scope: item.Scope, EndpointID: endpoint.ID, Operation: capability.ConversationDeliveryTypingIndicator,
		ConversationID: item.ConversationID, ChannelMessageID: item.ChannelMessageID,
		ExternalConversationID: item.Event.ExternalConversationID, ExternalThreadID: thread,
		Parameters:     map[string]interface{}{"state": state, "status": text},
		IdempotencyKey: "run-thread-status:" + item.ID + ":" + state + ":" + eventID,
	})
	if err != nil {
		return nil, err
	}
	return result.Delivery, nil
}

func externalThreadHasActiveWork(ctx context.Context, store PortfolioStore, run *AgentRun) (bool, error) {
	for offset := 0; ; offset += 100 {
		runs, err := store.ListAgentRuns(ctx, AgentRunFilter{Scope: run.Scope, Owner: &run.Owner, ConcurrencyKey: run.ConcurrencyKey, Limit: 100, Offset: offset})
		if err != nil {
			return false, err
		}
		for _, candidate := range runs {
			if !isTerminalAgentRunStatus(candidate.Status) {
				return true, nil
			}
		}
		if len(runs) < 100 {
			return false, nil
		}
	}
}
