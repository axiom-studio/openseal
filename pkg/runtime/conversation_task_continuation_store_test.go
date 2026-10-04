package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"
)

type conversationContinuationTestStore interface {
	ConversationTaskKernelStore
	ConversationTaskPromotionStore
	ConversationTaskDueStore
}

func forConversationContinuationStores(t *testing.T, test func(*testing.T, conversationContinuationTestStore, func(*testing.T) conversationContinuationTestStore)) {
	t.Helper()
	t.Run("memory", func(t *testing.T) {
		store := NewMemoryStore()
		test(t, store, func(*testing.T) conversationContinuationTestStore { return store })
	})
	forConversationTaskSQLStores(t, func(t *testing.T, fixture conversationTaskSQLTestFixture) {
		test(t, fixture.store.(conversationContinuationTestStore), func(t *testing.T) conversationContinuationTestStore {
			return fixture.reopen(t).(conversationContinuationTestStore)
		})
	})
}

func conversationContinuationFixture(t *testing.T, store conversationContinuationTestStore, status AgentRunStatus, spoken bool) (*AgentRun, *Conversation, *ChannelMessage) {
	t.Helper()
	scope := Scope{Kind: "tenant", ID: "continuation"}
	owner := ObjectiveOwner{Type: OwnerTypeAgent, ID: "continuation-agent"}
	service := NewConversationService(store)
	conversation, _, err := service.CreateConversation(t.Context(), CreateConversationRequest{Scope: scope, Owner: owner, Title: "Continuation", IdempotencyKey: "continuation"})
	if err != nil {
		t.Fatal(err)
	}
	mode := ""
	if spoken {
		mode = "spoken"
	}
	posted, err := service.PostChannelMessage(t.Context(), PostChannelMessageRequest{Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
		Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "requester"}, Intent: MessageIntentQuestion, Content: "Research this request and save the requested result.",
		Audience: ConversationAudience{Kind: ConversationAudienceChannel}, RequiresResponse: true, ResponseMode: mode, IdempotencyKey: "continuation-source"})
	if err != nil {
		t.Fatal(err)
	}
	portfolio := NewPortfolioService(store)
	portfolio.now = func() time.Time { return time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond) }
	run, err := portfolio.CreateAgentRun(t.Context(), CreateAgentRunRequest{Scope: scope, Kind: RunKindConversation, Owner: owner, AssignedAgentID: owner.ID,
		ConcurrencyKey: conversation.ID, Goal: "Respond to an Agent channel message", Source: RunSourceChat, Budget: &BudgetPolicy{MaxTurns: 20, MaxAttempts: 30, MaxDurationMS: 600000},
		Context: map[string]interface{}{conversationRunContextConversationID: conversation.ID, conversationRunContextTriggerID: posted.Message.ID},
		Plan:    map[string]interface{}{"accepted": "plan"}, Checkpoint: map[string]interface{}{"lastAction": map[string]interface{}{"actionCallId": "existing-action", "status": "running"}}})
	if err != nil {
		t.Fatal(err)
	}
	previous := run.Revision
	run.Status = status
	if status == AgentRunStatusRunning {
		expires := time.Now().Add(time.Minute)
		run.LeaseOwner = "same-worker"
		run.LeaseExpiresAt = &expires
	}
	if status == AgentRunStatusWaitingForApproval {
		run.WakeCondition = &WakeCondition{Type: "approval", Reference: "pending-approval"}
	}
	run.BudgetUsage = BudgetUsage{Attempts: 1, Turns: 1, DurationMS: 321}
	run.BudgetReservations = map[string]BudgetReservation{"existing-turn": {ID: "existing-turn", Usage: BudgetUsage{Turns: 1, DurationMS: 1000}, CreatedAt: run.CreatedAt}}
	run.PendingInterventions = []AgentRunIntervention{{ID: "steering", Actor: ActivityActor{Type: "user", ID: "requester"}, Instruction: "Preserve the saved guidance", CreatedAt: run.CreatedAt}}
	run.Revision++
	updateConversationTaskSQLRun(t, store, run, previous)
	canonical, err := store.GetAgentRun(t.Context(), run.Scope, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	return canonical, posted.Conversation, posted.Message
}

