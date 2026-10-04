package runtime

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func promotedContinuationAuthorityFixture(t *testing.T, id string) (*MemoryStore, *ConversationTaskResult, *Conversation, *ChannelMessage) {
	t.Helper()
	store := NewMemoryStore()
	run, conversation, message := continuationLifecycleFixture(t, store, id)
	now := time.Now().UTC()
	run = backdateContinuationLifecycleRun(t, store, run, now.Add(-ConversationTaskForegroundTimeout-time.Second))
	result, err := store.PromoteConversationTask(t.Context(), ConversationTaskPromotionRequest{Scope: run.Scope, RunID: run.ID, Now: now})
	if err != nil || result == nil {
		t.Fatalf("promote continuation: %#v %v", result, err)
	}
	return store, result, conversation, message
}

func finishContinuationAuthorityRun(t *testing.T, store *MemoryStore, run *AgentRun, status AgentRunStatus) *AgentRun {
	t.Helper()
	store.mu.Lock()
	defer store.mu.Unlock()
	current := cloneAgentRun(store.agentRuns[portfolioKey(run.Scope, run.ID)])
	now := time.Now().UTC()
	current.Status, current.LeaseOwner, current.LeaseExpiresAt = status, "", nil
	current.CompletedAt, current.UpdatedAt = &now, now
	current.Revision++
	store.saveMemoryAgentRunLocked(portfolioKey(current.Scope, current.ID), current)
	return cloneAgentRun(current)
}

func TestConversationTaskContinuationUsesTaskReserveAndLeavesForegroundSlot(t *testing.T) {
	store := NewMemoryStore()
	base, _, _ := continuationLifecycleFixture(t, store, "continuation-reserve")
	now := time.Now().UTC()
	base = backdateContinuationLifecycleRun(t, store, base, now.Add(-ConversationTaskForegroundTimeout-time.Second))
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
	store.mu.Lock()
	store.requests[requestStoreKey(request.Scope, request.ID)] = cloneAgentRequest(request)
	store.mu.Unlock()
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
	store.mu.Lock()
	err = store.prepareConversationTaskAdmissionLocked(t.Context(), &claim, []*AgentRun{child})
	store.mu.Unlock()
	if err != nil || !claim.conversationTaskRuns[child.ID] {
		t.Fatalf("original delegate bypassed background reserve: %#v %v", claim.conversationTaskRuns, err)
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

func TestConversationTaskContinuationTeamAssignmentRequiresCanonicalSelectedAction(t *testing.T) {
	store := NewMemoryStore()
	service := NewConversationService(store)
	scope := Scope{Kind: "tenant", ID: "continued-team-action"}
	conversation, _, err := service.CreateConversation(t.Context(), CreateConversationRequest{Scope: scope,
		Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "release-team"}, Title: "Release Team", IdempotencyKey: "continued-team-action"})
	if err != nil {
		t.Fatal(err)
	}
	message := postConversationRunTestMessage(t, service, conversation, ConversationParticipantUser, MessageIntentQuestion, "Review the release and propose the action", "continued-team-request")
	scheduled, _, err := mustConversationRunScheduler(t, store).ScheduleMessage(t.Context(), scope, conversation.ID, message.ID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	run := backdateContinuationLifecycleRun(t, store, scheduled.Run, now.Add(-ConversationTaskForegroundTimeout-time.Second))
	result, err := store.PromoteConversationTask(t.Context(), ConversationTaskPromotionRequest{Scope: scope, RunID: run.ID, Now: now})
	if err != nil || result == nil || result.Task.TargetAgentID != "" {
		t.Fatalf("unassigned Team continuation: %#v %v", result, err)
	}
	roundID := "continued-team-round"
	proposal := ParticipationProposal{ID: "selected-action", RoundID: roundID, Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: "release-reviewer"},
		WantsToSpeak: true, Intent: MessageIntentProposal, Content: "Propose the release review action", Audience: ConversationAudience{Kind: ConversationAudienceChannel},
		Signals:        ParticipationSignals{RoleRelevant: true, CoordinatesWork: true},
		ProposedAction: &TurnAction{Type: "skill_action", Capability: "release.review", Summary: "Review release", InputRef: "/actionInputs/review"},
		ActionInputs:   map[string]interface{}{"actionInputs": map[string]interface{}{"review": map[string]interface{}{"release": "canary"}}}}
	round := &ParticipationRound{ID: roundID, Scope: scope, ConversationID: conversation.ID, TriggerMessageID: message.ID, Status: ParticipationRoundCommitted,
		Policy: DefaultConversationArbitrationPolicy(), Proposals: []ParticipationProposal{proposal}, Arbitration: ConversationArbitration{RoundID: roundID, Speakers: []string{proposal.ID}},
		IdempotencyKey: conversationTaskLedgerParticipationRoundKey(result.Task), Revision: 1, CreatedAt: now, CommittedAt: now}
	if err := round.Validate(); err != nil {
		t.Fatal(err)
	}
	current, err := store.GetConversation(t.Context(), scope, conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	next := cloneConversation(current)
	next.Revision++
	if _, err := store.CommitParticipationRound(t.Context(), ParticipationRoundCommitRecord{Conversation: next, ExpectedRevision: current.Revision, Round: round}); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	assigned := cloneAgentRun(store.agentRuns[portfolioKey(scope, run.ID)])
	assigned.AssignedAgentID, assigned.Status = proposal.Participant.ID, AgentRunStatusWaitingForApproval
	assigned.Revision++
	store.saveMemoryAgentRunLocked(portfolioKey(scope, run.ID), assigned)
	store.mu.Unlock()
	if valid, err := VerifyConversationTaskWorkRun(t.Context(), store, result.Task, assigned); err != nil || !valid {
		t.Fatalf("canonical Team action assignment rejected: %v %v", valid, err)
	}
	if validConversationTaskAdmissionSource(assigned, current, message, result.Task) {
		t.Fatal("structural Team assignment passed without selected round")
	}
	foreignRound := cloneParticipationRound(round)
	foreignRound.Scope.ID = "foreign-team-scope"
	if validConversationTaskAdmissionSourceWithRound(assigned, current, message, result.Task, foreignRound) {
		t.Fatal("foreign participation round established Team task authority")
	}
	wrong := cloneAgentRun(assigned)
	wrong.AssignedAgentID = "unselected-agent"
	if validConversationTaskAdmissionSourceWithRound(wrong, current, message, result.Task, round) {
		t.Fatal("unselected Team Agent established Task authority")
	}
	wrong.Checkpoint = map[string]interface{}{teamActionAssignedAgentCheckpointKey: proposal.Participant.ID}
	if validConversationTaskAdmissionSource(wrong, current, message, result.Task) {
		t.Fatal("copied attribution checkpoint replaced canonical selected round")
	}
	finished := finishContinuationAuthorityRun(t, store, assigned, AgentRunStatusFailed)
	if err := projectTerminalRunReporting(t.Context(), store, finished); err != nil {
		t.Fatal(err)
	}
	if err := projectTerminalRunReporting(t.Context(), store, finished); err != nil {
		t.Fatal(err)
	}
	if final, err := FindConversationTaskResultMessage(t.Context(), store, result.Task, finished); err != nil || final == nil {
		t.Fatalf("Team terminal result lost task identity: %#v %v", final, err)
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
