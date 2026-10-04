package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type taskFailureReportingStore interface {
	KernelStore
	ConversationTaskKernelStore
	terminalReportingContractStore
}

func admittedFailureReportingTask(t *testing.T, store taskFailureReportingStore, threaded bool) (*ConversationTaskResult, *ChannelMessage) {
	t.Helper()
	scope := Scope{Kind: "tenant", ID: "safe-task-failure"}
	service := NewConversationService(store)
	conversation, _, err := service.CreateConversation(t.Context(), CreateConversationRequest{Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, Title: "Private task", IdempotencyKey: "failure-thread"})
	if err != nil {
		t.Fatal(err)
	}
	actor := ConversationParticipant{Type: ConversationParticipantUser, ID: "requester"}
	target := ConversationParticipant{Type: ConversationParticipantAgent, ID: "agent"}
	audience := ConversationAudience{Kind: ConversationAudienceParticipants, Participants: []ConversationParticipant{target}}
	root := ""
	if threaded {
		parent, err := service.PostChannelMessage(t.Context(), PostChannelMessageRequest{Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision, Sender: actor, Intent: MessageIntentQuestion, Content: "Private thread", Audience: audience, IdempotencyKey: "thread"})
		if err != nil {
			t.Fatal(err)
		}
		conversation, root = parent.Conversation, parent.Message.ID
		audience = ConversationAudience{Kind: ConversationAudienceChannel}
	}
	posted, err := service.PostChannelMessage(t.Context(), PostChannelMessageRequest{Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision, Sender: actor, Intent: MessageIntentQuestion, Content: "Prepare the requested result", Audience: audience, ReplyToMessageID: root, RequiresResponse: true, IdempotencyKey: "work"})
	if err != nil {
		t.Fatal(err)
	}
	scheduler, err := NewConversationRunScheduler(store, store, ConversationRunSchedulerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := scheduler.ScheduleMessage(t.Context(), scope, conversation.ID, posted.Message.ID); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimNextAgentRun(t.Context(), AgentRunClaim{Scope: scope, Kind: RunKindConversation, WorkerID: "worker", Now: time.Now(), LeaseDuration: time.Minute, AgingInterval: time.Minute})
	if err != nil || claimed == nil {
		t.Fatalf("source claim: %#v %v", claimed, err)
	}
	proposal := &TurnTaskProposal{TaskKey: "private-work", Goal: "GOAL_SECRET prepare the complete result", Acknowledgment: "I will prepare the result."}
	settled, err := NewTurnCoordinator(store, store, store).Advance(t.Context(), AdvanceAgentRunRequest{Scope: scope, RunID: claimed.ID, WorkerID: "worker"}, TurnRunnerFunc(func(context.Context, TurnExecutionContext) (*TurnOutcome, error) {
		return &TurnOutcome{NextRunStatus: AgentRunStatusRunning, ProposedTask: proposal}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := NewConversationTaskService(store).Start(t.Context(), taskStartRequest(settled.Run, settled.Turn))
	if err != nil {
		t.Fatal(err)
	}
	return accepted, posted.Message
}

func TestConversationTaskFailureReportingSafeOnceAcrossRestartAndThread(t *testing.T) {
	for _, outcome := range []string{"review rejected", "unknown failure", "completed"} {
		for _, threaded := range []bool{false, true} {
			t.Run(outcome+map[bool]string{false: "/direct", true: "/thread"}[threaded], func(t *testing.T) {
				eventWaitContractFixtures(t, func(t *testing.T, fixture eventWaitContractFixture) {
					store := fixture.store.(taskFailureReportingStore)
					accepted, trigger := admittedFailureReportingTask(t, store, threaded)
					activity := NewRunActivityService(store, store)
					running, _, err := activity.TransitionRun(t.Context(), accepted.WorkRun.Scope, accepted.WorkRun.ID, RunTransitionRequest{ExpectedRevision: accepted.WorkRun.Revision, Status: AgentRunStatusRunning})
					if err != nil {
						t.Fatal(err)
					}
					status, checkpoint, expected := AgentRunStatusFailed, rejectedTaskReviewCheckpoint(), TerminalFailureReply("task_completion_rejected")
					if outcome == "unknown failure" {
						checkpoint["_atlasTaskCompletionReview"].(map[string]interface{})["failureCode"] = "VERDICT_SECRET"
						expected = TerminalFailureReply("execution_failed")
					} else if outcome == "completed" {
						status, expected = AgentRunStatusCompleted, "Completed fixture result."
					}
					output := map[string]interface{}{"summary": "OUTPUT_SECRET"}
					if status == AgentRunStatusCompleted {
						output["summary"] = expected
					}
					work, _, err := activity.TransitionRun(t.Context(), running.Scope, running.ID, RunTransitionRequest{ExpectedRevision: running.Revision, Status: status, Checkpoint: checkpoint, Output: output, Error: "ERROR_SECRET provider argument and private response"})
					if err != nil {
						t.Fatal(err)
					}
					if fixture.reopen != nil {
						fixture.store = fixture.reopen()
						store = fixture.store.(taskFailureReportingStore)
					}
					// Deliver work first, then replay through the actual durable outbox.
					// The original source acknowledgment must still be published first.
					if err := projectTerminalRunReporting(t.Context(), store, work); err != nil {
						t.Fatal(err)
					}
					worker := terminalReportingContractWorker(t, store, time.Now().Add(time.Second))
					if _, err := worker.ProcessBatch(t.Context()); err != nil {
						t.Fatal(err)
					}
					for range 2 {
						if err := projectTerminalRunReporting(t.Context(), store, work); err != nil {
							t.Fatal(err)
						}
						if err := projectTerminalRunReporting(t.Context(), store, accepted.SourceRun); err != nil {
							t.Fatal(err)
						}
					}
					if n, err := worker.ProcessBatch(t.Context()); err != nil || n != 0 {
						t.Fatalf("delivered outbox replayed: %d %v", n, err)
					}
					messages, err := store.ListChannelMessages(t.Context(), ChannelMessageFilter{Scope: work.Scope, ConversationID: accepted.Task.ConversationID, Limit: 100})
					if err != nil {
						t.Fatal(err)
					}
					answers := []*ChannelMessage{}
					for _, message := range messages {
						if message.Intent == MessageIntentAnswer {
							answers = append(answers, message)
						}
					}
					if len(answers) != 2 || answers[0].Content != accepted.Task.Acknowledgment || answers[1].Content != expected {
						t.Fatalf("wrong acknowledgment/final sequence: %#v", answers)
					}
					service := NewConversationService(store)
					for _, message := range answers {
						if message.Scope != work.Scope || message.ReplyToMessageID != trigger.ID || message.ThreadRootID != accepted.Task.ThreadRootID || message.BroadcastToChannel == threaded {
							t.Fatalf("failure widened origin: %#v", message)
						}
						for _, secret := range []string{"GOAL_SECRET", "ERROR_SECRET", "VERDICT_SECRET", "CANDIDATE_SECRET", "OUTPUT_SECRET"} {
							if strings.Contains(message.Content, secret) {
								t.Fatalf("private failure data published: %q", message.Content)
							}
						}
						if _, err := service.GetVisibleChannelMessage(t.Context(), work.Scope, accepted.Task.ConversationID, message.ID, ConversationViewer{Participant: trigger.Sender}); err != nil {
							t.Fatalf("source actor lost access: %v", err)
						}
						if _, err := service.GetVisibleChannelMessage(t.Context(), work.Scope, accepted.Task.ConversationID, message.ID, ConversationViewer{Participant: ConversationParticipant{Type: ConversationParticipantUser, ID: "stranger"}}); !errors.Is(err, ErrChannelMessageNotFound) {
							t.Fatalf("private thread widened: %v", err)
						}
					}
					for _, mutate := range []func(*AgentRun){func(r *AgentRun) { r.Scope.ID = "foreign" }, func(r *AgentRun) { r.Owner.ID = "foreign" }, func(r *AgentRun) { r.Context[conversationRunContextTriggerID] = "foreign" }} {
						forged := cloneAgentRun(work)
						mutate(forged)
						if err := projectTerminalRunReporting(t.Context(), store, forged); !errors.Is(err, ErrInvalidConversationTask) {
							t.Fatalf("forged origin accepted: %v", err)
						}
					}
					intent, err := store.GetRunTerminalReport(t.Context(), work.Scope, work.ID, work.Status)
					if err != nil || intent.DeliveredAt == nil {
						t.Fatalf("failure outbox not settled: %#v %v", intent, err)
					}
					canonical, err := store.GetAgentRun(t.Context(), work.Scope, work.ID)
					if err != nil || canonical.Status != work.Status || canonical.Revision != work.Revision {
						t.Fatalf("reporting changed execution: %#v %v", canonical, err)
					}
				})
			})
		}
	}
}

func TestConversationTaskContinuationFailureUsesSanitizedReply(t *testing.T) {
	store, result, _, _ := promotedContinuationAuthorityFixture(t, "safe-continuation-failure")
	run := finishContinuationAuthorityRun(t, store, result.WorkRun, AgentRunStatusFailed)
	for range 2 {
		if err := projectTerminalRunReporting(t.Context(), store, run); err != nil {
			t.Fatal(err)
		}
	}
	message, err := FindConversationTaskResultMessage(t.Context(), store, result.Task, run)
	if err != nil || message == nil || message.Content != TerminalFailureReply("execution_failed") {
		t.Fatalf("legacy failure lost safe reply: %#v %v", message, err)
	}
}
