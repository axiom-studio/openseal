package agentbuilder

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/conversationrun"
)

type agentBuilderDraftFake struct {
	conversations []*models.Conversation
	messages      map[string][]*models.ChannelMessage // ascending
	runs          map[string][]*models.AgentRun       // newest first
	approvals     map[string][]*models.ApprovalCheckpoint
	created       map[string]bool // produced its Agent or Team
	filters       []models.ConversationFilter
	statusReads   []string // conversations whose approvals were read
}

// ListConversations selects as a store does: the person's unfinished builder
// conversations in the asked statuses.
func (f *agentBuilderDraftFake) ListConversations(_ context.Context, filter models.ConversationFilter) ([]*models.Conversation, error) {
	f.filters = append(f.filters, filter)
	selected := []*models.Conversation{}
	for _, conversation := range f.conversations {
		if f.messages[conversation.ID][0].Sender.ID == filter.AgentBuilderDraftOf && !f.created[conversation.ID] &&
			slices.Contains(filter.Statuses, conversation.Status) {
			selected = append(selected, conversation)
		}
	}
	if filter.Offset >= len(selected) {
		return []*models.Conversation{}, nil
	}
	selected = selected[filter.Offset:]
	if filter.Limit > 0 && len(selected) > filter.Limit {
		selected = selected[:filter.Limit]
	}
	return selected, nil
}

func (f *agentBuilderDraftFake) ListChannelMessages(_ context.Context, filter models.ChannelMessageFilter) ([]*models.ChannelMessage, error) {
	values := append([]*models.ChannelMessage(nil), f.messages[filter.ConversationID]...)
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

func (f *agentBuilderDraftFake) ListAgentRuns(_ context.Context, filter models.AgentRunFilter) ([]*models.AgentRun, error) {
	if filter.AssignedAgentID != conversationrun.AgentBuilderDeploymentID || filter.Order != models.AgentRunOrderCreatedDesc {
		return nil, errors.New("unexpected run filter")
	}
	return f.runs[filter.ConversationID], nil
}

func (f *agentBuilderDraftFake) ListApprovals(_ context.Context, filter models.ApprovalFilter) ([]*models.ApprovalCheckpoint, error) {
	if !filter.NewestFirst {
		return nil, errors.New("unexpected approval order")
	}
	f.statusReads = append(f.statusReads, filter.ConversationID)
	return f.approvals[filter.ConversationID], nil
}

var (
	agentBuilderDraftScope     = models.Scope{Kind: "tenant", ID: "one"}
	agentBuilderDraftRequester = models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "ana"}
	agentBuilderDraftPrincipal = models.ApprovalPrincipal{Type: "user", ID: "ana"}
)

func (f *agentBuilderDraftFake) add(id string, updated time.Time, sender models.ConversationParticipant) {
	if f.messages == nil {
		f.messages, f.runs, f.approvals, f.created = map[string][]*models.ChannelMessage{}, map[string][]*models.AgentRun{}, map[string][]*models.ApprovalCheckpoint{}, map[string]bool{}
	}
	f.conversations = append(f.conversations, &models.Conversation{
		ID: id, Scope: agentBuilderDraftScope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: conversationrun.AgentBuilderDeploymentID},
		Title: "New agent", Status: models.ConversationStatusActive, UpdatedAt: updated, SpaceID: "space-" + id,
		Origin: &models.ConversationReference{Kind: models.ConversationReferenceAgentBuilder, ID: id},
	})
	f.messages[id] = []*models.ChannelMessage{{
		ID: id + "-m1", ConversationID: id, Sequence: 1, Sender: sender, Content: "Build a press agent",
		BuilderParameters: &models.AgentBuilderParameters{Kind: models.AgentBuilderKindAgent, Name: "Press " + id},
	}}
}

func agentBuilderCard(id, operation string, status models.ApprovalStatus, approvers ...models.ApprovalPrincipal) *models.ApprovalCheckpoint {
	return &models.ApprovalCheckpoint{
		ID: id, ActionCallID: id + "-call", Status: status, EligibleApprovers: approvers,
		ProposedAction: map[string]interface{}{
			"resourceType": models.AgentProfileResourceType, "operation": operation,
			"agent": map[string]interface{}{"displayName": "Card " + id},
		},
	}
}

