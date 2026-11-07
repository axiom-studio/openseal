package runtime

import (
	"context"
	"strings"
	"testing"
)

// receiptFixture is a chat with one trigger, a Run of its Agent and one
// completed governed tool entry whose call saved a file.
type receiptFixture struct {
	ctx          context.Context
	store        *MemoryStore
	service      *ConversationService
	runner       *ConversationRunTurnRunner
	conversation *Conversation
	trigger      *ChannelMessage
	run          *AgentRun
	call         *ActionCall
	entry        *SessionEntry
	artifact     *Artifact
	actions      *fakeActionCalls
}

func newReceiptFixture(t *testing.T) *receiptFixture {
	t.Helper()
	ctx := context.Background()
	store := NewMemoryStore()
	service := NewConversationService(store)
	scope := Scope{Kind: "tenant", ID: "one"}
	owner := ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}
	conversation, _, err := service.CreateConversation(ctx, CreateConversationRequest{Scope: scope, Owner: owner, Title: "Files", IdempotencyKey: "files"})
	if err != nil {
		t.Fatal(err)
	}
	posted, err := service.PostChannelMessage(ctx, PostChannelMessageRequest{
		Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
		Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "user"}, Intent: MessageIntentQuestion,
		Content: "PDF please", Audience: ConversationAudience{Kind: ConversationAudienceChannel}, IdempotencyKey: "trigger",
	})
	if err != nil {
		t.Fatal(err)
	}
	artifact := &Artifact{ID: "file", Version: 1, Scope: scope, Name: "file.pdf",
		Provenance: ArtifactProvenance{Producer: ActivityActor{Type: "agent", ID: "agent"}, Owner: &owner, RunID: "run", ActionID: "action"},
		ContentRef: "object-store:file", Digest: "sha256:" + strings.Repeat("a", 64), SizeBytes: 10, Classification: ArtifactClassificationInternal}
	call := &ActionCall{ID: "action", Scope: scope, RunID: "run", Status: ActionCallStatusSucceeded,
		Output: map[string]interface{}{"artifactRefs": []interface{}{map[string]interface{}{"id": "file", "version": 1}}}}
	return &receiptFixture{
		ctx: ctx, store: store, service: service, runner: &ConversationRunTurnRunner{conversations: service, artifacts: store},
		conversation: conversation, trigger: posted.Message, artifact: artifact, call: call,
		run:   &AgentRun{ID: "run", Scope: scope, Owner: owner},
		entry: &SessionEntry{ID: "entry", RunID: "run", Type: SessionEntryTool, State: ToolEntryCompleted, ActionCallID: "action"},
	}
}

func (f *receiptFixture) post(t *testing.T) []*ChannelMessage {
	t.Helper()
	if err := f.store.CreateArtifactVersion(f.ctx, f.artifact, 0); err != nil {
		t.Fatal(err)
	}
	f.actions = &fakeActionCalls{calls: map[string]*ActionCall{f.call.ID: f.call}}
	for range 2 {
		if err := f.runner.postToolArtifactReceipt(f.ctx, f.actions, f.run, f.conversation, f.trigger, f.entry); err != nil {
			t.Fatal(err)
		}
	}
	messages, err := f.service.ListChannelMessages(f.ctx, ChannelMessageFilter{Scope: f.run.Scope, ConversationID: f.conversation.ID})
	if err != nil {
		t.Fatal(err)
	}
	return messages
}

func TestToolArtifactReceiptPostsOnceWithProvenance(t *testing.T) {
	f := newReceiptFixture(t)
	messages := f.post(t)
	if len(messages) != 2 {
		t.Fatalf("messages = %#v", messages)
	}
	receipt := messages[1]
	if receipt.Sender != (ConversationParticipant{Type: ConversationParticipantService, ID: conversationArtifactReceiptService}) ||
		receipt.Intent != MessageIntentUpdate || receipt.ResolvesMessageID != "" || receipt.RequiresResponse ||
		conversationMessageStartsRun(f.conversation, receipt) || receipt.ReplyToMessageID != f.trigger.ID {
		t.Fatalf("receipt must not finish or restart work: %#v", receipt)
	}
	want := []ConversationReference{{Kind: ConversationReferenceRun, ID: "run"}, {Kind: ConversationReferenceArtifact, ID: "file", Version: 1}}
	if len(receipt.References) != 2 || receipt.References[0] != want[0] || receipt.References[1] != want[1] {
		t.Fatalf("receipt references = %#v", receipt.References)
	}
}

