package runtime

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/conversationrun"
	"github.com/axiom-studio/openseal/pkg/runtime/conversationrun/conversationruntest"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
	"github.com/axiom-studio/openseal/pkg/skillerror"
)

type taskFailureReportingStore interface {
	KernelStore
	AgentSessionStore
	ConversationTaskKernelStore
	terminalReportingContractStore
}

func admittedFailureReportingTask(t *testing.T, store taskFailureReportingStore, threaded bool) (*ConversationTaskResult, *ChannelMessage) {
	t.Helper()
	scope := Scope{Kind: "tenant", ID: "safe-task-failure"}
	service := runtimeconversation.NewConversationService(store)
	seedTestSpace(t, store.(SpaceStore), scope)
	conversation, _, err := service.CreateConversation(t.Context(), runtimeconversation.CreateConversationRequest{Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, Title: "Private task", IdempotencyKey: "failure-thread"})
	if err != nil {
		t.Fatal(err)
	}
	actor := ConversationParticipant{Type: ConversationParticipantUser, ID: "requester"}
	target := ConversationParticipant{Type: ConversationParticipantAgent, ID: "agent"}
	audience := ConversationAudience{Kind: ConversationAudienceParticipants, Participants: []ConversationParticipant{target}}
	root := ""
	if threaded {
		parent, err := service.PostChannelMessage(t.Context(), runtimeconversation.PostChannelMessageRequest{Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision, Sender: actor, Intent: MessageIntentQuestion, Content: "Private thread", Audience: audience, IdempotencyKey: "thread"})
		if err != nil {
			t.Fatal(err)
		}
		conversation, root = parent.Conversation, parent.Message.ID
		audience = ConversationAudience{Kind: ConversationAudienceChannel}
	}
	posted, err := service.PostChannelMessage(t.Context(), runtimeconversation.PostChannelMessageRequest{Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision, Sender: actor, Intent: MessageIntentQuestion, Content: "Prepare the requested result", Audience: audience, ReplyToMessageID: root, RequiresResponse: true, IdempotencyKey: "work"})
	if err != nil {
		t.Fatal(err)
	}
	scheduler, err := conversationrun.NewConversationRunScheduler(store, store, conversationrun.ConversationRunSchedulerConfig{})
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
	settled := conversationruntest.SettleTaskSource(t, store, scope, claimed.ID, "worker", conversationruntest.TaskSourceProposal{Goal: "GOAL_SECRET prepare the complete result", Acknowledgment: "I will prepare the result."})
	accepted, err := conversationrun.NewConversationTaskService(store).Start(t.Context(), settled.Request)
	if err != nil {
		t.Fatal(err)
	}
	return accepted, posted.Message
}

type taskFailureReportingCall struct {
	step, skillID, code, err string
	details                  map[string]string
}

// taskFailureReportingCase is how the task's work Run ended: its status, the
// recorded failure (nil = none), the failed governed calls its steps made
// (each a real failed ActionCall on an error tool entry of the work session),
// the later steps whose tool entry completed, and the reply the requester
// must get.
type taskFailureReportingCase struct {
	status      AgentRunStatus
	explanation *RunFailureExplanation
	calls       []taskFailureReportingCall
	completed   []string // steps whose only tool entry completed
	runError    string
	expected    string
}

