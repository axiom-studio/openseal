package runtime

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
)

func promotedContinuationAuthorityFixture(t *testing.T, id string) (*SQLiteStore, *ConversationTaskResult, *Conversation, *ChannelMessage) {
	t.Helper()
	store, db := newTestSQLiteStore(t)
	run, conversation, message := continuationLifecycleFixture(t, store, id)
	now := time.Now().UTC()
	run = backdateContinuationLifecycleRun(t, store, db, run, now.Add(-ConversationTaskForegroundTimeout-time.Second))
	result, err := store.PromoteConversationTask(t.Context(), ConversationTaskPromotionRequest{Scope: run.Scope, RunID: run.ID, Now: now})
	if err != nil || result == nil {
		t.Fatalf("promote continuation: %#v %v", result, err)
	}
	return store, result, conversation, message
}

func finishContinuationAuthorityRun(t *testing.T, store *SQLiteStore, run *AgentRun, status AgentRunStatus) *AgentRun {
	t.Helper()
	current, err := store.GetAgentRun(t.Context(), run.Scope, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	current.Status, current.LeaseOwner, current.LeaseExpiresAt = status, "", nil
	current.CompletedAt, current.UpdatedAt = &now, now
	current.Revision++
	storetest.UpdateRun(t, store, current, current.Revision-1)
	return current
}

func TestConversationTaskContinuationUsesTaskReserveAndLeavesForegroundSlot(t *testing.T) {
	store, db := newTestSQLiteStore(t)
	base, _, _ := continuationLifecycleFixture(t, store, "continuation-reserve")
	now := time.Now().UTC()
	base = backdateContinuationLifecycleRun(t, store, db, base, now.Add(-ConversationTaskForegroundTimeout-time.Second))
	for i := range 4 {
		run := cloneAgentRun(base)
		if i > 0 {
			run.ID, run.RootRunID = fmt.Sprintf("continued-root-%d", i), fmt.Sprintf("continued-root-%d", i)
			if err := store.CreateAgentRun(t.Context(), run); err != nil {
				t.Fatal(err)
			}
		}
		result, err := store.PromoteConversationTask(t.Context(), ConversationTaskPromotionRequest{Scope: run.Scope, RunID: run.ID, Now: now})
		if err != nil || result == nil {
			t.Fatalf("promotion %d: %#v %v", i, result, err)
		}
	}
	claim := AgentRunClaim{Scope: base.Scope, Kind: RunKindConversation, WorkerID: "task-reserve", Now: now.Add(time.Second),
		LeaseDuration: time.Minute, AgingInterval: time.Minute, AssignedAgentID: base.AssignedAgentID, MaxActiveForAgent: 4, ConversationTaskForegroundReserve: 1}
	for range 3 {
		decision, err := store.ClaimNextAgentRunWithDecision(t.Context(), claim)
		if err != nil || decision.Run == nil || decision.Run.Kind != RunKindConversation {
			t.Fatalf("claim continued root: %#v %v", decision, err)
		}
	}
	decision, err := store.ClaimNextAgentRunWithDecision(t.Context(), claim)
	if err != nil || decision.Outcome != AgentRunAdmissionBackpressured || decision.Run != nil {
		t.Fatalf("fourth continuation bypassed reserve: %#v %v", decision, err)
	}
	blocked := false
	for _, block := range decision.Blocks {
		if block.Reason == AgentRunAdmissionReasonTaskCapacity && block.Limit == 3 {
			blocked = true
		}
	}
	if !blocked {
		t.Fatalf("missing task capacity evidence: %#v", decision.Blocks)
	}
	fresh := cloneAgentRun(base)
	fresh.ID, fresh.RootRunID = "new-foreground", "new-foreground"
	fresh.CreatedAt, fresh.QueueEnteredAt, fresh.AvailableAt = now, now, now
	if err := store.CreateAgentRun(t.Context(), fresh); err != nil {
		t.Fatal(err)
	}
	decision, err = store.ClaimNextAgentRunWithDecision(t.Context(), claim)
	if err != nil || decision.Run == nil || decision.Run.ID != fresh.ID {
		t.Fatalf("reserved foreground slot unavailable: %#v %v", decision, err)
	}
}

func TestConversationTaskContinuationAdmitsOriginalDelegatedAuthorityOnly(t *testing.T) {
	store, result, _, _ := promotedContinuationAuthorityFixture(t, "continuation-delegates")
	parent := result.WorkRun
	child := cloneAgentRun(parent)
	child.ID, child.ParentRunID, child.Source = "original-delegate", parent.ID, RunSourceHandoff
	child.Owner, child.AssignedAgentID, child.Goal = ObjectiveOwner{Type: OwnerTypeAgent, ID: "specialist"}, "specialist", "Review the specialist portion"
	// A prepromotion child keeps its original kind and lane. Neither field
	// needs to be rewritten to classify its canonical delegated ancestry.
	child.Kind, child.ConcurrencyKey = RunKindConversation, result.Task.ConversationID
	delete(child.Context, ConversationTaskContextKey)
	child.Context["collaboration"] = map[string]interface{}{"requestId": "original-request"}
	request := &AgentRequest{ID: "original-request", Scope: child.Scope, Kind: AgentRequestKindHandoff, Status: AgentRequestStatusAccepted,
		Requester: CollaborationParty{Type: parent.Owner.Type, ID: parent.Owner.ID}, Recipient: CollaborationParty{Type: child.Owner.Type, ID: child.Owner.ID},
		SourceRunID: parent.ID, ChildRunID: child.ID, AssignedAgentID: child.AssignedAgentID, Goal: child.Goal,
		AcceptancePolicy: AgentRequestAcceptancePreauthorized, Revision: 2, CreatedAt: parent.CreatedAt, UpdatedAt: parent.UpdatedAt}
	if err := request.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateAgentRun(t.Context(), child); err != nil {
		t.Fatal(err)
	}
	// The handoff moves the parent to its next revision, as a delegation
	// does.
	source := getStoredRun(t, store, parent.Scope, parent.ID)
	source.Revision++
	if _, err := store.CreateAgentRequest(t.Context(), AgentRequestCreateRecord{Request: request, SourceRun: source, ExpectedSourceRevision: source.Revision - 1,
		Event: &ActivityEvent{ID: "original-request-created", Scope: parent.Scope, RunID: parent.ID, EventType: "agent_request.created", Summary: "Hand off the specialist portion",
			Actor: ActivityActor{Type: "agent", ID: parent.Owner.ID}, Visibility: ActivityVisibilityScope, CreatedAt: parent.UpdatedAt}}); err != nil {
		t.Fatal(err)
	}
	getRun := func(id string) (*AgentRun, error) { return store.GetAgentRun(t.Context(), parent.Scope, id) }
	getRequest := func(ctx context.Context, scope Scope, id string) (*AgentRequest, error) {
		return store.GetAgentRequest(ctx, scope, id)
	}
	valid, err := conversationTaskAdmissionLineage(t.Context(), parent.Scope, result.Task, child, getRun, getRequest)
	if err != nil || !valid {
		t.Fatalf("original specialist delegation lost task scheduling identity: %v %v", valid, err)
	}
	claim := AgentRunClaim{Scope: child.Scope, Kind: RunKindConversation, WorkerID: "specialist", AssignedAgentID: child.AssignedAgentID,
		Now: time.Now().Add(time.Second), LeaseDuration: time.Minute, AgingInterval: time.Minute, MaxActiveForAgent: 4, ConversationTaskForegroundReserve: 1}
	// The store prepares admission with the same reads inside its claim
	// transaction; nothing writes between them here.
	err = prepareConversationTaskAdmission(t.Context(), &claim, []*AgentRun{child}, conversationTaskAdmissionLookup{
		Task: store.FindConversationTaskByWorkRunID, Run: store.GetAgentRun, Conversation: store.GetConversation,
		Message: store.GetChannelMessage, Request: store.GetAgentRequest,
		Round: func(context.Context, Scope, string, string) (*ParticipationRound, error) {
			t.Fatal("a chat continuation needs no participation round proof")
			return nil, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	// With every agent slot reserved for the foreground, task work is held
	// back by task capacity: the delegate counts as task work.
	claim.ConversationTaskForegroundReserve = claim.MaxActiveForAgent
	if _, decision := evaluateAgentRunAdmission([]*AgentRun{child}, nil, claim); len(decision.Blocks) != 1 ||
		decision.Blocks[0].RunID != child.ID || decision.Blocks[0].Reason != AgentRunAdmissionReasonTaskCapacity {
		t.Fatalf("original delegate bypassed background reserve: %#v", decision)
	}
	for _, mutate := range []func(*AgentRun){
		func(run *AgentRun) { run.AssignedAgentID = "foreign-agent" },
		func(run *AgentRun) { run.Scope.ID = "foreign-scope" },
		func(run *AgentRun) { run.ParentRunID = "unrelated-run" },
	} {
		forged := cloneAgentRun(child)
		mutate(forged)
		valid, err := conversationTaskAdmissionLineage(t.Context(), parent.Scope, result.Task, forged, getRun, getRequest)
		if err != nil || valid {
			t.Fatalf("forged delegated ancestry established task admission: %v %v", valid, err)
		}
	}
	legacy := cloneConversationTask(result.Task)
	legacy.Mode = ConversationTaskModeIndependent
	if valid, err := conversationTaskAdmissionLineage(t.Context(), parent.Scope, legacy, child, getRun, getRequest); err != nil || valid {
		t.Fatalf("independent task delegation restriction widened: %v %v", valid, err)
	}
}

func TestConversationTaskContinuationCatalogAndProofUsePersistedActorAndDeadline(t *testing.T) {
	store, result, conversation, message := promotedContinuationAuthorityFixture(t, "continuation-proof")
	claim := AgentRunClaim{Scope: result.WorkRun.Scope, Kind: RunKindConversation, WorkerID: "catalog", Now: time.Now(), LeaseDuration: time.Minute, AgingInterval: time.Minute}
	run, err := store.ClaimNextAgentRun(t.Context(), claim)
	if err != nil || run == nil {
		t.Fatalf("claim continuation: %#v %v", run, err)
	}
	lowered := cloneAgentRun(run)
	lowered.Kind = RunKindAgentWork
	context, err := resolveCatalogConversationTaskContext(t.Context(), nil, lowered, store)
	if err != nil || context == nil || context.CanStart || len(context.Tasks) != 1 || context.Tasks[0].Mode != ConversationTaskModeContinuation || context.Tasks[0].Status != AgentRunStatusRunning {
		t.Fatalf("catalog lost exact continuation state or offered duplicate task: %#v %v", context, err)
	}
	for _, mutate := range []func(*ConversationTask){
		func(task *ConversationTask) { task.AuthenticatedActor.ID = "foreign-user" },
		func(task *ConversationTask) { deadline := run.CreatedAt; task.ForegroundDeadline = &deadline },
	} {
		forged := cloneConversationTask(result.Task)
		mutate(forged)
		forged.RequestDigest, err = conversationTaskDigest(forged)
		if err != nil {
			t.Fatal(err)
		}
		if validConversationTaskAdmissionSource(run, conversation, message, forged) {
			t.Fatal("forged actor or deadline established continuation authority")
		}
	}
	forged := cloneAgentRun(run)
	forged.Context[ConversationTaskContextKey] = "copied-task-marker"
	if _, _, err := conversationTaskForReport(t.Context(), store, forged); !errors.Is(err, ErrInvalidConversationTask) {
		t.Fatalf("copied marker established reporting authority: %v", err)
	}
}

func TestConversationTaskContinuationTerminalReportingReusesCanonicalAnswer(t *testing.T) {
	store, result, _, source := promotedContinuationAuthorityFixture(t, "continuation-answer")
	service := NewConversationService(store)
	conversation, err := service.GetConversation(t.Context(), result.Task.Scope, result.Task.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	// This is the valid promotion-versus-publication race: the canonical
	// answer has its exact Run reference, but the earlier lookup had no Task.
	posted, err := service.PostChannelMessage(t.Context(), PostChannelMessageRequest{
		Scope: result.Task.Scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
		Sender: ConversationParticipant{Type: ConversationParticipantAgent, ID: result.WorkRun.AssignedAgentID},
		Intent: MessageIntentAnswer, Content: "The complete release review.", Audience: ConversationAudience{Kind: ConversationAudienceChannel},
		ReplyToMessageID: source.ID, ResolvesMessageID: source.ID, BroadcastToChannel: true,
		References:     []ConversationReference{{Kind: ConversationReferenceRun, ID: result.WorkRun.ID}},
		IdempotencyKey: conversationTaskFinalResponseKey(result.WorkRun),
	})
	if err != nil {
		t.Fatal(err)
	}
	run := finishContinuationAuthorityRun(t, store, result.WorkRun, AgentRunStatusCompleted)
	for range 2 {
		if err := projectTerminalRunReporting(t.Context(), store, run); err != nil {
			t.Fatal(err)
		}
	}
	current, err := service.GetConversation(t.Context(), run.Scope, conversation.ID)
	if err != nil || current.LastSequence != posted.Message.Sequence {
		t.Fatalf("terminal reporting duplicated canonical answer: %#v %v", current, err)
	}
	restored, err := NewConversationTaskService(store).Get(t.Context(), GetConversationTaskRequest{
		Scope: result.Task.Scope, Owner: result.Task.Owner, ConversationID: result.Task.ConversationID, TaskID: result.Task.ID, AuthenticatedActor: result.Task.AuthenticatedActor,
	})
	if err != nil || restored.TerminalReportMessageID != posted.Message.ID {
		t.Fatalf("task restore lost canonical final message: %#v %v", restored, err)
	}
}

func TestConversationTaskContinuationFailureAndCancellationReportOnce(t *testing.T) {
	for _, status := range []AgentRunStatus{AgentRunStatusFailed, AgentRunStatusCanceled} {
		t.Run(string(status), func(t *testing.T) {
			store, result, _, _ := promotedContinuationAuthorityFixture(t, "continued-"+string(status))
			run := finishContinuationAuthorityRun(t, store, result.WorkRun, status)
			for range 2 {
				if err := projectTerminalRunReporting(t.Context(), store, run); err != nil {
					t.Fatal(err)
				}
			}
			message, err := FindConversationTaskResultMessage(t.Context(), store, result.Task, run)
			if err != nil || message == nil || !conversationTaskResultMessageMatches(result.Task, run, message, true) {
				t.Fatalf("terminal result lost exact task/source identity: %#v %v", message, err)
			}
			conversation, err := store.GetConversation(t.Context(), run.Scope, result.Task.ConversationID)
			if err != nil || conversation.LastSequence != message.Sequence {
				t.Fatalf("terminal result duplicated: %#v %v", conversation, err)
			}
		})
	}
}
