package runtime

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

type conversationTaskClarificationStore interface {
	KernelStore
	ConversationStore
	ConversationTaskStore
}

type conversationTaskClarificationFixture struct {
	store        conversationTaskClarificationStore
	service      *ConversationService
	scheduler    *ConversationRunScheduler
	conversation *Conversation
	trigger      *ChannelMessage
	task         *ConversationTask
	source, work *AgentRun
	questionTurn *AgentTurn
}

func forConversationTaskClarificationStores(t *testing.T, test func(*testing.T, conversationTaskClarificationStore)) {
	t.Helper()
	t.Run("memory", func(t *testing.T) { test(t, NewMemoryStore()) })
	t.Run("sqlite", func(t *testing.T) {
		store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "task-clarification.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Close() })
		test(t, store)
	})
}

func newConversationTaskClarificationFixture(t *testing.T, store conversationTaskClarificationStore, threaded bool) *conversationTaskClarificationFixture {
	t.Helper()
	service := NewConversationService(store)
	conversation, _, err := service.CreateConversation(t.Context(), CreateConversationRequest{
		Scope: Scope{Kind: "organization", ID: "task-clarification"}, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "writer"},
		Title: "Independent writing task", IdempotencyKey: "task-clarification",
	})
	if err != nil {
		t.Fatal(err)
	}
	f := &conversationTaskClarificationFixture{store: store, service: service, conversation: conversation}
	f.resetScheduler(t)
	f.trigger = f.post(t, PostChannelMessageRequest{
		Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "requester"}, Intent: MessageIntentQuestion,
		Content:          "Write a 1000-character introduction to black holes for the requested audience.",
		RequiresResponse: true, StartThread: threaded, IdempotencyKey: "original-request",
	})
	scheduled, _, err := f.scheduler.ScheduleMessage(t.Context(), conversation.Scope, conversation.ID, f.trigger.ID)
	if err != nil || scheduled == nil || scheduled.Run == nil {
		t.Fatalf("schedule source: %#v, %v", scheduled, err)
	}
	claimed, err := store.ClaimNextAgentRun(t.Context(), AgentRunClaim{
		Scope: conversation.Scope, Kind: RunKindConversation, WorkerID: "worker", Now: time.Now(),
		LeaseDuration: time.Minute, AgingInterval: time.Minute,
	})
	if err != nil || claimed == nil || claimed.ID != scheduled.Run.ID {
		t.Fatalf("claim source: %#v, %v", claimed, err)
	}
	proposal := &TurnTaskProposal{TaskKey: "write-introduction", Goal: f.trigger.Content, Acknowledgment: "I have started the introduction."}
	settled, err := NewTurnCoordinator(store, store, store).Advance(t.Context(), AdvanceAgentRunRequest{
		Scope: conversation.Scope, RunID: claimed.ID, WorkerID: "worker", DefinitionID: "writer", DefinitionVersion: "1",
	}, TurnRunnerFunc(func(context.Context, TurnExecutionContext) (*TurnOutcome, error) {
		return &TurnOutcome{NextRunStatus: AgentRunStatusRunning, ProposedTask: proposal}, nil
	}))
	if err != nil || settled == nil || settled.Run == nil || settled.Turn == nil || settled.Turn.RequestedTask == nil {
		t.Fatalf("settle source proposal: %#v, %v", settled, err)
	}
	accepted, err := NewConversationTaskService(store).Start(t.Context(), StartConversationTaskRequest{
		Scope: conversation.Scope, SourceRunID: settled.Run.ID, ExpectedSourceRevision: settled.Run.Revision,
		WorkerID: "worker", TurnID: settled.Turn.ID, AssignedAgentID: settled.Run.AssignedAgentID,
		TaskKey: proposal.TaskKey, Goal: proposal.Goal, Acknowledgment: proposal.Acknowledgment,
	})
	if err != nil || accepted == nil || accepted.Task == nil || accepted.SourceRun == nil || accepted.WorkRun == nil {
		t.Fatalf("admit independent task: %#v, %v", accepted, err)
	}
	f.task, f.source, f.work = accepted.Task, accepted.SourceRun, accepted.WorkRun
	if f.source.Status != AgentRunStatusCompleted || f.source.Output["summary"] != proposal.Acknowledgment ||
		f.work.Kind != RunKindAgentWork || f.work.ParentRunID != "" || f.work.RootRunID != f.work.ID || !ConversationTaskMatchesWorkRun(f.task, f.work) {
		t.Fatalf("admission lost independent execution identity: %#v", accepted)
	}
	f.ask(t, "Should I write for schoolchildren or adult general readers?")
	return f
}