func TestAgentBuilderDraftsStatusesAndFiltering(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	fake := &agentBuilderDraftFake{}
	agent := models.ConversationParticipant{Type: models.ConversationParticipantAgent, ID: conversationrun.AgentBuilderDeploymentID}
	at := func(i int) time.Time { return now.Add(-time.Duration(i) * time.Minute) }

	fake.add("drafting", at(0), agentBuilderDraftRequester)
	fake.runs["drafting"] = []*models.AgentRun{
		{ID: "child", ParentRunID: "run-drafting", Status: models.AgentRunStatusFailed},
		{ID: "run-drafting", Status: models.AgentRunStatusRunning},
	}
	fake.approvals["drafting"] = []*models.ApprovalCheckpoint{agentBuilderCard("drafting-card", models.AgentActionCreateAgent, models.ApprovalStatusPending, agentBuilderDraftPrincipal)}

	fake.add("ready", at(1), agentBuilderDraftRequester)
	fake.runs["ready"] = []*models.AgentRun{{ID: "run-ready", Status: models.AgentRunStatusWaitingForApproval}}
	fake.approvals["ready"] = []*models.ApprovalCheckpoint{
		{ID: "unrelated", Status: models.ApprovalStatusPending, ProposedAction: map[string]interface{}{"resourceType": "skill_binding"}},
		agentBuilderCard("ready-card", models.AgentActionCreateAgent, models.ApprovalStatusPending, agentBuilderDraftPrincipal),
	}

	fake.add("needs-approval", at(2), agentBuilderDraftRequester)
	fake.runs["needs-approval"] = []*models.AgentRun{{ID: "run-na", Status: models.AgentRunStatusWaitingForApproval}}
	fake.approvals["needs-approval"] = []*models.ApprovalCheckpoint{agentBuilderCard("na-card", models.AgentActionUpdateAgent, models.ApprovalStatusPending, models.ApprovalPrincipal{Type: "user", ID: "boss"})}

	fake.add("needs-input", at(3), agentBuilderDraftRequester)
	fake.runs["needs-input"] = []*models.AgentRun{{ID: "run-ni", Status: models.AgentRunStatusWaitingForEvent}}
	fake.messages["needs-input"] = append(fake.messages["needs-input"],
		&models.ChannelMessage{ID: "q1", Sequence: 2, Sender: agent, Intent: models.MessageIntentQuestion, Content: "Which tone?"},
		&models.ChannelMessage{ID: "status", Sequence: 3, Sender: agent, Intent: models.MessageIntentUpdate, Content: "Waiting"})

	fake.add("answered", at(4), agentBuilderDraftRequester)
	fake.messages["answered"] = append(fake.messages["answered"],
		&models.ChannelMessage{ID: "q2", Sequence: 2, Sender: agent, Intent: models.MessageIntentQuestion, Content: "Which tone?"},
		&models.ChannelMessage{ID: "a2", Sequence: 3, Sender: agentBuilderDraftRequester, Intent: models.MessageIntentAnswer, Content: "Warm",
			BuilderParameters: &models.AgentBuilderParameters{Kind: models.AgentBuilderKindAgent, Name: "Renamed"}})

	fake.add("stopped", at(5), agentBuilderDraftRequester)
	fake.runs["stopped"] = []*models.AgentRun{{ID: "run-stopped", Status: models.AgentRunStatusCanceled}}

	fake.add("failed", at(6), agentBuilderDraftRequester)
	fake.runs["failed"] = []*models.AgentRun{{ID: "run-failed", Status: models.AgentRunStatusFailed}}
	fake.approvals["failed"] = []*models.ApprovalCheckpoint{agentBuilderCard("failed-card", models.AgentActionCreateAgent, models.ApprovalStatusApproved, agentBuilderDraftPrincipal)}

	fake.add("created", at(7), agentBuilderDraftRequester)
	fake.runs["created"] = []*models.AgentRun{{ID: "run-created", Status: models.AgentRunStatusCompleted}}
	fake.approvals["created"] = []*models.ApprovalCheckpoint{
		agentBuilderCard("later-update", models.AgentActionUpdateAgent, models.ApprovalStatusPending, agentBuilderDraftPrincipal),
		agentBuilderCard("created-card", models.AgentActionCreateAgent, models.ApprovalStatusApproved, agentBuilderDraftPrincipal),
	}
	fake.created["created"] = true

	fake.add("someone-else", at(8), models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "bo"})
	fake.add("archived", at(9), agentBuilderDraftRequester)
	fake.conversations[len(fake.conversations)-1].Status = models.ConversationStatusArchived

	drafts, err := ListAgentBuilderDrafts(t.Context(), fake, DraftFilter{
		Scope: agentBuilderDraftScope, Requester: agentBuilderDraftRequester, Principal: agentBuilderDraftPrincipal,
	})
	if err != nil {
		t.Fatal(err)
	}
	type want struct {
		status            DraftStatus
		title, run, card  string
		questionMessageID string
	}
	wants := map[string]want{
		"drafting":       {DraftDrafting, "Card drafting-card", "run-drafting", "drafting-card", ""},
		"ready":          {DraftReady, "Card ready-card", "run-ready", "ready-card", ""},
		"needs-approval": {DraftNeedsApproval, "Card na-card", "run-na", "na-card", ""},
		"needs-input":    {DraftNeedsInput, "Press needs-input", "run-ni", "", "q1"},
		"answered":       {DraftIdle, "Renamed", "", "", ""},
		"stopped":        {DraftStopped, "Press stopped", "run-stopped", "", ""},
		"failed":         {DraftFailed, "Card failed-card", "run-failed", "", ""},
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
	if filter.OriginKind != models.ConversationReferenceAgentBuilder || filter.Owner == nil || filter.Owner.ID != conversationrun.AgentBuilderDeploymentID ||
		len(filter.Statuses) != 1 || filter.Statuses[0] != models.ConversationStatusActive ||
		filter.AgentBuilderDraftOf != "ana" || filter.Limit != defaultAgentBuilderDraftLimit || filter.Offset != 0 {
		t.Fatalf("conversation filter = %#v", filter)
	}
	// Someone else's, the created one and the archived one are never read for status.
	if !reflect.DeepEqual(fake.statusReads, order) {
		t.Fatalf("status reads = %v", fake.statusReads)
	}

	// The store pages; only the page is read for status.
	fake.statusReads = nil
	page, err := ListAgentBuilderDrafts(t.Context(), fake, DraftFilter{
		Scope: agentBuilderDraftScope, Requester: agentBuilderDraftRequester, Principal: agentBuilderDraftPrincipal, Limit: 2, Offset: 3,
	})
	if err != nil || len(page) != 2 || page[0].ConversationID != "needs-input" || page[1].ConversationID != "answered" {
		t.Fatalf("page = %#v, %v", page, err)
	}
	if last := fake.filters[len(fake.filters)-1]; len(fake.filters) != 2 || last.Limit != 2 || last.Offset != 3 ||
		!reflect.DeepEqual(fake.statusReads, []string{"needs-input", "answered"}) {
		t.Fatalf("page filters = %#v, status reads = %v", fake.filters, fake.statusReads)
	}
	for _, bad := range []DraftFilter{
		{Scope: agentBuilderDraftScope, Requester: agentBuilderDraftRequester, Limit: 101},
		{Scope: agentBuilderDraftScope, Requester: agentBuilderDraftRequester, Offset: -1},
		{Scope: agentBuilderDraftScope, Requester: models.ConversationParticipant{Type: models.ConversationParticipantAgent, ID: "x"}},
	} {
		if _, err := ListAgentBuilderDrafts(t.Context(), fake, bad); !errors.Is(err, models.ErrInvalidConversation) {
			t.Fatalf("filter %#v accepted: %v", bad, err)
		}
	}
}

