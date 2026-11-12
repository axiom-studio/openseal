package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/action"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
)

// An event routine occurrence, and a Run under one, is manual whatever the
// conversation's mode, and loosening the mode never approves its checkpoints.
func TestRoutineEventOccurrenceApprovalIsManual(t *testing.T) {
	ctx := context.Background()
	store := newTestKernelStore(t)
	conversation := ApprovalModeConversation(t, store)
	service := runtimeconversation.NewConversationApprovalModeService(store)
	coordinator := action.NewActionCoordinator(store, store, ApprovalModeCatalog(t), action.NewDefaultActionPolicy(), nil)
	scope := conversation.Scope

	occurrence := &AgentRun{ID: "occurrence", Scope: scope, RootRunID: "occurrence", Context: map[string]interface{}{
		"conversationId": conversation.ID, scheduledTaskContextKey: true, RoutineEventBatchContextKey: "batch-1",
	}}
	child := buildCollaborationChildRun(occurrence, &AgentRequest{ID: "request", Kind: AgentRequestKindHandoff}, time.Now(), "child")
	scheduled := &AgentRun{ID: "scheduled", Scope: scope, RootRunID: "scheduled", Context: map[string]interface{}{
		"conversationId": conversation.ID, scheduledTaskContextKey: true,
	}}
	runs := RunsByID{occurrence.ID: cloneAgentRun(occurrence)}
	for _, test := range []struct {
		run  *AgentRun
		want ConversationApprovalMode
	}{
		{occurrence, ConversationApprovalManual},
		{child, ConversationApprovalManual},
		{scheduled, ConversationApprovalAuto},
	} {
		id, mode, err := action.RunConversationApprovalMode(ctx, runs, store, test.run)
		if err != nil || id != conversation.ID || mode != test.want {
			t.Fatalf("%s mode = %q %q %v, want %q", test.run.ID, id, mode, err, test.want)
		}
	}

	// Under auto, an event occurrence's write still waits for review.
	result, f, err := ProposeLoopForApprovalModeWithContext(t, store, coordinator, map[string]interface{}{
		"conversationId": conversation.ID, scheduledTaskContextKey: true, RoutineEventBatchContextKey: "batch-2",
	}, "note", "event-run", map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Approval == nil {
		t.Fatalf("event occurrence write ran without review: %#v", result.Call)
	}
	f.ParkOnApproval(result.Approval.ID)
	conversation = SetApprovalMode(t, service, conversation, ConversationApprovalManual).Conversation
	plain := ProposeForApprovalMode(t, store, coordinator, conversation, "note")

	loosened := SetApprovalMode(t, service, conversation, ConversationApprovalSkip)
	if len(loosened.ApprovedApprovalIDs) != 1 || loosened.ApprovedApprovalIDs[0] != plain.ID || loosened.ApprovalSweepIncomplete {
		t.Fatalf("skip sweep = %#v", loosened)
	}
	if pending, _ := store.GetApproval(ctx, scope, result.Approval.ID); pending.Status != ApprovalStatusPending {
		t.Fatalf("the sweep approved an event occurrence's checkpoint: %#v", pending)
	}
}

// countingRunReader counts store reads made while resolving a Run's mode.
type countingRunReader struct {
	models.AgentRunReader
	reads int
}

func (r *countingRunReader) GetAgentRun(ctx context.Context, scope Scope, runID string) (*AgentRun, error) {
	r.reads++
	return r.AgentRunReader.GetAgentRun(ctx, scope, runID)
}

// Children carry their source's routineEventBatchId, never one their caller
// supplied, so resolving a delegated child's mode needs no root read.
func TestRoutineEventBatchInheritedWithoutRootRead(t *testing.T) {
	ctx := context.Background()
	store := newTestKernelStore(t)
	conversation := ApprovalModeConversation(t, store)
	scope := conversation.Scope
	chat := &AgentRun{ID: "chat", Kind: RunKindConversation, Scope: scope, RootRunID: "chat", Context: map[string]interface{}{
		"conversationId": conversation.ID,
	}}
	occurrence := &AgentRun{ID: "occurrence", Kind: RunKindConversation, Scope: scope, RootRunID: "occurrence", Context: map[string]interface{}{
		"conversationId": conversation.ID, scheduledTaskContextKey: true, RoutineEventBatchContextKey: "batch-1",
	}}
	forged := &AgentRequest{ID: "request", Kind: AgentRequestKindHandoff, SharedContext: map[string]interface{}{RoutineEventBatchContextKey: "forged"}}
	for _, test := range []struct {
		source    *AgentRun
		wantBatch string
		wantMode  ConversationApprovalMode
	}{
		{chat, "", ConversationApprovalAuto},
		{occurrence, "batch-1", ConversationApprovalManual},
	} {
		child := buildCollaborationChildRun(test.source, forged, time.Now(), test.source.ID+"-child")
		if got := routineEventBatchOf(child); got != test.wantBatch {
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