func (f *conversationTaskClarificationFixture) ask(t *testing.T, question string) {
	t.Helper()
	claimed, err := f.store.ClaimNextAgentRun(t.Context(), AgentRunClaim{
		Scope: f.task.Scope, Kind: RunKindAgentWork, WorkerID: "task-worker", Now: time.Now(),
		LeaseDuration: time.Minute, AgingInterval: time.Minute,
	})
	if err != nil || claimed == nil || claimed.ID != f.work.ID {
		t.Fatalf("claim independent work: %#v, %v", claimed, err)
	}
	settled, err := NewTurnCoordinator(f.store, f.store, f.store).Advance(t.Context(), AdvanceAgentRunRequest{
		Scope: f.task.Scope, RunID: claimed.ID, WorkerID: "task-worker", DefinitionID: "writer", DefinitionVersion: "1",
	}, TurnRunnerFunc(func(context.Context, TurnExecutionContext) (*TurnOutcome, error) {
		return &TurnOutcome{
			NextRunStatus: AgentRunStatusWaitingForEvent, WakeCondition: &WakeCondition{Type: "user_message", Reference: "user:next_message"},
			OutputSummary: question, RunOutput: map[string]interface{}{"summary": question},
		}, nil
	}))
	if err != nil || settled == nil || settled.Run == nil || settled.Turn == nil || settled.Turn.Status != AgentTurnStatusCompleted ||
		settled.Run.Status != AgentRunStatusWaitingForEvent || settled.Run.LastAppliedTurn != settled.Turn.Sequence {
		t.Fatalf("settle independent task clarification: %#v, %v", settled, err)
	}
	f.work, f.questionTurn = settled.Run, settled.Turn
}

func (f *conversationTaskClarificationFixture) resetScheduler(t *testing.T) {
	t.Helper()
	var err error
	f.scheduler, err = NewConversationRunScheduler(f.store, f.store, ConversationRunSchedulerConfig{MessagePageSize: 2})
	if err != nil {
		t.Fatal(err)
	}
}

