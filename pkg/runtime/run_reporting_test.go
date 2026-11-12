package runtime

import (
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation/conversationtest"
	"github.com/axiom-studio/openseal/pkg/runtime/conversationrun"
	"github.com/axiom-studio/openseal/pkg/runtime/conversationrun/conversationruntest"
)

func TestProjectConversationRunbookStartAcknowledgesDurableChild(t *testing.T) {
	store := conversationtest.NewStore(t)
	ctx := t.Context()
	scope := models.Scope{Kind: "tenant", ID: "conversation-runbook-start"}
	owner := models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent-42"}
	service := conversation.NewConversationService(store)
	conversationtest.SeedSpace(t, store, scope)
	conversation, _, err := service.CreateConversation(ctx, conversation.CreateConversationRequest{
		Scope: scope, Owner: owner, Title: "Agent work", IdempotencyKey: "agent-work-channel",
	})
	if err != nil {
		t.Fatal(err)
	}
	trigger := conversationruntest.PostMessage(t, service, conversation, models.ConversationParticipantUser, models.MessageIntentQuestion, "Run it now.", "run-now")
	source := &models.AgentRun{
		ID: "conversation-run", Kind: models.RunKindConversation, Scope: scope, Owner: owner, AssignedAgentID: owner.ID,
		Context: map[string]interface{}{models.ConversationRunConversationIDContextKey: conversation.ID, models.ConversationRunTriggerIDContextKey: trigger.ID},
	}
	child := &models.AgentRun{ID: "operation-run", Kind: models.RunKindAgentWork, Scope: scope, Owner: owner, AssignedAgentID: owner.ID}
	summary := "Run the on-demand engagement operation"
	if err := conversationrun.ProjectConversationRunbookStart(ctx, store, source, summary, child); err != nil {
		t.Fatal(err)
	}
	if err := conversationrun.ProjectConversationRunbookStart(ctx, store, source, summary, child); err != nil {
		t.Fatal(err)
	}
	messages, err := service.ListChannelMessages(ctx, models.ChannelMessageFilter{Scope: scope, ConversationID: conversation.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || messages[1].Intent != models.MessageIntentUpdate || messages[1].ReplyToMessageID != trigger.ID ||
		messages[1].Content != "Started: Run the on-demand engagement operation" || len(messages[1].References) != 1 ||
		messages[1].References[0] != (models.ConversationReference{Kind: models.ConversationReferenceRun, ID: child.ID}) {
		t.Fatalf("conversation Runbook start projection = %#v", messages)
	}
}

// A routine occurrence that finished silent posts nothing in HelloSeal and
// nothing in the external chat the routine was created from. A failed
// occurrence still reports.
func TestRoutineOccurrenceSilentPostsNothing(t *testing.T) {
	f := newRoutineReportFixture(t, "", true)
	run := f.occurrence("quiet-occurrence", true)
	run.Context[models.RoutineEventBatchContextKey] = "batch-1"
	run.Context[models.RoutineEventHeaderContextKey] = "New email from Ana Ruiz: Invoice #4411"
	run.Output = map[string]interface{}{agentrun.FinalOutputSilentKey: true}
	if err := f.worker.project(t.Context(), run); err != nil {
		t.Fatal(err)
	}
	messages, err := conversation.NewConversationService(f.store).ListChannelMessages(t.Context(), models.ChannelMessageFilter{Scope: run.Scope, ConversationID: f.conversation.ID})
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
	failed.Status = models.AgentRunStatusCanceled
	failed.Output = map[string]interface{}{agentrun.FinalOutputSilentKey: true}
	if err := f.worker.project(t.Context(), failed); err != nil {
		t.Fatal(err)
	}
	report, err := f.store.FindChannelMessageByIdempotencyKey(t.Context(), failed.Scope, f.conversation.ID, "run-reporting-terminal:"+failed.ID+":"+string(failed.Status))
	if err != nil || report == nil {
		t.Fatalf("a stopped occurrence must still report: %#v, %v", report, err)
	}
}