// The form name set only in the first message still titles a draft whose
// newest page of messages no longer reaches it.
func TestAgentBuilderDraftsTitleFromFirstMessagePastTheNewestPage(t *testing.T) {
	fake := &agentBuilderDraftFake{}
	fake.add("long", time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC), agentBuilderDraftRequester)
	agent := models.ConversationParticipant{Type: models.ConversationParticipantAgent, ID: conversationrun.AgentBuilderDeploymentID}
	for sequence := int64(2); sequence <= agentBuilderDraftMessageScan+1; sequence++ {
		fake.messages["long"] = append(fake.messages["long"], &models.ChannelMessage{ID: "update", Sequence: sequence, Sender: agent, Intent: models.MessageIntentUpdate})
	}
	drafts, err := ListAgentBuilderDrafts(t.Context(), fake, DraftFilter{Scope: agentBuilderDraftScope, Requester: agentBuilderDraftRequester})
	if err != nil || len(drafts) != 1 || drafts[0].Title != "Press long" {
		t.Fatalf("drafts = %#v, %v", drafts, err)
	}
}

// A drafts list for one space asks the store for that space's conversations only.
func TestAgentBuilderDraftsFilterBySpace(t *testing.T) {
	fake := &agentBuilderDraftFake{}
	fake.add("work", time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC), agentBuilderDraftRequester)
	if _, err := ListAgentBuilderDrafts(t.Context(), fake, DraftFilter{
		Scope: agentBuilderDraftScope, Requester: agentBuilderDraftRequester, Principal: agentBuilderDraftPrincipal, SpaceID: " space-work ",
	}); err != nil {
		t.Fatal(err)
	}
	if len(fake.filters) == 0 || fake.filters[0].SpaceID != "space-work" {
		t.Fatalf("filters = %#v", fake.filters)
	}
}