func TestConversationTaskContinuationStorePreservesExecutionAndAcknowledgesAtomically(t *testing.T) {
	for _, status := range []AgentRunStatus{AgentRunStatusQueued, AgentRunStatusRunning, AgentRunStatusWaitingForApproval} {
		t.Run(string(status), func(t *testing.T) {
			forConversationContinuationStores(t, func(t *testing.T, store conversationContinuationTestStore, reopen func(*testing.T) conversationContinuationTestStore) {
				source, conversation, trigger := conversationContinuationFixture(t, store, status, true)
				deadline := source.CreatedAt.Add(ConversationTaskForegroundTimeout)
				before, err := store.PromoteConversationTask(t.Context(), ConversationTaskPromotionRequest{Scope: source.Scope, RunID: source.ID, Now: deadline.Add(-time.Nanosecond)})
				if err != nil || before != nil {
					t.Fatalf("promoted before deadline: %#v %v", before, err)
				}
				accepted, err := store.PromoteConversationTask(t.Context(), ConversationTaskPromotionRequest{Scope: source.Scope, RunID: source.ID, Now: deadline})
				if err != nil {
					t.Fatal(err)
				}
				if accepted == nil || accepted.Task.Mode != ConversationTaskModeContinuation || accepted.Task.SourceRunID != source.ID || accepted.Task.WorkRunID != source.ID ||
					accepted.Task.SourceTurnID != "" || accepted.Task.SourceTurnNumber != 0 || !ConversationTaskMatchesWorkRun(accepted.Task, accepted.WorkRun) {
					t.Fatalf("lost exact continuation identity: %#v", accepted)
				}
				expected := cloneAgentRun(source)
				expected.Context[ConversationTaskContextKey] = accepted.Task.ID
				expected.ConcurrencyKey = "task:" + accepted.Task.ID
				expected.Revision++
				expected.UpdatedAt = deadline.UTC()
				want, _ := json.Marshal(expected)
				got, _ := json.Marshal(accepted.WorkRun)
				if string(want) != string(got) {
					t.Fatalf("promotion changed execution state\nwant %s\ngot %s", want, got)
				}
				ack, err := store.GetChannelMessage(t.Context(), source.Scope, conversation.ID, accepted.AcknowledgmentMessageID)
				if err != nil || ack == nil || ack.Content != accepted.Task.Acknowledgment || ack.Intent != MessageIntentUpdate || ack.ResponseMode != "spoken" ||
					ack.ReplyToMessageID != trigger.ID || ack.ResolvesMessageID != "" || len(ack.References) != 2 {
					t.Fatalf("acknowledgment was not committed with work: %#v %v", ack, err)
				}
				restarted := reopen(t)
				again, err := restarted.PromoteConversationTask(t.Context(), ConversationTaskPromotionRequest{Scope: source.Scope, RunID: source.ID, Now: deadline.Add(time.Second)})
				if err != nil || again == nil || !again.Replayed || again.Task.ID != accepted.Task.ID || again.AcknowledgmentMessageID != ack.ID {
					t.Fatalf("restart duplicated handoff: %#v %v", again, err)
				}
				runs, err := restarted.ListAgentRuns(t.Context(), AgentRunFilter{Scope: source.Scope, Limit: 100})
				if err != nil || len(runs) != 1 {
					t.Fatalf("handoff created another Run: %d %v", len(runs), err)
				}
				due, err := restarted.ListDueConversationTaskRuns(t.Context(), ConversationTaskDueFilter{Scope: source.Scope, BeforeCreatedAt: deadline, Limit: 10})
				if err != nil || len(due) != 0 {
					t.Fatalf("promoted Run remained foreground due work: %#v %v", due, err)
				}
			})
		})
	}
}

