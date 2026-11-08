package runtime

import (
	"context"
	"errors"
	"testing"
	"time"
)

type agentBuilderDraftFake struct {
	conversations []*Conversation
	messages      map[string][]*ChannelMessage // ascending
	runs          map[string][]*AgentRun       // newest first
	approvals     map[string][]*ApprovalCheckpoint
	calls         map[string]*ActionCall
	filters       []ConversationFilter
}

func (f *agentBuilderDraftFake) ListConversations(_ context.Context, filter ConversationFilter) ([]*Conversation, error) {
	f.filters = append(f.filters, filter)
	return pageConversations(append([]*Conversation(nil), f.conversations...), filter.Offset, filter.Limit), nil
}

func (f *agentBuilderDraftFake) ListChannelMessages(_ context.Context, filter ChannelMessageFilter) ([]*ChannelMessage, error) {
	values := append([]*ChannelMessage(nil), f.messages[filter.ConversationID]...)
	if filter.Descending {
		for i, j := 0, len(values)-1; i < j; i, j = i+1, j-1 {
			values[i], values[j] = values[j], values[i]
		}
	}
	if filter.Limit > 0 && len(values) > filter.Limit {
		values = values[:filter.Limit]
	}
	return values, nil
}

func (f *agentBuilderDraftFake) ListAgentRuns(_ context.Context, filter AgentRunFilter) ([]*AgentRun, error) {
	if filter.AssignedAgentID != AgentBuilderDeploymentID || filter.Order != AgentRunOrderCreatedDesc {
		return nil, errors.New("unexpected run filter")
	}
	return f.runs[filter.ConversationID], nil
}

func (f *agentBuilderDraftFake) ListApprovals(_ context.Context, filter ApprovalFilter) ([]*ApprovalCheckpoint, error) {
	if !filter.NewestFirst {
		return nil, errors.New("unexpected approval order")
	}
	return f.approvals[filter.ConversationID], nil
}

func (f *agentBuilderDraftFake) GetActionCall(_ context.Context, _ Scope, id string) (*ActionCall, error) {
	if call := f.calls[id]; call != nil {
		return call, nil
	}
	return nil, ErrActionNotFound
}

var (
	agentBuilderDraftScope     = Scope{Kind: "tenant", ID: "one"}
	agentBuilderDraftRequester = ConversationParticipant{Type: ConversationParticipantUser, ID: "ana"}
	agentBuilderDraftPrincipal = ApprovalPrincipal{Type: "user", ID: "ana"}
)

func (f *agentBuilderDraftFake) add(id string, updated time.Time, sender ConversationParticipant) {
	if f.messages == nil {
		f.messages, f.runs, f.approvals, f.calls = map[string][]*ChannelMessage{}, map[string][]*AgentRun{}, map[string][]*ApprovalCheckpoint{}, map[string]*ActionCall{}
	}
	f.conversations = append(f.conversations, &Conversation{
		ID: id, Scope: agentBuilderDraftScope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: AgentBuilderDeploymentID},
		Title: "New agent", Status: ConversationStatusActive, UpdatedAt: updated, SpaceID: "space-" + id,
		Origin: &ConversationReference{Kind: ConversationReferenceAgentBuilder, ID: id},
	})
	f.messages[id] = []*ChannelMessage{{
		ID: id + "-m1", ConversationID: id, Sequence: 1, Sender: sender, Content: "Build a press agent",
		BuilderParameters: &AgentBuilderParameters{Kind: AgentBuilderKindAgent, Name: "Press " + id},
	}}
}

func agentBuilderCard(id, operation string, status ApprovalStatus, approvers ...ApprovalPrincipal) *ApprovalCheckpoint {
	return &ApprovalCheckpoint{
		ID: id, ActionCallID: id + "-call", Status: status, EligibleApprovers: approvers,
		ProposedAction: map[string]interface{}{
			"resourceType": agentProfileResourceType, "operation": operation,
			"agent": map[string]interface{}{"displayName": "Card " + id},
		},
	}
}

