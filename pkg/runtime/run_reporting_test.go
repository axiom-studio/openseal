package runtime

import (
	"testing"
)

func TestProjectConversationRunbookStartAcknowledgesDurableChild(t *testing.T) {
	store := newTestKernelStore(t)
	ctx := t.Context()
	scope := Scope{Kind: "tenant", ID: "conversation-runbook-start"}
	owner := ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent-42"}
	service := NewConversationService(store)
	seedTestSpace(t, store, scope)
	conversation, _, err := service.CreateConversation(ctx, CreateConversationRequest{
		Scope: scope, Owner: owner, Title: "Agent work", IdempotencyKey: "agent-work-channel",
	})
	if err != nil {
		t.Fatal(err)
	}
	trigger := postConversationRunTestMessage(t, service, conversation, ConversationParticipantUser, MessageIntentQuestion, "Run it now.", "run-now")
	source := &AgentRun{
		ID: "conversation-run", Kind: RunKindConversation, Scope: scope, Owner: owner, AssignedAgentID: owner.ID,
		Context: map[string]interface{}{conversationRunContextConversationID: conversation.ID, conversationRunContextTriggerID: trigger.ID},
	}
	child := &AgentRun{ID: "operation-run", Kind: RunKindAgentWork, Scope: scope, Owner: owner, AssignedAgentID: owner.ID}
	summary := "Run the on-demand engagement operation"
	if err := projectConversationRunbookStart(ctx, store, source, summary, child); err != nil {
		t.Fatal(err)
	}
	if err := projectConversationRunbookStart(ctx, store, source, summary, child); err != nil {
		t.Fatal(err)
	}
	messages, err := service.ListChannelMessages(ctx, ChannelMessageFilter{Scope: scope, ConversationID: conversation.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || messages[1].Intent != MessageIntentUpdate || messages[1].ReplyToMessageID != trigger.ID ||
		messages[1].Content != "Started: Run the on-demand engagement operation" || len(messages[1].References) != 1 ||
		messages[1].References[0] != (ConversationReference{Kind: ConversationReferenceRun, ID: child.ID}) {
		t.Fatalf("conversation Runbook start projection = %#v", messages)
	}
}

// A routine occurrence that finished silent posts nothing in HelloSeal and
// nothing in the external chat the routine was created from. A failed
// occurrence still reports.
func TestRoutineOccurrenceSilentPostsNothing(t *testing.T) {
	f := newRoutineReportFixture(t, "", true)
	run := f.occurrence("quiet-occurrence", true)
	run.Context[RoutineEventBatchContextKey] = "batch-1"
	run.Context[RoutineEventHeaderContextKey] = "New email from Ana Ruiz: Invoice #4411"
	run.Output = map[string]interface{}{finalOutputSilentKey: true}
	if err := f.worker.project(t.Context(), run); err != nil {
		t.Fatal(err)
	}
	messages, err := NewConversationService(f.store).ListChannelMessages(t.Context(), ChannelMessageFilter{Scope: run.Scope, ConversationID: f.conversation.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0].ID != f.source.ID {
		t.Fatalf("silent occurrence posted: %#v", messages)
	}
	if deliveries := f.deliveries(t); len(deliveries) != 0 {
		t.Fatalf("silent occurrence was mirrored: %#v", deliveries)
	}

	failed := f.occurrence("failed-occurrence", true)
	failed.Status = AgentRunStatusCanceled
	failed.Output = map[string]interface{}{finalOutputSilentKey: true}
	if err := f.worker.project(t.Context(), failed); err != nil {
		t.Fatal(err)
	}
	report, err := f.store.FindChannelMessageByIdempotencyKey(t.Context(), failed.Scope, f.conversation.ID, "run-reporting-terminal:"+failed.ID+":"+string(failed.Status))
	if err != nil || report == nil {
		t.Fatalf("a stopped occurrence must still report: %#v, %v", report, err)
	}
}