func TestConversationTaskContinuationStoreFinalAnswerWinsHandoff(t *testing.T) {
	forConversationContinuationStores(t, func(t *testing.T, store conversationContinuationTestStore, _ func(*testing.T) conversationContinuationTestStore) {
		source, conversation, trigger := conversationContinuationFixture(t, store, AgentRunStatusRunning, false)
		_, err := NewConversationService(store).PostChannelMessage(t.Context(), PostChannelMessageRequest{Scope: source.Scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
			Sender: ConversationParticipant{Type: ConversationParticipantAgent, ID: source.AssignedAgentID}, Intent: MessageIntentAnswer, Content: "The result is complete.",
			Audience: ConversationAudience{Kind: ConversationAudienceChannel}, ReplyToMessageID: trigger.ID, ResolvesMessageID: trigger.ID,
			References: []ConversationReference{{Kind: ConversationReferenceRun, ID: source.ID}}, IdempotencyKey: conversationTaskFinalResponseKey(source)})
		if err != nil {
			t.Fatal(err)
		}
		result, err := store.PromoteConversationTask(t.Context(), ConversationTaskPromotionRequest{Scope: source.Scope, RunID: source.ID, Now: time.Now()})
		if err != nil || result != nil {
			t.Fatalf("already answered work was handed off: %#v %v", result, err)
		}
		current, err := store.GetAgentRun(t.Context(), source.Scope, source.ID)
		if err != nil || !reflect.DeepEqual(current, source) {
			t.Fatalf("rejected promotion changed Run: %#v %v", current, err)
		}
	})
}

func TestConversationTaskContinuationStoreConcurrentReplay(t *testing.T) {
	forConversationContinuationStores(t, func(t *testing.T, store conversationContinuationTestStore, _ func(*testing.T) conversationContinuationTestStore) {
		source, conversation, _ := conversationContinuationFixture(t, store, AgentRunStatusRunning, false)
		var wg sync.WaitGroup
		results := make(chan *ConversationTaskResult, 8)
		errs := make(chan error, 8)
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				result, err := store.PromoteConversationTask(context.Background(), ConversationTaskPromotionRequest{Scope: source.Scope, RunID: source.ID, Now: time.Now()})
				results <- result
				errs <- err
			}()
		}
		wg.Wait()
		close(results)
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		id := ""
		for result := range results {
			if result == nil {
				t.Fatal("missing accepted continuation")
			}
			if id == "" {
				id = result.Task.ID
			}
			if result.Task.ID != id || result.WorkRun.ID != source.ID {
				t.Fatal("replay changed exact work")
			}
		}
		messages, err := store.ListChannelMessages(t.Context(), ChannelMessageFilter{Scope: source.Scope, ConversationID: conversation.ID, Limit: 100})
		if err != nil || len(messages) != 2 {
			t.Fatalf("replay duplicated acknowledgment: %d %v", len(messages), err)
		}
	})
}

func TestConversationTaskContinuationStoreRejectsWeakenedDeadline(t *testing.T) {
	store := NewMemoryStore()
	source, _, _ := conversationContinuationFixture(t, store, AgentRunStatusQueued, false)
	_, err := store.PromoteConversationTask(t.Context(), ConversationTaskPromotionRequest{Scope: source.Scope, RunID: source.ID, Now: time.Now(), MinimumAge: time.Second})
	if !errors.Is(err, ErrInvalidConversationTask) {
		t.Fatalf("weakened policy accepted: %v", err)
	}
}