func TestAgentBuilderDraftsStatusesAndFiltering(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	fake := &agentBuilderDraftFake{}
	agent := ConversationParticipant{Type: ConversationParticipantAgent, ID: AgentBuilderDeploymentID}
	at := func(i int) time.Time { return now.Add(-time.Duration(i) * time.Minute) }

	fake.add("drafting", at(0), agentBuilderDraftRequester)
	fake.runs["drafting"] = []*AgentRun{
		{ID: "child", ParentRunID: "run-drafting", Status: AgentRunStatusFailed},
		{ID: "run-drafting", Status: AgentRunStatusRunning},
	}
	fake.approvals["drafting"] = []*ApprovalCheckpoint{agentBuilderCard("drafting-card", AgentActionCreateAgent, ApprovalStatusPending, agentBuilderDraftPrincipal)}

	fake.add("ready", at(1), agentBuilderDraftRequester)
	fake.runs["ready"] = []*AgentRun{{ID: "run-ready", Status: AgentRunStatusWaitingForApproval}}
	fake.approvals["ready"] = []*ApprovalCheckpoint{
		{ID: "unrelated", Status: ApprovalStatusPending, ProposedAction: map[string]interface{}{"resourceType": "skill_binding"}},
		agentBuilderCard("ready-card", AgentActionCreateAgent, ApprovalStatusPending, agentBuilderDraftPrincipal),
	}

	fake.add("needs-approval", at(2), agentBuilderDraftRequester)
	fake.runs["needs-approval"] = []*AgentRun{{ID: "run-na", Status: AgentRunStatusWaitingForApproval}}
	fake.approvals["needs-approval"] = []*ApprovalCheckpoint{agentBuilderCard("na-card", AgentActionUpdateAgent, ApprovalStatusPending, ApprovalPrincipal{Type: "user", ID: "boss"})}

	fake.add("needs-input", at(3), agentBuilderDraftRequester)
	fake.runs["needs-input"] = []*AgentRun{{ID: "run-ni", Status: AgentRunStatusWaitingForEvent}}
	fake.messages["needs-input"] = append(fake.messages["needs-input"],
		&ChannelMessage{ID: "q1", Sequence: 2, Sender: agent, Intent: MessageIntentQuestion, Content: "Which tone?"},
		&ChannelMessage{ID: "status", Sequence: 3, Sender: agent, Intent: MessageIntentUpdate, Content: "Waiting"})

	fake.add("answered", at(4), agentBuilderDraftRequester)
	fake.messages["answered"] = append(fake.messages["answered"],
		&ChannelMessage{ID: "q2", Sequence: 2, Sender: agent, Intent: MessageIntentQuestion, Content: "Which tone?"},
		&ChannelMessage{ID: "a2", Sequence: 3, Sender: agentBuilderDraftRequester, Intent: MessageIntentAnswer, Content: "Warm",
			BuilderParameters: &AgentBuilderParameters{Kind: AgentBuilderKindAgent, Name: "Renamed"}})

	fake.add("stopped", at(5), agentBuilderDraftRequester)
	fake.runs["stopped"] = []*AgentRun{{ID: "run-stopped", Status: AgentRunStatusCanceled}}

	fake.add("failed", at(6), agentBuilderDraftRequester)
	fake.runs["failed"] = []*AgentRun{{ID: "run-failed", Status: AgentRunStatusFailed}}
	fake.approvals["failed"] = []*ApprovalCheckpoint{agentBuilderCard("failed-card", AgentActionCreateAgent, ApprovalStatusApproved, agentBuilderDraftPrincipal)}
	fake.calls["failed-card-call"] = &ActionCall{ID: "failed-card-call", Status: ActionCallStatusFailed}

	fake.add("created", at(7), agentBuilderDraftRequester)
	fake.runs["created"] = []*AgentRun{{ID: "run-created", Status: AgentRunStatusCompleted}}
	fake.approvals["created"] = []*ApprovalCheckpoint{
		agentBuilderCard("later-update", AgentActionUpdateAgent, ApprovalStatusPending, agentBuilderDraftPrincipal),
		agentBuilderCard("created-card", AgentActionCreateAgent, ApprovalStatusApproved, agentBuilderDraftPrincipal),
	}
	fake.calls["created-card-call"] = &ActionCall{ID: "created-card-call", Status: ActionCallStatusSucceeded}

	fake.add("someone-else", at(8), ConversationParticipant{Type: ConversationParticipantUser, ID: "bo"})
	fake.add("archived", at(9), agentBuilderDraftRequester)
	fake.conversations[len(fake.conversations)-1].Status = ConversationStatusArchived

	drafts, err := ListAgentBuilderDrafts(t.Context(), fake, AgentBuilderDraftFilter{
		Scope: agentBuilderDraftScope, Requester: agentBuilderDraftRequester, Principal: agentBuilderDraftPrincipal,
	})
	if err != nil {
		t.Fatal(err)
	}
	type want struct {
		status            AgentBuilderDraftStatus
		title, run, card  string
		questionMessageID string
	}
	wants := map[string]want{
		"drafting":       {AgentBuilderDraftDrafting, "Card drafting-card", "run-drafting", "drafting-card", ""},
		"ready":          {AgentBuilderDraftReady, "Card ready-card", "run-ready", "ready-card", ""},
		"needs-approval": {AgentBuilderDraftNeedsApproval, "Card na-card", "run-na", "na-card", ""},
		"needs-input":    {AgentBuilderDraftNeedsInput, "Press needs-input", "run-ni", "", "q1"},
		"answered":       {AgentBuilderDraftIdle, "Renamed", "", "", ""},
		"stopped":        {AgentBuilderDraftStopped, "Press stopped", "run-stopped", "", ""},
		"failed":         {AgentBuilderDraftFailed, "Card failed-card", "run-failed", "", ""},
	}
	order := []string{"drafting", "ready", "needs-approval", "needs-input", "answered", "stopped", "failed"}
	if len(drafts) != len(order) {
		t.Fatalf("drafts = %#v", drafts)
	}
	for index, draft := range drafts {
		id := order[index]
		expected := wants[id]
		card := ""
		if draft.Approval != nil {
			card = draft.Approval.ID
		}
		if draft.ConversationID != id || draft.SpaceID != "space-"+id || draft.Status != expected.status || draft.Title != expected.title || draft.RunID != expected.run ||
			card != expected.card || draft.QuestionMessageID != expected.questionMessageID {
			t.Fatalf("draft %d = %#v, want %s %#v", index, draft, id, expected)
		}
	}
	filter := fake.filters[0]
	if filter.OriginKind != ConversationReferenceAgentBuilder || filter.Owner == nil || filter.Owner.ID != AgentBuilderDeploymentID ||
		len(filter.Statuses) != 1 || filter.Statuses[0] != ConversationStatusActive {
		t.Fatalf("conversation filter = %#v", filter)
	}

	page, err := ListAgentBuilderDrafts(t.Context(), fake, AgentBuilderDraftFilter{
		Scope: agentBuilderDraftScope, Requester: agentBuilderDraftRequester, Principal: agentBuilderDraftPrincipal, Limit: 2, Offset: 3,
	})
	if err != nil || len(page) != 2 || page[0].ConversationID != "needs-input" || page[1].ConversationID != "answered" {
		t.Fatalf("page = %#v, %v", page, err)
	}
	for _, bad := range []AgentBuilderDraftFilter{
		{Scope: agentBuilderDraftScope, Requester: agentBuilderDraftRequester, Limit: 101},
		{Scope: agentBuilderDraftScope, Requester: agentBuilderDraftRequester, Offset: -1},
		{Scope: agentBuilderDraftScope, Requester: ConversationParticipant{Type: ConversationParticipantAgent, ID: "x"}},
	} {
		if _, err := ListAgentBuilderDrafts(t.Context(), fake, bad); !errors.Is(err, ErrInvalidConversation) {
			t.Fatalf("filter %#v accepted: %v", bad, err)
		}
	}
}

func TestAgentBuilderDraftsScanPastFullPages(t *testing.T) {
	fake := &agentBuilderDraftFake{}
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	for index := 0; index < agentBuilderDraftScanPage+5; index++ {
		sender := ConversationParticipant{Type: ConversationParticipantUser, ID: "bo"}
		if index >= agentBuilderDraftScanPage {
			sender = agentBuilderDraftRequester
		}
		fake.add("c"+time.Duration(index).String(), now.Add(-time.Duration(index)*time.Second), sender)
	}
	drafts, err := ListAgentBuilderDrafts(t.Context(), fake, AgentBuilderDraftFilter{Scope: agentBuilderDraftScope, Requester: agentBuilderDraftRequester})
	if err != nil || len(drafts) != 5 || len(fake.filters) != 2 || fake.filters[1].Offset != agentBuilderDraftScanPage {
		t.Fatalf("drafts = %d, filters = %#v, %v", len(drafts), fake.filters, err)
	}
}