func TestToolArtifactReceiptRequiresExactProvenance(t *testing.T) {
	for _, mismatch := range []string{"tenant", "owner", "run", "action", "version", "failed", "other-run-call", "entry-not-completed", "no-action"} {
		t.Run(mismatch, func(t *testing.T) {
			f := newReceiptFixture(t)
			switch mismatch {
			case "tenant":
				f.artifact.Scope.ID = "other"
			case "owner":
				f.artifact.Provenance.Owner = &ObjectiveOwner{Type: OwnerTypeAgent, ID: "other"}
			case "run":
				f.artifact.Provenance.RunID = "other"
			case "action":
				f.artifact.Provenance.ActionID = "other"
			case "version":
				f.call.Output = map[string]interface{}{"artifactRefs": []interface{}{map[string]interface{}{"id": "file", "version": 2}}}
			case "failed":
				f.call.Status = ActionCallStatusFailed
			case "other-run-call":
				f.call.RunID = "other"
			case "entry-not-completed":
				f.entry.State = ToolEntryError
			case "no-action":
				f.entry.ActionCallID = ""
			}
			if messages := f.post(t); len(messages) != 1 {
				t.Fatalf("unproven artifact published: %#v", messages)
			}
		})
	}
}

// runActionArtifactReferences reads the Run's tool entries from the session
// log; this lister returns fixed entries.
type fixedEntryLister []*SessionEntry

func (l fixedEntryLister) ListAgentSessionEntries(_ context.Context, filter SessionEntryFilter) ([]*SessionEntry, error) {
	var out []*SessionEntry
	for _, entry := range l {
		if entry.RunID != filter.RunID {
			continue
		}
		if len(filter.States) > 0 && entry.State != filter.States[0] {
			continue
		}
		out = append(out, entry)
	}
	return out, nil
}

func TestRunActionArtifactReferencesFromSucceededCalls(t *testing.T) {
	scope := Scope{Kind: "tenant", ID: "one"}
	run := &AgentRun{ID: "run", Scope: scope}
	refs := func(values ...interface{}) map[string]interface{} {
		list := make([]interface{}, 0, len(values)/2)
		for i := 0; i+1 < len(values); i += 2 {
			list = append(list, map[string]interface{}{"id": values[i], "version": values[i+1]})
		}
		return map[string]interface{}{"artifactRefs": list}
	}
	actions := &fakeActionCalls{calls: map[string]*ActionCall{
		"a1": {ID: "a1", RunID: "run", Status: ActionCallStatusSucceeded, Output: refs("one", 1, "two", 3)},
		"a2": {ID: "a2", RunID: "run", Status: ActionCallStatusSucceeded, Output: refs("one", 1, strings.Repeat("x", 257), 1, "zero", 0)},
		"a3": {ID: "a3", RunID: "run", Status: ActionCallStatusFailed, Output: refs("failed", 1)},
	}}
	entries := fixedEntryLister{
		{ID: "e1", RunID: "run", Type: SessionEntryTool, State: ToolEntryCompleted, ActionCallID: "a1"},
		{ID: "e2", RunID: "run", Type: SessionEntryTool, State: ToolEntryCompleted, ActionCallID: "a2"},
		{ID: "e3", RunID: "run", Type: SessionEntryTool, State: ToolEntryCompleted, ActionCallID: "a3"},
		{ID: "e4", RunID: "run", Type: SessionEntryTool, State: ToolEntryCompleted, ActionCallID: "missing"},
		{ID: "e5", RunID: "run", Type: SessionEntryTool, State: ToolEntryCompleted},
	}
	got, err := runActionArtifactReferences(context.Background(), entries, actions, run)
	if err != nil {
		t.Fatal(err)
	}
	want := []ConversationReference{
		{Kind: ConversationReferenceArtifact, ID: "one", Version: 1},
		{Kind: ConversationReferenceArtifact, ID: "two", Version: 3},
	}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("references = %#v", got)
	}
}