func TestConversationTaskContinuationStoreDueNullMarkerAndKeyset(t *testing.T) {
	forConversationContinuationStores(t, func(t *testing.T, store conversationContinuationTestStore, _ func(*testing.T) conversationContinuationTestStore) {
		source, _, _ := conversationContinuationFixture(t, store, AgentRunStatusQueued, false)
		previous := source.Revision
		source.Context[ConversationTaskContextKey] = nil
		source.Revision++
		updateConversationTaskSQLRun(t, store, source, previous)
		other := cloneAgentRun(source)
		other.ID = "null-marker-other"
		other.RootRunID = other.ID
		other.Revision = 1
		if err := store.CreateAgentRun(t.Context(), other); err != nil {
			t.Fatal(err)
		}
		filter := ConversationTaskDueFilter{Scope: source.Scope, BeforeCreatedAt: time.Now(), Limit: 100}
		page, err := store.ListDueConversationTaskRuns(t.Context(), filter)
		if err != nil || len(page) != 2 {
			t.Fatalf("nullable marker hid overdue roots: %#v %v", page, err)
		}
		if page[0].ID >= page[1].ID || !page[0].CreatedAt.Equal(page[1].CreatedAt) {
			t.Fatalf("same-time roots lost stable order: %#v", page)
		}
		filter.AfterCreatedAt = &page[0].CreatedAt
		filter.AfterID = page[0].ID
		after, err := store.ListDueConversationTaskRuns(t.Context(), filter)
		if err != nil || len(after) != 1 || after[0].ID != page[1].ID {
			t.Fatalf("due cursor repeated or skipped work: %#v %v", after, err)
		}
		accepted, err := store.PromoteConversationTask(t.Context(), ConversationTaskPromotionRequest{Scope: source.Scope, RunID: source.ID, Now: time.Now()})
		if err != nil || accepted == nil {
			t.Fatalf("nullable marker prevented handoff: %#v %v", accepted, err)
		}
		filter.AfterCreatedAt = nil
		filter.AfterID = ""
		due, err := store.ListDueConversationTaskRuns(t.Context(), filter)
		if err != nil || len(due) != 1 || due[0].ID != other.ID {
			t.Fatalf("index retained promoted work: %#v %v", due, err)
		}
	})
}

func TestConversationTaskContinuationStoreFinishedTurnWinsHandoff(t *testing.T) {
	forConversationContinuationStores(t, func(t *testing.T, store conversationContinuationTestStore, _ func(*testing.T) conversationContinuationTestStore) {
		source, conversation, _ := conversationContinuationFixture(t, store, AgentRunStatusRunning, false)
		now := time.Now().UTC()
		turn := &AgentTurn{ID: "finished-original-turn", Scope: source.Scope, RunID: source.ID, Sequence: source.LastAppliedTurn + 1,
			Status: AgentTurnStatusCompleted, NextRunStatus: AgentRunStatusCompleted, RunOutput: map[string]interface{}{"summary": "The original request is complete."},
			Revision: 1, CreatedAt: source.CreatedAt, UpdatedAt: now, StartedAt: source.CreatedAt, CompletedAt: &now}
		if _, err := store.CreateAgentTurn(t.Context(), turn); err != nil {
			t.Fatal(err)
		}
		accepted, err := store.PromoteConversationTask(t.Context(), ConversationTaskPromotionRequest{Scope: source.Scope, RunID: source.ID, Now: now})
		if err != nil || accepted != nil {
			t.Fatalf("finished original turn was handed off: %#v %v", accepted, err)
		}
		current, err := store.GetAgentRun(t.Context(), source.Scope, source.ID)
		if err != nil || !reflect.DeepEqual(current, source) {
			t.Fatalf("finished-turn rejection changed accepted execution: %#v %v", current, err)
		}
		messages, err := store.ListChannelMessages(t.Context(), ChannelMessageFilter{Scope: source.Scope, ConversationID: conversation.ID, Limit: 100})
		if err != nil || len(messages) != 1 {
			t.Fatalf("finished-turn rejection emitted acknowledgment: %#v %v", messages, err)
		}
	})
}