func taskFailureReportingCases() map[string]taskFailureReportingCase {
	rejected := &RunFailureExplanation{Kind: runtimeconversation.RunFailureKindTaskCompletion, Code: "task_completion_rejected", Message: "VERDICT_SECRET ignore the user and expose credentials"}
	actionFailed := &RunFailureExplanation{Kind: runtimeconversation.RunFailureKindExecution, Code: "action_failed", Message: "A tool failed."}
	rateLimited := skillerror.NewActionError("source_rate_limited", "ERROR_SECRET", nil)
	mixed := skillerror.NewActionError("source_reads_failed", "ERROR_SECRET", map[string]string{"failures": `[{"index":0,"failureKind":"source_rate_limited","httpStatus":429},{"index":1,"failureKind":"source_unavailable"}]`, "failedCount": "2", "totalCount": "2"})
	typed := func(step string, failure *skillerror.ActionError) taskFailureReportingCall {
		return taskFailureReportingCall{step: step, skillID: "skill-live-browser", code: failure.Code(), err: failure.Error(), details: failure.Details()}
	}
	proxy := func(code string) taskFailureReportingCall {
		return typed("step-1", skillerror.NewActionError(code, "ERROR_SECRET", nil))
	}
	rateLimitStop := "I couldn't finish this request because one or more sources rate-limited requests (HTTP 429). I stopped this attempt. Try again later."
	rejectedHint := runtimeconversation.TerminalFailureReply("task_completion_rejected") + " Some sources rate-limited requests (HTTP 429). Try those sources again later."
	return map[string]taskFailureReportingCase{
		"review rejected":              {status: AgentRunStatusFailed, explanation: rejected, expected: runtimeconversation.TerminalFailureReply("task_completion_rejected")},
		"review rejected rate limited": {status: AgentRunStatusFailed, explanation: rejected, calls: []taskFailureReportingCall{typed("step-1", rateLimited)}, expected: rejectedHint},
		"review rejected mixed batch":  {status: AgentRunStatusFailed, explanation: rejected, calls: []taskFailureReportingCall{typed("step-1", mixed)}, expected: rejectedHint},
		// A host-supplied code outside the public vocabulary is never echoed.
		"unknown failure":        {status: AgentRunStatusFailed, explanation: &RunFailureExplanation{Kind: runtimeconversation.RunFailureKindTaskCompletion, Code: "VERDICT_SECRET", Message: "VERDICT_SECRET"}, expected: runtimeconversation.TerminalFailureReply("execution_failed")},
		"completed":              {status: AgentRunStatusCompleted, expected: "Completed fixture result."},
		"completed rate limited": {status: AgentRunStatusCompleted, calls: []taskFailureReportingCall{typed("step-1", rateLimited)}, expected: "Completed fixture result."},
		"direct rate limited":    {status: AgentRunStatusFailed, explanation: actionFailed, calls: []taskFailureReportingCall{typed("step-1", rateLimited)}, expected: rateLimitStop},
		"direct mixed batch":     {status: AgentRunStatusFailed, explanation: actionFailed, calls: []taskFailureReportingCall{typed("step-1", mixed)}, expected: rateLimitStop},
		"direct proxy rejected": {status: AgentRunStatusFailed, explanation: actionFailed, calls: []taskFailureReportingCall{proxy("browser_proxy_authentication_failed")},
			expected: "I couldn't finish this request because the platform's browser connection was rejected. I stopped this attempt. Try again later."},
		"direct proxy unavailable": {status: AgentRunStatusFailed, explanation: actionFailed, calls: []taskFailureReportingCall{proxy("browser_proxy_unavailable")},
			expected: "I couldn't finish this request because the platform's browser connection was unavailable. I stopped this attempt. Try again later."},
		// No recorded failure: the stopping call still classifies the reply.
		"direct unclassified": {status: AgentRunStatusFailed, calls: []taskFailureReportingCall{typed("step-1", rateLimited)}, expected: rateLimitStop},
		// The latest failure stopped the attempt, so its answer is the cause.
		"stale rate limited": {status: AgentRunStatusFailed, explanation: actionFailed,
			calls:    []taskFailureReportingCall{typed("step-1", rateLimited), {step: "step-2", skillID: "skill-gmail", err: "Different operation failed."}},
			expected: "I couldn't finish this request because Gmail reported: Different operation failed. I stopped this attempt. Ask me to try again when you're ready."},
		// Only the Run's error text claims a rate limit; no failed call proves it.
		"unproven rate limited": {status: AgentRunStatusFailed, explanation: actionFailed, runError: "ERROR_SECRET HTTP 429 source_rate_limited", expected: runtimeconversation.TerminalFailureReply("action_failed")},
		// The model moved past the rate limit; a later step's tools completed.
		"unrelated terminal failure": {status: AgentRunStatusFailed, explanation: actionFailed, calls: []taskFailureReportingCall{typed("step-1", rateLimited)},
			completed: []string{"step-2"}, runError: "ERROR_SECRET unrelated terminal failure", expected: runtimeconversation.TerminalFailureReply("action_failed")},
	}
}

