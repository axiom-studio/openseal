package collaboration

import (
	"context"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/action"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation/conversationtest"
)

// An event routine occurrence, and a Run under one, is manual whatever the
// conversation's mode, and loosening the mode never approves its checkpoints.
func TestRoutineEventOccurrenceApprovalIsManual(t *testing.T) {
	ctx := context.Background()
	store := conversationtest.NewStore(t)
	conversation := conversationtest.ApprovalModeConversation(t, store)
	service := runtimeconversation.NewConversationApprovalModeService(store)
	coordinator := action.NewActionCoordinator(store, store, conversationtest.ApprovalModeCatalog(t), action.NewDefaultActionPolicy(), nil)
	scope := conversation.Scope

	occurrence := &models.AgentRun{ID: "occurrence", Scope: scope, RootRunID: "occurrence", Context: map[string]interface{}{
		"conversationId": conversation.ID, models.ScheduledTaskContextKey: true, models.RoutineEventBatchContextKey: "batch-1",
	}}
	child := buildCollaborationChildRun(occurrence, &models.AgentRequest{ID: "request", Kind: models.AgentRequestKindHandoff}, time.Now(), "child")
	scheduled := &models.AgentRun{ID: "scheduled", Scope: scope, RootRunID: "scheduled", Context: map[string]interface{}{
		"conversationId": conversation.ID, models.ScheduledTaskContextKey: true,
	}}
	runs := conversationtest.RunsByID{occurrence.ID: models.CloneAgentRun(occurrence)}
	for _, test := range []struct {
		run  *models.AgentRun
		want models.ConversationApprovalMode
	}{
		{occurrence, models.ConversationApprovalManual},
		{child, models.ConversationApprovalManual},
		{scheduled, models.ConversationApprovalAuto},
	} {
		id, mode, err := action.RunConversationApprovalMode(ctx, runs, store, test.run)
		if err != nil || id != conversation.ID || mode != test.want {
			t.Fatalf("%s mode = %q %q %v, want %q", test.run.ID, id, mode, err, test.want)
		}
	}

	// Under auto, an event occurrence's write still waits for review.
	result, f, err := conversationtest.ProposeLoopForApprovalModeWithContext(t, store, coordinator, map[string]interface{}{
		"conversationId": conversation.ID, models.ScheduledTaskContextKey: true, models.RoutineEventBatchContextKey: "batch-2",
	}, "note", "event-run", map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Approval == nil {
		t.Fatalf("event occurrence write ran without review: %#v", result.Call)
	}
	f.ParkOnApproval(result.Approval.ID)
	conversation = conversationtest.SetApprovalMode(t, service, conversation, models.ConversationApprovalManual).Conversation
	plain := conversationtest.ProposeForApprovalMode(t, store, coordinator, conversation, "note")

	loosened := conversationtest.SetApprovalMode(t, service, conversation, models.ConversationApprovalSkip)
	if len(loosened.ApprovedApprovalIDs) != 1 || loosened.ApprovedApprovalIDs[0] != plain.ID || loosened.ApprovalSweepIncomplete {
		t.Fatalf("skip sweep = %#v", loosened)
	}
	if pending, _ := store.GetApproval(ctx, scope, result.Approval.ID); pending.Status != models.ApprovalStatusPending {
		t.Fatalf("the sweep approved an event occurrence's checkpoint: %#v", pending)
	}
}

// countingRunReader counts store reads made while resolving a Run's mode.
type countingRunReader struct {
	models.AgentRunReader
	reads int
}

func (r *countingRunReader) GetAgentRun(ctx context.Context, scope models.Scope, runID string) (*models.AgentRun, error) {
	r.reads++
	return r.AgentRunReader.GetAgentRun(ctx, scope, runID)
}

// Children carry their source's routineEventBatchId, never one their caller
// supplied, so resolving a delegated child's mode needs no root read.
func TestRoutineEventBatchInheritedWithoutRootRead(t *testing.T) {
	ctx := context.Background()
	store := conversationtest.NewStore(t)
	conversation := conversationtest.ApprovalModeConversation(t, store)
	scope := conversation.Scope
	chat := &models.AgentRun{ID: "chat", Kind: models.RunKindConversation, Scope: scope, RootRunID: "chat", Context: map[string]interface{}{
		"conversationId": conversation.ID,
	}}
	occurrence := &models.AgentRun{ID: "occurrence", Kind: models.RunKindConversation, Scope: scope, RootRunID: "occurrence", Context: map[string]interface{}{
		"conversationId": conversation.ID, models.ScheduledTaskContextKey: true, models.RoutineEventBatchContextKey: "batch-1",
	}}
	forged := &models.AgentRequest{ID: "request", Kind: models.AgentRequestKindHandoff, SharedContext: map[string]interface{}{models.RoutineEventBatchContextKey: "forged"}}
	for _, test := range []struct {
		source    *models.AgentRun
		wantBatch string
		wantMode  models.ConversationApprovalMode
	}{
		{chat, "", models.ConversationApprovalAuto},
		{occurrence, "batch-1", models.ConversationApprovalManual},
	} {
		child := buildCollaborationChildRun(test.source, forged, time.Now(), test.source.ID+"-child")
		if got := models.RoutineEventBatchID(child); got != test.wantBatch {
			t.Fatalf("%s child batch = %q, want %q", test.source.ID, got, test.wantBatch)
		}
		reader := &countingRunReader{AgentRunReader: store}
		id, mode, err := action.RunConversationApprovalMode(ctx, reader, store, child)
		if err != nil || id != conversation.ID || mode != test.wantMode {
			t.Fatalf("%s child mode = %q %q %v, want %q", test.source.ID, id, mode, err, test.wantMode)
		}
		if reader.reads != 0 {
			t.Fatalf("%s child mode read %d Runs, want none", test.source.ID, reader.reads)
		}
	}
}
