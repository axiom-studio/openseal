package runtime

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

type interactionChangeFixture struct {
	ctx          context.Context
	store        KernelStore
	scope        Scope
	conversation *Conversation
	trigger      *ChannelMessage
	run          *AgentRun
	changes      *ConversationChangeService
}

func interactionChangeStores() []struct {
	name  string
	store func(*testing.T) KernelStore
} {
	return []struct {
		name  string
		store func(*testing.T) KernelStore
	}{
		{name: "memory", store: func(*testing.T) KernelStore { return NewMemoryStore() }},
		{name: "sqlite", store: func(t *testing.T) KernelStore {
			store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "interaction-changes.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			return store
		}},
	}
}

func newInteractionChangeFixture(t *testing.T, store KernelStore) *interactionChangeFixture {
	t.Helper()
	f := &interactionChangeFixture{ctx: context.Background(), store: store, scope: Scope{Kind: "tenant", ID: "interactions"}}
	conversationStore := store.(ConversationStore)
	conversations := NewConversationService(conversationStore)
	conversation, _, err := conversations.CreateConversation(f.ctx, CreateConversationRequest{
		Scope: f.scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "assistant"}, Title: "Chat", IdempotencyKey: "chat",
	})
	if err != nil {
		t.Fatal(err)
	}
	posted, err := conversations.PostChannelMessage(f.ctx, PostChannelMessageRequest{
		Scope: f.scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
		Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "operator"}, Intent: MessageIntentQuestion,
		Content: "Deploy the release", Audience: ConversationAudience{Kind: ConversationAudienceChannel},
		RequiresResponse: true, IdempotencyKey: "deploy",
	})
	if err != nil {
		t.Fatal(err)
	}
	scheduler, err := NewConversationRunScheduler(conversationStore, store, ConversationRunSchedulerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	scheduled, _, err := scheduler.ScheduleMessage(f.ctx, f.scope, conversation.ID, posted.Message.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.changes, err = NewConversationChangeService(conversationStore, store)
	if err != nil {
		t.Fatal(err)
	}
	f.conversation, f.trigger, f.run = conversation, posted.Message, scheduled.Run
	return f
}

func (f *interactionChangeFixture) list(t *testing.T, cursor string) *ConversationChangeSet {
	t.Helper()
	changes, err := f.changes.ListChanges(f.ctx, ConversationChangeRequest{Scope: f.scope, ConversationID: f.conversation.ID, Cursor: cursor})
	if err != nil {
		t.Fatal(err)
	}
	return changes
}

func TestConversationChangesProjectApprovalsWithConversationContext(t *testing.T) {
	for _, fixture := range interactionChangeStores() {
		t.Run(fixture.name, func(t *testing.T) {
			f := newInteractionChangeFixture(t, fixture.store(t))
			initial := f.list(t, "")
			if !initial.ApprovalsChanged || initial.Approvals == nil || len(initial.Approvals) != 0 {
				t.Fatalf("initial approvals = %#v", initial)
			}
			run, err := f.store.GetAgentRun(f.ctx, f.scope, f.run.ID)
			if err != nil {
				t.Fatal(err)
			}
			actions := f.store.(ActionStore)
			proposal := sqliteApprovalProposal(run, "deploy-call", "deploy-key", "deploy-event")
			proposal.Run.WakeCondition.Reference = proposal.Approval.ID
			if _, err := actions.CreateActionProposal(f.ctx, proposal); err != nil {
				t.Fatal(err)
			}
			// Creating an approval advances the cursor, so a subscriber emits it.
			requested := f.list(t, initial.Cursor)
			if !requested.HasChanges || !requested.ApprovalsChanged || len(requested.Approvals) != 1 || requested.Cursor == initial.Cursor {
				t.Fatalf("requested approvals = %#v", requested)
			}
			approval := requested.Approvals[0]
			if approval.Status != ApprovalStatusPending || approval.RunID != run.ID || approval.ConversationContext == nil ||
				*approval.ConversationContext != (ApprovalConversationContext{ConversationID: f.conversation.ID, TriggerMessageID: f.trigger.ID}) {
				t.Fatalf("approval context = %#v", approval)
			}
			unchanged := f.list(t, requested.Cursor)
			if unchanged.ApprovalsChanged || len(unchanged.Approvals) != 0 || unchanged.HasChanges {
				t.Fatalf("unchanged approvals = %#v", unchanged)
			}
			coordinator := NewApprovalCoordinator(f.store, actions, ApprovalAuthorizerFunc(func(context.Context, ApprovalPrincipal, *ApprovalCheckpoint) error { return nil }))
			if _, err := coordinator.Resolve(f.ctx, ResolveApprovalRequest{
				Scope: f.scope, ApprovalID: approval.ID, ExpectedRevision: approval.Revision, DecisionID: "decision",
				Decision: ApprovalDecisionApprove, Principal: ApprovalPrincipal{Type: "role", ID: "release-manager"},
			}); err != nil {
				t.Fatal(err)
			}
			// Resolution advances the cursor too; the decided approval stays visible.
			resolved := f.list(t, unchanged.Cursor)
			if !resolved.ApprovalsChanged || len(resolved.Approvals) != 1 || resolved.Approvals[0].Status != ApprovalStatusApproved ||
				resolved.Approvals[0].ConversationContext.ConversationID != f.conversation.ID {
				t.Fatalf("resolved approvals = %#v", resolved)
			}
			// The durable checkpoint never stores the projection.
			stored, err := actions.GetApproval(f.ctx, f.scope, approval.ID)
			if err != nil || stored.ConversationContext != nil {
				t.Fatalf("projection persisted: %#v %v", stored, err)
			}
		})
	}
}

func TestConversationChangesRunScopedApprovalFilter(t *testing.T) {
	for _, fixture := range interactionChangeStores() {
		t.Run(fixture.name, func(t *testing.T) {
			f := newInteractionChangeFixture(t, fixture.store(t))
			run, err := f.store.GetAgentRun(f.ctx, f.scope, f.run.ID)
			if err != nil {
				t.Fatal(err)
			}
			actions := f.store.(ActionStore)
			if _, err := actions.CreateActionProposal(f.ctx, sqliteApprovalProposal(run, "call", "key", "event")); err != nil {
				t.Fatal(err)
			}
			for _, test := range []struct {
				ids  []string
				want int
			}{{nil, 1}, {[]string{}, 0}, {[]string{"other"}, 0}, {[]string{"other", run.ID}, 1}} {
				approvals, err := actions.ListApprovals(f.ctx, ApprovalFilter{Scope: f.scope, RunIDs: test.ids})
				if err != nil || len(approvals) != test.want {
					t.Fatalf("RunIDs %v: %d approvals, %v", test.ids, len(approvals), err)
				}
			}
		})
	}
}

func TestConversationChangesProjectSkillSetupRequests(t *testing.T) {
	for _, fixture := range interactionChangeStores() {
		t.Run(fixture.name, func(t *testing.T) {
			f := newInteractionChangeFixture(t, fixture.store(t))
			initial := f.list(t, "")
			if !initial.SkillSetupRequestsChanged || initial.SkillSetupRequests == nil || len(initial.SkillSetupRequests) != 0 {
				t.Fatalf("initial setup requests = %#v", initial)
			}
			setups := f.store.(SkillSetupRequestStore)
			now := time.Now().UTC()
			request := &SkillSetupRequest{ID: "setup", Scope: f.scope, DeploymentID: "assistant", ConversationID: f.conversation.ID,
				TriggerMessageID: f.trigger.ID, RunID: f.run.ID, ActionCallID: "call", Kind: "configure", SkillID: "reddit.reader",
				SkillVersion: "1.0.0", SkillName: "Reddit", Reason: "Connect Reddit", Status: "pending", Revision: 1, CreatedAt: now, UpdatedAt: now}
			if err := setups.SaveSkillSetupRequest(f.ctx, request, 0); err != nil {
				t.Fatal(err)
			}
			pending := f.list(t, initial.Cursor)
			if !pending.HasChanges || !pending.SkillSetupRequestsChanged || len(pending.SkillSetupRequests) != 1 ||
				pending.SkillSetupRequests[0].ID != "setup" || pending.SkillSetupRequests[0].Status != "pending" {
				t.Fatalf("pending setup = %#v", pending)
			}
			resolved := cloneSkillSetupRequest(request)
			resolved.Status, resolved.ResolvedBy, resolved.ResolvedBindingID, resolved.ResolvedBindingRevision = "resolved", "user", "binding", 1
			resolved.Revision, resolved.UpdatedAt = 2, now.Add(time.Second)
			if err := setups.SaveSkillSetupRequest(f.ctx, resolved, 1); err != nil {
				t.Fatal(err)
			}
			// Completing setup advances the cursor and carries the resolved
			// state; the completed request never comes back as pending.
			completed := f.list(t, pending.Cursor)
			if !completed.SkillSetupRequestsChanged || len(completed.SkillSetupRequests) != 1 ||
				completed.SkillSetupRequests[0].Status != "resolved" || completed.SkillSetupRequests[0].Revision != 2 {
				t.Fatalf("completed setup = %#v", completed)
			}
			for _, cursor := range []string{completed.Cursor, ""} {
				for _, item := range f.list(t, cursor).SkillSetupRequests {
					if item.Status == "pending" {
						t.Fatalf("completed setup reappeared: %#v", item)
					}
				}
			}
			if later := f.list(t, completed.Cursor); later.SkillSetupRequestsChanged || later.HasChanges {
				t.Fatalf("unchanged setup = %#v", later)
			}
		})
	}
}

func TestConversationChangesBoundSettledInteractions(t *testing.T) {
	f := newInteractionChangeFixture(t, NewMemoryStore())
	setups := f.store.(SkillSetupRequestStore)
	start := time.Now().UTC()
	for index := 0; index < conversationInteractionHistoryLimit+5; index++ {
		at := start.Add(time.Duration(index) * time.Second)
		request := &SkillSetupRequest{ID: fmt.Sprintf("setup-%03d", index), Scope: f.scope, DeploymentID: "assistant", ConversationID: f.conversation.ID,
			TriggerMessageID: f.trigger.ID, RunID: f.run.ID, ActionCallID: fmt.Sprintf("call-%d", index), Kind: "configure", SkillID: "reddit.reader",
			SkillVersion: "1.0.0", SkillName: "Reddit", Reason: "Connect Reddit", Status: "dismissed", ResolvedBy: "user", Revision: 1, CreatedAt: at, UpdatedAt: at}
		if index == 0 {
			request.Status, request.ResolvedBy = "pending", ""
		}
		if err := setups.SaveSkillSetupRequest(f.ctx, request, 0); err != nil {
			t.Fatal(err)
		}
	}
	projected := f.list(t, "").SkillSetupRequests
	if len(projected) != conversationInteractionHistoryLimit+1 || projected[0].ID != "setup-000" || projected[0].Status != "pending" ||
		projected[1].ID != "setup-005" || projected[len(projected)-1].ID != fmt.Sprintf("setup-%03d", conversationInteractionHistoryLimit+4) {
		t.Fatalf("bounded projection kept %d items starting %s", len(projected), projected[0].ID)
	}
}

func TestConversationRunLineageValueInheritsThreadPlacement(t *testing.T) {
	root := &AgentRun{ID: "root", RootRunID: "root", Context: map[string]interface{}{"threadRootMessageId": "thread", "triggerMessageId": "trigger"}}
	parent := &AgentRun{ID: "parent", RootRunID: "root", ParentRunID: "root", Context: map[string]interface{}{}}
	child := &AgentRun{ID: "child", RootRunID: "root", ParentRunID: "parent", Context: map[string]interface{}{"triggerMessageId": "own"}}
	runs := map[string]*AgentRun{"root": root, "parent": parent, "child": child}
	if got := conversationRunLineageValue(child, runs, "threadRootMessageId"); got != "thread" {
		t.Fatalf("thread = %q", got)
	}
	if got := conversationRunLineageValue(child, runs, "triggerMessageId"); got != "own" {
		t.Fatalf("trigger = %q", got)
	}
	if got := conversationRunLineageValue(&AgentRun{ID: "orphan", ParentRunID: "missing"}, runs, "threadRootMessageId"); got != "" {
		t.Fatalf("orphan = %q", got)
	}
}