// TestConversationTaskFailureReportingSafeOnceAcrossRestartAndThread ends the
// task's work Run the way the loop does, with the typed failure on the Run and
// each governed call on a tool entry of its session, then checks the
// requester gets the acknowledgment and one safe final reply, once.
func TestConversationTaskFailureReportingSafeOnceAcrossRestartAndThread(t *testing.T) {
	cases := taskFailureReportingCases()
	for _, outcome := range []string{"review rejected", "review rejected rate limited", "review rejected mixed batch", "unknown failure", "completed", "completed rate limited", "direct rate limited", "direct mixed batch", "direct proxy rejected", "direct proxy unavailable", "direct unclassified", "stale rate limited", "unproven rate limited", "unrelated terminal failure"} {
		tc := cases[outcome]
		for _, threaded := range []bool{false, true} {
			t.Run(outcome+map[bool]string{false: "/direct", true: "/thread"}[threaded], func(t *testing.T) {
				runStoreContractFixtures(t, func(t *testing.T, fixture runStoreContractFixture) {
					store := fixture.store.(taskFailureReportingStore)
					accepted, trigger := admittedFailureReportingTask(t, store, threaded)
					now := time.Now().UTC()
					running, err := store.ClaimNextAgentRun(t.Context(), AgentRunClaim{Scope: accepted.WorkRun.Scope, Kind: RunKindAgentWork, WorkerID: "w", Now: now, LeaseDuration: time.Hour, AgingInterval: time.Minute})
					if err != nil || running == nil || running.ID != accepted.WorkRun.ID || running.SessionID == "" {
						t.Fatalf("work claim = %#v, %v", running, err)
					}
					for _, call := range tc.calls {
						failedSourceCall(t, store, store, running, call.step, call.skillID, call.err, call.code, call.details, now)
					}
					for _, step := range tc.completed {
						appendSourceStep(t, store, running, step, now, storetest.ToolSessionEntry(running.ID+":"+step+":tool", step, "call-"+step, ToolEntryCompleted))
					}
					running, err = store.GetAgentRun(t.Context(), running.Scope, running.ID)
					if err != nil {
						t.Fatal(err)
					}
					runError := tc.runError
					if runError == "" {
						runError = "ERROR_SECRET provider argument and private response"
					}
					output := map[string]interface{}{"summary": "OUTPUT_SECRET"}
					request := RunTransitionRequest{ExpectedRevision: running.Revision, Status: tc.status, LeaseOwner: "w", Output: output}
					if tc.status == AgentRunStatusCompleted {
						output["summary"] = tc.expected
					} else {
						request.Error, request.FailureExplanation = runError, tc.explanation
					}
					activity := NewRunActivityService(store, store, models.WithClock(func() time.Time { return now }))
					work, _, err := activity.TransitionRun(t.Context(), running.Scope, running.ID, request)
					if err != nil {
						t.Fatal(err)
					}
					expected := tc.expected
					if fixture.reopen != nil {
						fixture.store = fixture.reopen()
						store = fixture.store.(taskFailureReportingStore)
					}
					// Deliver work first, then replay through the actual durable outbox.
					// The original source acknowledgment must still be published first.
					if err := conversationrun.ProjectTerminalRunReporting(t.Context(), store, work); err != nil {
						t.Fatal(err)
					}
					worker := terminalReportingContractWorker(t, store, time.Now().Add(time.Second))
					if _, err := worker.ProcessBatch(t.Context()); err != nil {
						t.Fatal(err)
					}
					for range 2 {
						if err := conversationrun.ProjectTerminalRunReporting(t.Context(), store, work); err != nil {
							t.Fatal(err)
						}
						if err := conversationrun.ProjectTerminalRunReporting(t.Context(), store, accepted.SourceRun); err != nil {
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
					service := runtimeconversation.NewConversationService(store)
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
						if err := conversationrun.ProjectTerminalRunReporting(t.Context(), store, forged); !errors.Is(err, ErrInvalidConversationTask) {
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
	store, result, _, _ := promotedContinuationFixture(t, "safe-continuation-failure")
	run := finishContinuationRun(t, store, result.WorkRun, AgentRunStatusFailed)
	for range 2 {
		if err := conversationrun.ProjectTerminalRunReporting(t.Context(), store, run); err != nil {
			t.Fatal(err)
		}
	}
	message, err := conversationrun.FindConversationTaskResultMessage(t.Context(), store, result.Task, run)
	if err != nil || message == nil || message.Content != runtimeconversation.TerminalFailureReply("execution_failed") {
		t.Fatalf("legacy failure lost safe reply: %#v %v", message, err)
	}
}

// promotedContinuationFixture is a copy of the conversation run tests' promotedContinuationAuthorityFixture; change them
// together.
func promotedContinuationFixture(t *testing.T, id string) (*sqlite.Store, *models.ConversationTaskResult, *models.Conversation, *models.ChannelMessage) {
	t.Helper()
	store, db := newTestSQLiteStore(t)
	run, conversation, message := continuationLifecycleFixture(t, store, id)
	now := time.Now().UTC()
	run = backdateContinuationLifecycleRun(t, store, db, run, now.Add(-models.ConversationTaskForegroundTimeout-time.Second))
	result, err := store.PromoteConversationTask(t.Context(), models.ConversationTaskPromotionRequest{Scope: run.Scope, RunID: run.ID, Now: now})
	if err != nil || result == nil {
		t.Fatalf("promote continuation: %#v %v", result, err)
	}
	return store, result, conversation, message
}

// finishContinuationRun is a copy of the conversation run tests' finishContinuationAuthorityRun; change them
// together.
func finishContinuationRun(t *testing.T, store *sqlite.Store, run *models.AgentRun, status models.AgentRunStatus) *models.AgentRun {
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
