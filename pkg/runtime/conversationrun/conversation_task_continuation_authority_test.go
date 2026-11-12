package conversationrun

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
)

func promotedContinuationAuthorityFixture(t *testing.T, id string) (*sqlite.Store, *models.ConversationTaskResult, *models.Conversation, *models.ChannelMessage) {
	t.Helper()
	store, db := kerneltest.NewSQLiteFileStore(t)
	run, conversation, message := continuationFixture(t, store, id)
	now := time.Now().UTC()
	run = backdateContinuationRun(t, store, db, run, now.Add(-models.ConversationTaskForegroundTimeout-time.Second))
	result, err := store.PromoteConversationTask(t.Context(), models.ConversationTaskPromotionRequest{Scope: run.Scope, RunID: run.ID, Now: now})
	if err != nil || result == nil {
		t.Fatalf("promote continuation: %#v %v", result, err)
	}
	return store, result, conversation, message
}

func finishContinuationAuthorityRun(t *testing.T, store *sqlite.Store, run *models.AgentRun, status models.AgentRunStatus) *models.AgentRun {
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
	store, db := kerneltest.NewSQLiteFileStore(t)
	base, _, _ := continuationFixture(t, store, "continuation-reserve")
	now := time.Now().UTC()
	base = backdateContinuationRun(t, store, db, base, now.Add(-models.ConversationTaskForegroundTimeout-time.Second))
	for i := range 4 {
		run := models.CloneAgentRun(base)
		if i > 0 {
			run.ID, run.RootRunID = fmt.Sprintf("continued-root-%d", i), fmt.Sprintf("continued-root-%d", i)
			if err := store.CreateAgentRun(t.Context(), run); err != nil {
				t.Fatal(err)
			}
		}
		result, err := store.PromoteConversationTask(t.Context(), models.ConversationTaskPromotionRequest{Scope: run.Scope, RunID: run.ID, Now: now})
		if err != nil || result == nil {
			t.Fatalf("promotion %d: %#v %v", i, result, err)
		}
	}
	claim := models.AgentRunClaim{Scope: base.Scope, Kind: models.RunKindConversation, WorkerID: "task-reserve", Now: now.Add(time.Second),
		LeaseDuration: time.Minute, AgingInterval: time.Minute, AssignedAgentID: base.AssignedAgentID, MaxActiveForAgent: 4, ConversationTaskForegroundReserve: 1}
	for range 3 {
		decision, err := store.ClaimNextAgentRunWithDecision(t.Context(), claim)
		if err != nil || decision.Run == nil || decision.Run.Kind != models.RunKindConversation {
			t.Fatalf("claim continued root: %#v %v", decision, err)
		}
	}
	decision, err := store.ClaimNextAgentRunWithDecision(t.Context(), claim)
	if err != nil || decision.Outcome != models.AgentRunAdmissionBackpressured || decision.Run != nil {
		t.Fatalf("fourth continuation bypassed reserve: %#v %v", decision, err)
	}
	blocked := false
	for _, block := range decision.Blocks {
		if block.Reason == models.AgentRunAdmissionReasonTaskCapacity && block.Limit == 3 {
			blocked = true
		}
	}
	if !blocked {
		t.Fatalf("missing task capacity evidence: %#v", decision.Blocks)
	}
	fresh := models.CloneAgentRun(base)
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
	child := models.CloneAgentRun(parent)
	child.ID, child.ParentRunID, child.Source = "original-delegate", parent.ID, models.RunSourceHandoff
	child.Owner, child.AssignedAgentID, child.Goal = models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "specialist"}, "specialist", "Review the specialist portion"
	// A prepromotion child keeps its original kind and lane. Neither field
	// needs to be rewritten to classify its canonical delegated ancestry.
	child.Kind, child.ConcurrencyKey = models.RunKindConversation, result.Task.ConversationID
	delete(child.Context, models.ConversationTaskContextKey)
	child.Context["collaboration"] = map[string]interface{}{"requestId": "original-request"}
	request := &models.AgentRequest{ID: "original-request", Scope: child.Scope, Kind: models.AgentRequestKindHandoff, Status: models.AgentRequestStatusAccepted,
		Requester: models.CollaborationParty{Type: parent.Owner.Type, ID: parent.Owner.ID}, Recipient: models.CollaborationParty{Type: child.Owner.Type, ID: child.Owner.ID},
		SourceRunID: parent.ID, ChildRunID: child.ID, AssignedAgentID: child.AssignedAgentID, Goal: child.Goal,
		AcceptancePolicy: models.AgentRequestAcceptancePreauthorized, Revision: 2, CreatedAt: parent.CreatedAt, UpdatedAt: parent.UpdatedAt}
	if err := request.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateAgentRun(t.Context(), child); err != nil {
		t.Fatal(err)
	}
	// The handoff moves the parent to its next revision, as a delegation
	// does.
	source := storedRun(t, store, parent.Scope, parent.ID)
	source.Revision++
	if _, err := store.CreateAgentRequest(t.Context(), models.AgentRequestCreateRecord{Request: request, SourceRun: source, ExpectedSourceRevision: source.Revision - 1,
		Event: &models.ActivityEvent{ID: "original-request-created", Scope: parent.Scope, RunID: parent.ID, EventType: "agent_request.created", Summary: "Hand off the specialist portion",
			Actor: models.ActivityActor{Type: "agent", ID: parent.Owner.ID}, Visibility: models.ActivityVisibilityScope, CreatedAt: parent.UpdatedAt}}); err != nil {
		t.Fatal(err)
	}
	getRun := func(id string) (*models.AgentRun, error) { return store.GetAgentRun(t.Context(), parent.Scope, id) }
	getRequest := func(ctx context.Context, scope models.Scope, id string) (*models.AgentRequest, error) {
		return store.GetAgentRequest(ctx, scope, id)
	}
	valid, err := models.ConversationTaskAdmissionLineage(t.Context(), parent.Scope, result.Task, child, getRun, getRequest)
	if err != nil || !valid {
		t.Fatalf("original specialist delegation lost task scheduling identity: %v %v", valid, err)
	}
	claim := models.AgentRunClaim{Scope: child.Scope, Kind: models.RunKindConversation, WorkerID: "specialist", AssignedAgentID: child.AssignedAgentID,
		Now: time.Now().Add(time.Second), LeaseDuration: time.Minute, AgingInterval: time.Minute, MaxActiveForAgent: 4, ConversationTaskForegroundReserve: 1}
	// The store prepares admission with the same reads inside its claim
	// transaction; nothing writes between them here.
	err = models.PrepareConversationTaskAdmission(t.Context(), &claim, []*models.AgentRun{child}, models.ConversationTaskAdmissionLookup{
		Task: store.FindConversationTaskByWorkRunID, Run: store.GetAgentRun, Conversation: store.GetConversation,
		Message: store.GetChannelMessage, Request: store.GetAgentRequest,
		Round: func(context.Context, models.Scope, string, string) (*models.ParticipationRound, error) {
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
	if _, decision := models.EvaluateAgentRunAdmission([]*models.AgentRun{child}, nil, claim); len(decision.Blocks) != 1 ||
		decision.Blocks[0].RunID != child.ID || decision.Blocks[0].Reason != models.AgentRunAdmissionReasonTaskCapacity {
		t.Fatalf("original delegate bypassed background reserve: %#v", decision)
	}
	for _, mutate := range []func(*models.AgentRun){
		func(run *models.AgentRun) { run.AssignedAgentID = "foreign-agent" },
		func(run *models.AgentRun) { run.Scope.ID = "foreign-scope" },
		func(run *models.AgentRun) { run.ParentRunID = "unrelated-run" },
	} {
		forged := models.CloneAgentRun(child)
		mutate(forged)
		valid, err := models.ConversationTaskAdmissionLineage(t.Context(), parent.Scope, result.Task, forged, getRun, getRequest)
		if err != nil || valid {
			t.Fatalf("forged delegated ancestry established task admission: %v %v", valid, err)
		}
	}
	legacy := models.CloneConversationTask(result.Task)
	legacy.Mode = models.ConversationTaskModeIndependent
	if valid, err := models.ConversationTaskAdmissionLineage(t.Context(), parent.Scope, legacy, child, getRun, getRequest); err != nil || valid {
		t.Fatalf("independent task delegation restriction widened: %v %v", valid, err)
	}
}

func TestConversationTaskContinuationCatalogAndProofUsePersistedActorAndDeadline(t *testing.T) {
	store, result, conversation, message := promotedContinuationAuthorityFixture(t, "continuation-proof")
	claim := models.AgentRunClaim{Scope: result.WorkRun.Scope, Kind: models.RunKindConversation, WorkerID: "catalog", Now: time.Now(), LeaseDuration: time.Minute, AgingInterval: time.Minute}
	run, err := store.ClaimNextAgentRun(t.Context(), claim)
	if err != nil || run == nil {
		t.Fatalf("claim continuation: %#v %v", run, err)
	}
	lowered := models.CloneAgentRun(run)
	lowered.Kind = models.RunKindAgentWork
	context, err := ResolveCatalogConversationTaskContext(t.Context(), nil, lowered, store)
	if err != nil || context == nil || context.CanStart || len(context.Tasks) != 1 || context.Tasks[0].Mode != models.ConversationTaskModeContinuation || context.Tasks[0].Status != models.AgentRunStatusRunning {
		t.Fatalf("catalog lost exact continuation state or offered duplicate task: %#v %v", context, err)
	}
	for _, mutate := range []func(*models.ConversationTask){
		func(task *models.ConversationTask) { task.AuthenticatedActor.ID = "foreign-user" },
		func(task *models.ConversationTask) { deadline := run.CreatedAt; task.ForegroundDeadline = &deadline },
	} {
		forged := models.CloneConversationTask(result.Task)
		mutate(forged)
		forged.RequestDigest, err = models.ConversationTaskDigest(forged)
		if err != nil {
			t.Fatal(err)
		}
		if models.IsValidConversationTaskAdmissionSource(run, conversation, message, forged) {
			t.Fatal("forged actor or deadline established continuation authority")
		}
	}
	forged := models.CloneAgentRun(run)
	forged.Context[models.ConversationTaskContextKey] = "copied-task-marker"
	if _, _, err := runtimeconversation.TaskForReport(t.Context(), store, forged); !errors.Is(err, models.ErrInvalidConversationTask) {
		t.Fatalf("copied marker established reporting authority: %v", err)
	}
}

func TestConversationTaskContinuationTerminalReportingReusesCanonicalAnswer(t *testing.T) {
	store, result, _, source := promotedContinuationAuthorityFixture(t, "continuation-answer")
	service := runtimeconversation.NewService(store)
	conversation, err := service.GetConversation(t.Context(), result.Task.Scope, result.Task.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	// This is the valid promotion-versus-publication race: the canonical
	// answer has its exact Run reference, but the earlier lookup had no Task.
	posted, err := service.PostChannelMessage(t.Context(), runtimeconversation.PostChannelMessageRequest{
		Scope: result.Task.Scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
		Sender: models.ConversationParticipant{Type: models.ConversationParticipantAgent, ID: result.WorkRun.AssignedAgentID},
		Intent: models.MessageIntentAnswer, Content: "The complete release review.", Audience: models.ConversationAudience{Kind: models.ConversationAudienceChannel},
		ReplyToMessageID: source.ID, ResolvesMessageID: source.ID, BroadcastToChannel: true,
		References:     []models.ConversationReference{{Kind: models.ConversationReferenceRun, ID: result.WorkRun.ID}},
		IdempotencyKey: models.ConversationTaskFinalResponseKey(result.WorkRun),
	})
	if err != nil {
		t.Fatal(err)
	}
	run := finishContinuationAuthorityRun(t, store, result.WorkRun, models.AgentRunStatusCompleted)
	for range 2 {
		if err := ProjectTerminalRunReporting(t.Context(), store, run); err != nil {
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
	for _, status := range []models.AgentRunStatus{models.AgentRunStatusFailed, models.AgentRunStatusCanceled} {
		t.Run(string(status), func(t *testing.T) {
			store, result, _, _ := promotedContinuationAuthorityFixture(t, "continued-"+string(status))
			run := finishContinuationAuthorityRun(t, store, result.WorkRun, status)
			for range 2 {
				if err := ProjectTerminalRunReporting(t.Context(), store, run); err != nil {
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