func (f *conversationTaskClarificationFixture) post(t *testing.T, req PostChannelMessageRequest) *ChannelMessage {
	t.Helper()
	current, err := f.service.GetConversation(t.Context(), f.conversation.Scope, f.conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	req.Scope, req.ConversationID, req.ExpectedRevision = current.Scope, current.ID, current.Revision
	req.Audience = ConversationAudience{Kind: ConversationAudienceChannel}
	posted, err := f.service.PostChannelMessage(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	return posted.Message
}

func (f *conversationTaskClarificationFixture) transition(t *testing.T, req RunTransitionRequest) {
	t.Helper()
	req.ExpectedRevision = f.work.Revision
	var err error
	f.work, _, err = NewRunActivityService(f.store, f.store).TransitionRun(t.Context(), f.work.Scope, f.work.ID, req)
	if err != nil {
		t.Fatal(err)
	}
}

func (f *conversationTaskClarificationFixture) question(t *testing.T) *ChannelMessage {
	t.Helper()
	if _, err := f.scheduler.ReconcileScope(t.Context(), f.task.Scope); err != nil {
		t.Fatal(err)
	}
	question, err := f.store.FindChannelMessageByIdempotencyKey(t.Context(), f.task.Scope, f.conversation.ID, clarificationQuestionKey(f.work))
	if err != nil || question == nil {
		t.Fatalf("independent task did not project its question: %#v, %v", question, err)
	}
	if question.Content != f.work.Output["summary"] || question.Intent != MessageIntentQuestion ||
		!question.RequiresResponse || question.Sender != (ConversationParticipant{Type: ConversationParticipantAgent, ID: f.task.TargetAgentID}) ||
		question.ReplyToMessageID != f.trigger.ID || question.ThreadRootID != f.task.ThreadRootID {
		t.Fatalf("question lost content or canonical delivery: %#v", question)
	}
	for _, want := range []ConversationReference{{Kind: ConversationReferenceRun, ID: f.work.ID}, {Kind: ConversationReferenceTask, ID: f.task.ID}} {
		found := false
		for _, reference := range question.References {
			if reference.Kind == want.Kind && reference.ID == want.ID {
				found = true
			}
		}
		if !found {
			t.Fatalf("question missing canonical reference %#v: %#v", want, question.References)
		}
	}
	return question
}

func (f *conversationTaskClarificationFixture) assertSingleForeground(t *testing.T) {
	t.Helper()
	runs, err := f.store.ListAgentRuns(t.Context(), AgentRunFilter{Scope: f.task.Scope, Kind: RunKindConversation, Limit: 100})
	if err != nil || len(runs) != 1 || runs[0].ID != f.source.ID || runs[0].Status != AgentRunStatusCompleted {
		t.Fatalf("clarification created or reopened foreground work: %#v, %v", runs, err)
	}
}

func (f *conversationTaskClarificationFixture) assertAnswerReceipt(t *testing.T, question, answer *ChannelMessage, count int) {
	t.Helper()
	current, err := f.store.GetAgentRun(t.Context(), f.task.Scope, f.work.ID)
	if err != nil || current == nil || len(current.PendingInterventions) != count {
		t.Fatalf("durable interventions = %#v, %v; want %d", current, err, count)
	}
	if count == 1 {
		intervention := current.PendingInterventions[0]
		if intervention.Actor != (ActivityActor{Type: "user", ID: f.task.AuthenticatedActor.ID}) || intervention.Instruction != "Answer to your clarification:\n"+answer.Content {
			t.Fatalf("answer was not delivered to the same task: %#v", intervention)
		}
		want := &ConversationAnswerReceipt{
			Scope: f.task.Scope, RunID: f.work.ID, AgentID: f.task.TargetAgentID, ConversationTaskID: f.task.ID,
			ConversationID: f.conversation.ID, SourceRunID: f.source.ID, SourceMessageID: f.trigger.ID,
			ThreadRootMessageID: f.task.ThreadRootID, QuestionMessageID: question.ID, QuestionContent: question.Content,
			QuestionTurnID: f.questionTurn.ID, QuestionTurnSequence: f.questionTurn.Sequence,
			AnswerMessageID: answer.ID, AnswerContent: answer.Content, AuthenticatedActor: f.task.AuthenticatedActor,
		}
		if !reflect.DeepEqual(intervention.ConversationAnswer, want) {
			t.Fatalf("answer receipt lost canonical authority: got %#v, want %#v", intervention.ConversationAnswer, want)
		}
	}
	events, err := f.store.ListActivity(t.Context(), ActivityFilter{Scope: f.task.Scope, RunID: f.work.ID, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	eventCount := 0
	for _, event := range events {
		if event.EventType != conversationAnswerEvent {
			continue
		}
		eventCount++
		if event.RunID != f.work.ID || event.Scope != f.task.Scope || event.CorrelationID != answer.ID || event.CausationID != question.ID ||
			event.Actor != (ActivityActor{Type: "user", ID: f.task.AuthenticatedActor.ID}) {
			t.Fatalf("answer event lost execution authority: %#v", event)
		}
		for key, want := range map[string]string{
			"conversationId": f.conversation.ID, "questionMessageId": question.ID, "answerMessageId": answer.ID,
			"conversationTaskId": f.task.ID, "sourceRunId": f.source.ID, "sourceMessageId": f.trigger.ID,
			"threadRootMessageId": f.task.ThreadRootID, "authenticatedActorId": f.task.AuthenticatedActor.ID, "agentId": f.task.TargetAgentID,
		} {
			if event.Payload[key] != want {
				t.Fatalf("answer event %s = %#v, want %q", key, event.Payload[key], want)
			}
		}
	}
	if eventCount != count {
		t.Fatalf("answer transition count = %d, want %d", eventCount, count)
	}
	messages, err := f.store.ListChannelMessages(t.Context(), ChannelMessageFilter{Scope: f.task.Scope, ConversationID: f.conversation.ID, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	questionCount, acknowledgmentCount := 0, 0
	for _, message := range messages {
		if message.IdempotencyKey == question.IdempotencyKey {
			questionCount++
		}
		if message.IdempotencyKey != "conversation-answer-received:"+answer.ID {
			continue
		}
		acknowledgmentCount++
		if message.Intent != MessageIntentAcknowledgment || message.ReplyToMessageID != answer.ID || message.ResolvesMessageID != answer.ID {
			t.Fatalf("answer acknowledgment lost its target: %#v", message)
		}
	}
	if questionCount != 1 || acknowledgmentCount != count {
		t.Fatalf("question/acknowledgment count = %d/%d, want 1/%d", questionCount, acknowledgmentCount, count)
	}
	f.assertSingleForeground(t)
}

func TestConversationTaskClarificationRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name     string
		threaded bool
		status   AgentRunStatus
	}{
		{name: "implicit answer", status: AgentRunStatusQueued},
		{name: "explicit answer after completion", threaded: true, status: AgentRunStatusCompleted},
		{name: "explicit answer after cancellation", threaded: true, status: AgentRunStatusCanceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			forConversationTaskClarificationStores(t, func(t *testing.T, store conversationTaskClarificationStore) {
				f := newConversationTaskClarificationFixture(t, store, tc.threaded)
				question := f.question(t)
				f.resetScheduler(t)
				if again := f.question(t); again.ID != question.ID {
					t.Fatal("reconciliation duplicated the clarification question")
				}
				request := PostChannelMessageRequest{
					Sender: f.task.AuthenticatedActor, Intent: MessageIntentAnswer, RequiresResponse: true,
					Content: "Adult general readers. Keep the introduction within 1000 characters.", IdempotencyKey: "clarification-answer",
				}
				if tc.threaded {
					request.ReplyToMessageID, request.ResolvesMessageID = question.ID, question.ID
				}
				answer := f.post(t, request)
				resumed, replayed, err := f.scheduler.ScheduleMessage(t.Context(), f.task.Scope, f.conversation.ID, answer.ID)
				if err != nil || replayed || resumed == nil || resumed.Run == nil || resumed.Run.ID != f.work.ID ||
					resumed.Run.Status != AgentRunStatusQueued || resumed.Run.WakeCondition != nil || !ConversationTaskMatchesWorkRun(f.task, resumed.Run) {
					t.Fatalf("answer did not queue the original independent task: %#v, replay=%v, err=%v", resumed, replayed, err)
				}
				f.work = resumed.Run
				f.assertAnswerReceipt(t, question, answer, 1)
				cloned := cloneAgentRun(resumed.Run)
				cloned.PendingInterventions[0].ConversationAnswer.AnswerContent = "altered clone"
				if resumed.Run.PendingInterventions[0].ConversationAnswer.AnswerContent != answer.Content ||
					f.readWork(t).PendingInterventions[0].ConversationAnswer.AnswerContent != answer.Content {
					t.Fatal("cloning a run aliased its trusted answer receipt")
				}
				if tc.status == AgentRunStatusCompleted {
					f.transition(t, RunTransitionRequest{Status: AgentRunStatusRunning})
					f.transition(t, RunTransitionRequest{Status: AgentRunStatusCompleted, Output: map[string]interface{}{"summary": "A black hole is a region whose gravity prevents light from escaping."}})
				} else if tc.status == AgentRunStatusCanceled {
					f.cancel(t)
				}
				before := f.readWork(t)
				f.resetScheduler(t)
				for range 2 {
					again, replayed, err := f.scheduler.ScheduleMessage(t.Context(), f.task.Scope, f.conversation.ID, answer.ID)
					if err != nil || !replayed || again == nil || again.Run == nil || again.Run.ID != f.work.ID || again.Run.Status != tc.status {
						t.Fatalf("answer replay lost accepted task identity: %#v, replay=%v, err=%v", again, replayed, err)
					}
				}
				if _, err := f.scheduler.ReconcileScope(t.Context(), f.task.Scope); err != nil {
					t.Fatal(err)
				}
				f.assertWorkUnchanged(t, before)
				f.assertAnswerReceipt(t, question, answer, 1)
			})
		})
	}
}

func (f *conversationTaskClarificationFixture) cancel(t *testing.T) {
	t.Helper()
	result, err := NewConversationTaskService(f.store).Cancel(t.Context(), CancelConversationTaskRequest{
		GetConversationTaskRequest: GetConversationTaskRequest{
			Scope: f.task.Scope, Owner: f.task.Owner, ConversationID: f.task.ConversationID, ThreadRootID: f.task.ThreadRootID,
			TaskID: f.task.ID, AuthenticatedActor: f.task.AuthenticatedActor,
		},
		Actor: f.task.AuthenticatedActor, ExpectedWorkRunRevision: f.work.Revision, Summary: "Cancel the writing task",
	})
	if err != nil || result == nil || result.WorkRun == nil || result.WorkRun.Status != AgentRunStatusCanceled {
		t.Fatalf("cancel task: %#v, %v", result, err)
	}
	f.work = result.WorkRun
}

func (f *conversationTaskClarificationFixture) readWork(t *testing.T) *AgentRun {
	t.Helper()
	run, err := f.store.GetAgentRun(t.Context(), f.task.Scope, f.work.ID)
	if err != nil || run == nil {
		t.Fatalf("read canonical task work: %#v, %v", run, err)
	}
	return run
}

func (f *conversationTaskClarificationFixture) assertWorkUnchanged(t *testing.T, before *AgentRun) {
	t.Helper()
	// SQL and memory clones may normalize JSON numbers differently.
	want, err := json.Marshal(before)
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(f.readWork(t))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("answer changed task work: before=%s, after=%s", want, got)
	}
}

func TestConversationTaskClarificationRejectsExplicitAnswersOutsideAuthority(t *testing.T) {
	for _, reason := range []string{"other actor", "other thread", "canceled task"} {
		t.Run(reason, func(t *testing.T) {
			forConversationTaskClarificationStores(t, func(t *testing.T, store conversationTaskClarificationStore) {
				f := newConversationTaskClarificationFixture(t, store, true)
				question := f.question(t)
				request := PostChannelMessageRequest{
					Sender: f.task.AuthenticatedActor, Intent: MessageIntentAnswer, RequiresResponse: true,
					ReplyToMessageID: question.ID, ResolvesMessageID: question.ID,
					Content: "Use the finance team as the audience.", IdempotencyKey: "rejected-answer",
				}
				switch reason {
				case "other actor":
					request.Sender.ID = "other-member"
				case "other thread":
					other := f.post(t, PostChannelMessageRequest{
						Sender: f.task.AuthenticatedActor, Intent: MessageIntentUpdate, Content: "Another discussion", StartThread: true, IdempotencyKey: "other-thread",
					})
					request.ReplyToMessageID = other.ID
				case "canceled task":
					f.cancel(t)
				}
				answer := f.post(t, request)
				before := f.readWork(t)
				for range 2 {
					f.resetScheduler(t)
					result, _, err := f.scheduler.ScheduleMessage(t.Context(), f.task.Scope, f.conversation.ID, answer.ID)
					if result != nil {
						t.Fatalf("explicit %s answer scheduled work: %#v, %v", reason, result, err)
					}
					f.assertWorkUnchanged(t, before)
					f.assertAnswerReceipt(t, question, answer, 0)
				}
			})
		})
	}
}

func TestConversationTaskClarificationOriginRequiresCanonicalProvenance(t *testing.T) {
	forConversationTaskClarificationStores(t, func(t *testing.T, store conversationTaskClarificationStore) {
		f := newConversationTaskClarificationFixture(t, store, false)
		source, conversation, trigger, err := conversationWorkOrigin(t.Context(), store, store, f.work)
		if err != nil || source == nil || source.ID != f.source.ID || source.Status != AgentRunStatusCompleted ||
			conversation == nil || conversation.ID != f.conversation.ID || trigger == nil || trigger.ID != f.trigger.ID {
			t.Fatalf("canonical independent task lost its completed source: %#v, %#v, %#v, %v", source, conversation, trigger, err)
		}
		for _, tc := range []struct {
			name   string
			mutate func(*AgentRun)
		}{
			{name: "foreign scope", mutate: func(run *AgentRun) { run.Scope.ID = "another-organization" }},
			{name: "foreign assigned agent", mutate: func(run *AgentRun) { run.AssignedAgentID = "another-agent" }},
			{name: "foreign owner", mutate: func(run *AgentRun) { run.Owner.ID = "another-agent" }},
			{name: "forged task marker", mutate: func(run *AgentRun) { run.Context[ConversationTaskContextKey] = "fabricated-task" }},
			{name: "missing task marker", mutate: func(run *AgentRun) { delete(run.Context, ConversationTaskContextKey) }},
			{name: "copied task marker", mutate: func(run *AgentRun) { run.ID, run.RootRunID = "unrelated-work", "unrelated-work" }},
			{name: "forged conversation", mutate: func(run *AgentRun) { run.Context[conversationRunContextConversationID] = "another-conversation" }},
			{name: "forged source message", mutate: func(run *AgentRun) { run.Context[conversationRunContextTriggerID] = "another-message" }},
		} {
			t.Run(tc.name, func(t *testing.T) {
				forged := cloneAgentRun(f.work)
				tc.mutate(forged)
				for _, replay := range []bool{false, true} {
					source, conversation, trigger, err := conversationWorkOriginForState(t.Context(), store, store, forged, replay)
					if source != nil || conversation != nil || trigger != nil {
						t.Fatalf("forged provenance resolved an origin (replay=%v): %#v, %#v, %#v, %v", replay, source, conversation, trigger, err)
					}
				}
			})
		}
		f.assertSingleForeground(t)
	})
}

func TestConversationTaskClarificationReplayDoesNotAnswerLaterQuestion(t *testing.T) {
	forConversationTaskClarificationStores(t, func(t *testing.T, store conversationTaskClarificationStore) {
		f := newConversationTaskClarificationFixture(t, store, true)
		firstQuestion := f.question(t)
		firstAnswer := f.post(t, PostChannelMessageRequest{
			Sender: f.task.AuthenticatedActor, Intent: MessageIntentAnswer, RequiresResponse: true,
			ReplyToMessageID: firstQuestion.ID, ResolvesMessageID: firstQuestion.ID,
			Content: "Adult general readers.", IdempotencyKey: "first-answer",
		})
		accepted, replayed, err := f.scheduler.ScheduleMessage(t.Context(), f.task.Scope, f.conversation.ID, firstAnswer.ID)
		if err != nil || replayed || accepted == nil || accepted.Run == nil || accepted.Run.ID != f.work.ID || accepted.Run.Status != AgentRunStatusQueued {
			t.Fatalf("accept first answer: %#v, %v, %v", accepted, replayed, err)
		}
		f.work = accepted.Run
		f.assertAnswerReceipt(t, firstQuestion, firstAnswer, 1)
		firstReceipt := *f.readWork(t).PendingInterventions[0].ConversationAnswer
		f.ask(t, "Should the introduction include a numerical example?")
		secondQuestion := f.question(t)
		if secondQuestion.ID == firstQuestion.ID || f.questionTurn.ID == firstReceipt.QuestionTurnID || f.questionTurn.Sequence != firstReceipt.QuestionTurnSequence+1 {
			t.Fatal("later clarification reused the first question's turn identity")
		}
		before := f.readWork(t)
		f.resetScheduler(t)
		old, replayed, err := f.scheduler.ScheduleMessage(t.Context(), f.task.Scope, f.conversation.ID, firstAnswer.ID)
		if err != nil || !replayed || old == nil || old.Run == nil || old.Run.ID != f.work.ID || old.Run.Status != AgentRunStatusWaitingForEvent {
			t.Fatalf("old answer resumed later clarification: %#v, %v, %v", old, replayed, err)
		}
		f.assertWorkUnchanged(t, before)
		secondAnswer := f.post(t, PostChannelMessageRequest{
			Sender: f.task.AuthenticatedActor, Intent: MessageIntentAnswer, RequiresResponse: true,
			ReplyToMessageID: secondQuestion.ID, ResolvesMessageID: secondQuestion.ID,
			Content: "Yes, include one short numerical example.", IdempotencyKey: "second-answer",
		})
		accepted, replayed, err = f.scheduler.ScheduleMessage(t.Context(), f.task.Scope, f.conversation.ID, secondAnswer.ID)
		if err != nil || replayed || accepted == nil || accepted.Run == nil || accepted.Run.ID != f.work.ID || accepted.Run.Status != AgentRunStatusQueued {
			t.Fatalf("accept second answer: %#v, %v, %v", accepted, replayed, err)
		}
		current := f.readWork(t)
		if len(current.PendingInterventions) != 2 || !reflect.DeepEqual(current.PendingInterventions[0].ConversationAnswer, &firstReceipt) {
			t.Fatalf("later answer replaced the first durable receipt: %#v", current.PendingInterventions)
		}
		want := &ConversationAnswerReceipt{
			Scope: f.task.Scope, RunID: f.work.ID, AgentID: f.task.TargetAgentID, ConversationTaskID: f.task.ID,
			ConversationID: f.conversation.ID, SourceRunID: f.source.ID, SourceMessageID: f.trigger.ID,
			ThreadRootMessageID: f.task.ThreadRootID, QuestionMessageID: secondQuestion.ID, QuestionContent: secondQuestion.Content,
			QuestionTurnID: f.questionTurn.ID, QuestionTurnSequence: f.questionTurn.Sequence,
			AnswerMessageID: secondAnswer.ID, AnswerContent: secondAnswer.Content, AuthenticatedActor: f.task.AuthenticatedActor,
		}
		if !reflect.DeepEqual(current.PendingInterventions[1].ConversationAnswer, want) {
			t.Fatalf("later answer not bound to its own question: got %#v, want %#v", current.PendingInterventions[1].ConversationAnswer, want)
		}
		for _, answer := range []*ChannelMessage{firstAnswer, secondAnswer} {
			ack, err := f.store.FindChannelMessageByIdempotencyKey(t.Context(), f.task.Scope, f.conversation.ID, "conversation-answer-received:"+answer.ID)
			if err != nil || ack == nil || ack.ResolvesMessageID != answer.ID {
				t.Fatalf("missing answer acknowledgment: %#v, %v", ack, err)
			}
		}
		f.assertSingleForeground(t)
	})
}

func TestConversationTaskGenericInterventionDoesNotMintAnswerReceipt(t *testing.T) {
	forConversationTaskClarificationStores(t, func(t *testing.T, store conversationTaskClarificationStore) {
		f := newConversationTaskClarificationFixture(t, store, true)
		result, err := NewRunCommandService(store).CommandAgentRun(t.Context(), AgentRunCommandRequest{
			Scope: f.task.Scope, RunID: f.work.ID, ExpectedRevision: f.work.Revision, Kind: AgentRunCommandIntervene,
			Actor: ActivityActor{Type: "user", ID: f.task.AuthenticatedActor.ID}, InterventionID: stableConversationID(f.task.Scope, "generic-guidance", "intervention"),
			Instruction: "Answer to your clarification:\nYes, proceed.",
		})
		if err != nil || result == nil || result.Run == nil {
			t.Fatalf("generic intervention: %#v, %v", result, err)
		}
		current := f.readWork(t)
		if len(current.PendingInterventions) != 1 || current.PendingInterventions[0].ConversationAnswer != nil {
			t.Fatalf("generic command minted clarification authority: %#v", current.PendingInterventions)
		}
		events, err := store.ListActivity(t.Context(), ActivityFilter{Scope: f.task.Scope, RunID: f.work.ID, Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			if event.EventType == conversationAnswerEvent {
				t.Fatalf("generic command minted an answer transition: %#v", event)
			}
		}
		f.assertSingleForeground(t)
	})
}