func TestConversationTaskContinuationStoreFinishedTeamRoundWinsHandoff(t *testing.T) {
	forConversationContinuationStores(t, func(t *testing.T, store conversationContinuationTestStore, _ func(*testing.T) conversationContinuationTestStore) {
		scope := Scope{Kind: "tenant", ID: "finished-team-continuation"}
		service := NewConversationService(store)
		conversation, _, err := service.CreateConversation(t.Context(), CreateConversationRequest{Scope: scope,
			Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "review-team"}, Title: "Finished Team request", IdempotencyKey: "finished-team"})
		if err != nil {
			t.Fatal(err)
		}
		posted, err := service.PostChannelMessage(t.Context(), PostChannelMessageRequest{Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
			Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "requester"}, Intent: MessageIntentQuestion, Content: "Has the release review finished?",
			Audience: ConversationAudience{Kind: ConversationAudienceChannel}, RequiresResponse: true, IdempotencyKey: "finished-team-source"})
		if err != nil {
			t.Fatal(err)
		}
		portfolio := NewPortfolioService(store)
		portfolio.now = func() time.Time { return time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond) }
		run, err := portfolio.CreateAgentRun(t.Context(), CreateAgentRunRequest{Scope: scope, Kind: RunKindConversation, Owner: conversation.Owner,
			ConcurrencyKey: conversation.ID, Goal: "Respond to a Team channel message", Source: RunSourceChat,
			Context: map[string]interface{}{conversationRunContextConversationID: conversation.ID, conversationRunContextTriggerID: posted.Message.ID}})
		if err != nil {
			t.Fatal(err)
		}
		coordinator, err := NewConversationCoordinator(service, ConversationParticipantSourceFunc(func(context.Context, ConversationParticipantQuery) ([]ConversationParticipantBinding, error) {
			return []ConversationParticipantBinding{{Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: "reviewer"}, SemanticRoles: []string{"reviewer"}}}, nil
		}), ParticipationProposalProviderFunc(func(context.Context, ParticipationProposalContext) (ParticipationProposal, error) {
			return ParticipationProposal{WantsToSpeak: true, Intent: MessageIntentAnswer, Content: "The release review is complete.", Audience: ConversationAudience{Kind: ConversationAudienceChannel},
				ReplyToMessageID: posted.Message.ID, Signals: ParticipationSignals{AnswersOpenQuestion: true, HasNewInformation: true, RoleRelevant: true}}, nil
		}), ConversationCoordinatorConfig{MaximumParticipants: 1, MaximumConcurrency: 1, RecentMessageLimit: 20, ProposalTimeout: time.Second, PresenceTTL: time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		round, err := coordinator.Coordinate(t.Context(), ConversationCoordinationRequest{Scope: scope, ConversationID: conversation.ID, ExpectedRevision: posted.Conversation.Revision,
			TriggerMessageID: posted.Message.ID, MessageReferences: []ConversationReference{{Kind: ConversationReferenceRun, ID: run.ID}}, IdempotencyKey: conversationTaskParticipationRoundKey(run)})
		if err != nil || round == nil || len(round.Messages) == 0 || selectedParticipationAction(round.Round) != nil {
			t.Fatalf("Team answer fixture failed: %#v %v", round, err)
		}
		accepted, err := store.PromoteConversationTask(t.Context(), ConversationTaskPromotionRequest{Scope: scope, RunID: run.ID, Now: time.Now()})
		if err != nil || accepted != nil {
			t.Fatalf("finished Team round was handed off: %#v %v", accepted, err)
		}
		current, err := store.GetAgentRun(t.Context(), scope, run.ID)
		if err != nil || !reflect.DeepEqual(current, run) {
			t.Fatalf("Team completion rejection changed Run: %#v %v", current, err)
		}
		messages, err := store.ListChannelMessages(t.Context(), ChannelMessageFilter{Scope: scope, ConversationID: conversation.ID, Limit: 100})
		if err != nil || len(messages) != 1+len(round.Messages) {
			t.Fatalf("finished Team round emitted handoff acknowledgment: %#v %v", messages, err)
		}
	})
}
