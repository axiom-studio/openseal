package externalconversation

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/conversationrun"
)

// Native working state is independent of public chat updates. Providers map
// the lifecycle enum to their own UI; the optional wording comes from the
// Agent's existing public commentary, never a second model call or a template.
// agents are every Agent answering the item, whose lanes keep the thread busy.
func (w *RunProgressAcknowledgementWorker) projectThreadStatus(ctx context.Context, item *models.ExternalConversationInboxItem, reply externalItemReply, agents []string) (*models.ExternalConversationDelivery, error) {
	if item == nil || item.RunID == "" || item.Status != models.ExternalConversationInboxApplied {
		return nil, nil
	}
	run := reply.run
	if run.Status == models.AgentRunStatusCompleted || run.Status == models.AgentRunStatusFailed || conversationrun.ExternalInboxItemIsFollowUp(item, run) {
		return nil, nil // Finished replies clear state after their answer or failure notice is delivered.
	}
	endpoint, adapter, err := w.resolveEndpoint(ctx, item)
	if err != nil || endpoint == nil {
		return nil, err
	}
	thread := strings.TrimSpace(item.Event.ExternalThreadID)
	if thread == "" && endpoint.Policy.ReplyMode == models.ExternalConversationReplyThread {
		thread = strings.TrimSpace(item.Event.ExternalMessageID)
	}
	if thread == "" || !containsConversationDeliveryOperation(adapter.Adapter.Delivery.Operations, capability.ConversationDeliveryTypingIndicator) {
		return nil, nil
	}
	state := "processing"
	if models.IsTerminalAgentRunStatus(run.Status) {
		busy, err := externalThreadHasActiveWork(ctx, w.store, run, agents...)
		if err != nil || busy {
			return nil, err
		}
		state = "active"
	} else if run.Status == models.AgentRunStatusWaitingForApproval {
		state = "suspended"
	}
	text, eventID := "", ""
	if state != "active" {
		events, err := w.store.ListActivity(ctx, models.ActivityFilter{Scope: item.Scope, RunID: run.ID, EventTypes: []string{"turn.commentary"}, Visibilities: []models.ActivityVisibility{models.ActivityVisibilityScope, models.ActivityVisibilityTeam}, Descending: true, Limit: 1})
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
		IdempotencyKey: "run-thread-status:" + reply.id + ":" + state + ":" + eventID,
	})
	if err != nil {
		return nil, err
	}
	return result.Delivery, nil
}

// externalThreadHasActiveWork reports an unfinished Run in run's lane. A
// Team thread has a routing lane and one lane per member, so those of the
// current roster and of the answering members are all checked.
func externalThreadHasActiveWork(ctx context.Context, store models.PortfolioStore, run *models.AgentRun, members ...string) (bool, error) {
	lanes := []string{run.ConcurrencyKey}
	if run.Kind == models.RunKindConversation && run.Owner.Type == models.OwnerTypeTeam {
		conversationID, _ := run.Context[models.ConversationRunConversationIDContextKey].(string)
		root, _ := run.Context["threadRootMessageId"].(string)
		roster, _, err := models.TeamRoster(ctx, models.TeamRosterReaderOf(store), run.Scope, run.Owner.ID)
		if err != nil {
			return false, err
		}
		lanes = append(lanes, models.ConversationRunLane(conversationID, root, ""))
		for _, member := range append(roster, members...) {
			lanes = append(lanes, models.ConversationRunLane(conversationID, root, member))
		}
	}
	checked := make(map[string]bool, len(lanes))
	for _, lane := range lanes {
		if checked[lane] {
			continue
		}
		checked[lane] = true
		for offset := 0; ; offset += 100 {
			runs, err := store.ListAgentRuns(ctx, models.AgentRunFilter{Scope: run.Scope, Owner: &run.Owner, ConcurrencyKey: lane, Limit: 100, Offset: offset})
			if err != nil {
				return false, err
			}
			for _, candidate := range runs {
				if !models.IsTerminalAgentRunStatus(candidate.Status) {
					return true, nil
				}
			}
			if len(runs) < 100 {
				break
			}
		}
	}
	return false, nil
}
