package runtime

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/runtime/conversationrun"
)

type agentBuilderDraftFake struct {
	conversations []*Conversation
	messages      map[string][]*ChannelMessage // ascending
	runs          map[string][]*AgentRun       // newest first
	approvals     map[string][]*ApprovalCheckpoint
	created       map[string]bool // produced its Agent or Team
	filters       []ConversationFilter
	statusReads   []string // conversations whose approvals were read
}

// ListConversations selects as a store does: the person's unfinished builder
// conversations in the asked statuses.
func (f *agentBuilderDraftFake) ListConversations(_ context.Context, filter ConversationFilter) ([]*Conversation, error) {
	f.filters = append(f.filters, filter)
	selected := []*Conversation{}
	for _, conversation := range f.conversations {
		if f.messages[conversation.ID][0].Sender.ID == filter.AgentBuilderDraftOf && !f.created[conversation.ID] &&
			slices.Contains(filter.Statuses, conversation.Status) {
			selected = append(selected, conversation)
		}
	}
	if filter.Offset >= len(selected) {
		return []*Conversation{}, nil
	}
	selected = selected[filter.Offset:]
	if filter.Limit > 0 && len(selected) > filter.Limit {
		selected = selected[:filter.Limit]
	}
	return selected, nil
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
	if filter.AssignedAgentID != conversationrun.AgentBuilderDeploymentID || filter.Order != AgentRunOrderCreatedDesc {
		return nil, errors.New("unexpected run filter")
	}
	return f.runs[filter.ConversationID], nil
}

func (f *agentBuilderDraftFake) ListApprovals(_ context.Context, filter ApprovalFilter) ([]*ApprovalCheckpoint, error) {
	if !filter.NewestFirst {
		return nil, errors.New("unexpected approval order")
	}
	f.statusReads = append(f.statusReads, filter.ConversationID)
	return f.approvals[filter.ConversationID], nil
}

var (
	agentBuilderDraftScope     = Scope{Kind: "tenant", ID: "one"}
	agentBuilderDraftRequester = ConversationParticipant{Type: ConversationParticipantUser, ID: "ana"}
	agentBuilderDraftPrincipal = ApprovalPrincipal{Type: "user", ID: "ana"}
)

func (f *agentBuilderDraftFake) add(id string, updated time.Time, sender ConversationParticipant) {
	if f.messages == nil {
		f.messages, f.runs, f.approvals, f.created = map[string][]*ChannelMessage{}, map[string][]*AgentRun{}, map[string][]*ApprovalCheckpoint{}, map[string]bool{}
	}
	f.conversations = append(f.conversations, &Conversation{
		ID: id, Scope: agentBuilderDraftScope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: conversationrun.AgentBuilderDeploymentID},
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
	agent := ConversationParticipant{Type: ConversationParticipantAgent, ID: conversationrun.AgentBuilderDeploymentID}
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

	fake.add("created", at(7), agentBuilderDraftRequester)
	fake.runs["created"] = []*AgentRun{{ID: "run-created", Status: AgentRunStatusCompleted}}
	fake.approvals["created"] = []*ApprovalCheckpoint{
		agentBuilderCard("later-update", AgentActionUpdateAgent, ApprovalStatusPending, agentBuilderDraftPrincipal),
		agentBuilderCard("created-card", AgentActionCreateAgent, ApprovalStatusApproved, agentBuilderDraftPrincipal),
	}
	fake.created["created"] = true

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
	if filter.OriginKind != ConversationReferenceAgentBuilder || filter.Owner == nil || filter.Owner.ID != conversationrun.AgentBuilderDeploymentID ||
		len(filter.Statuses) != 1 || filter.Statuses[0] != ConversationStatusActive ||
		filter.AgentBuilderDraftOf != "ana" || filter.Limit != defaultAgentBuilderDraftLimit || filter.Offset != 0 {
		t.Fatalf("conversation filter = %#v", filter)
	}
	// Someone else's, the created one and the archived one are never read for status.
	if !reflect.DeepEqual(fake.statusReads, order) {
		t.Fatalf("status reads = %v", fake.statusReads)
	}

	// The store pages; only the page is read for status.
	fake.statusReads = nil
	page, err := ListAgentBuilderDrafts(t.Context(), fake, AgentBuilderDraftFilter{
		Scope: agentBuilderDraftScope, Requester: agentBuilderDraftRequester, Principal: agentBuilderDraftPrincipal, Limit: 2, Offset: 3,
	})
	if err != nil || len(page) != 2 || page[0].ConversationID != "needs-input" || page[1].ConversationID != "answered" {
		t.Fatalf("page = %#v, %v", page, err)
	}
	if last := fake.filters[len(fake.filters)-1]; len(fake.filters) != 2 || last.Limit != 2 || last.Offset != 3 ||
		!reflect.DeepEqual(fake.statusReads, []string{"needs-input", "answered"}) {
		t.Fatalf("page filters = %#v, status reads = %v", fake.filters, fake.statusReads)
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

// The form name set only in the first message still titles a draft whose
// newest page of messages no longer reaches it.
func TestAgentBuilderDraftsTitleFromFirstMessagePastTheNewestPage(t *testing.T) {
	fake := &agentBuilderDraftFake{}
	fake.add("long", time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC), agentBuilderDraftRequester)
	agent := ConversationParticipant{Type: ConversationParticipantAgent, ID: conversationrun.AgentBuilderDeploymentID}
	for sequence := int64(2); sequence <= agentBuilderDraftMessageScan+1; sequence++ {
		fake.messages["long"] = append(fake.messages["long"], &ChannelMessage{ID: "update", Sequence: sequence, Sender: agent, Intent: MessageIntentUpdate})
	}
	drafts, err := ListAgentBuilderDrafts(t.Context(), fake, AgentBuilderDraftFilter{Scope: agentBuilderDraftScope, Requester: agentBuilderDraftRequester})
	if err != nil || len(drafts) != 1 || drafts[0].Title != "Press long" {
		t.Fatalf("drafts = %#v, %v", drafts, err)
	}
}

// A drafts list for one space asks the store for that space's conversations only.
func TestAgentBuilderDraftsFilterBySpace(t *testing.T) {
	fake := &agentBuilderDraftFake{}
	fake.add("work", time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC), agentBuilderDraftRequester)
	if _, err := ListAgentBuilderDrafts(t.Context(), fake, AgentBuilderDraftFilter{
		Scope: agentBuilderDraftScope, Requester: agentBuilderDraftRequester, Principal: agentBuilderDraftPrincipal, SpaceID: " space-work ",
	}); err != nil {
		t.Fatal(err)
	}
	if len(fake.filters) == 0 || fake.filters[0].SpaceID != "space-work" {
		t.Fatalf("filters = %#v", fake.filters)
	}
}
