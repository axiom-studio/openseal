package runtime

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

type terminalReportingContractStore interface {
	RunCommandStore
	RunTerminalReportingStore
	ConversationStore
}

func terminalReportingContractFixture(t *testing.T, fixture eventWaitContractFixture) terminalReportingContractStore {
	t.Helper()
	store, ok := fixture.store.(terminalReportingContractStore)
	if !ok {
		t.Fatal("canonical persistent store lacks terminal reporting queue")
	}
	return store
}

func terminalReportingContractRun(t *testing.T, store terminalReportingContractStore, scope Scope, at time.Time) (*AgentRun, *Conversation, *ChannelMessage) {
	t.Helper()
	owner := ObjectiveOwner{Type: OwnerTypeAgent, ID: "report-agent"}
	conversations := NewConversationService(store)
	conversation, _, err := conversations.CreateConversation(t.Context(), CreateConversationRequest{Scope: scope, Owner: owner, Title: "Original request", IdempotencyKey: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	trigger, err := conversations.PostChannelMessage(t.Context(), PostChannelMessageRequest{
		Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
		Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "requesting-user"}, Intent: MessageIntentQuestion,
		Content: "Send me the result here when the work finishes", Audience: ConversationAudience{Kind: ConversationAudienceChannel}, IdempotencyKey: uuid.NewString(),
	})
	if err != nil {
		t.Fatal(err)
	}
	key := uuid.NewString()
	commands := NewRunCommandService(store)
	commands.now = func() time.Time { return at }
	created, err := commands.CreateAgentRun(t.Context(), CreateAgentRunRequest{
		Scope: scope, Owner: owner, AssignedAgentID: owner.ID, Goal: "Report the completed background work", Source: RunSourceSchedule, IdempotencyKey: key,
		Context: map[string]interface{}{
			conversationRunContextConversationID: conversation.ID, conversationRunContextTriggerID: trigger.Message.ID,
			runReportingContextRootRunID: runIDForIdempotencyKey(scope, key), runReportingContextMilestones: []string{"completed", "failed"}, scheduledTaskContextKey: true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return created.Run, trigger.Conversation, trigger.Message
}

func terminalReportingContractComplete(t *testing.T, store terminalReportingContractStore, run *AgentRun, at time.Time) *AgentRun {
	t.Helper()
	activity := NewRunActivityService(store, store)
	activity.now = func() time.Time { return at }
	running, _, err := activity.TransitionRun(t.Context(), run.Scope, run.ID, RunTransitionRequest{ExpectedRevision: run.Revision, Status: AgentRunStatusRunning})
	if err != nil {
		t.Fatal(err)
	}
	terminal, _, err := activity.TransitionRun(t.Context(), run.Scope, run.ID, RunTransitionRequest{
		ExpectedRevision: running.Revision, Status: AgentRunStatusCompleted, Output: map[string]interface{}{"summary": "The requested result is ready"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return terminal
}

func terminalReportingContractWorker(t *testing.T, store terminalReportingContractStore, at time.Time) *RunTerminalReportingWorker {
	t.Helper()
	worker, err := NewRunTerminalReportingWorker(store, RunTerminalReportingWorkerOptions{WorkerID: "report-worker", LeaseDuration: time.Minute, BatchSize: 10, Concurrency: 2})
	if err != nil {
		t.Fatal(err)
	}
	worker.now = func() time.Time { return at }
	return worker
}

func terminalReportingContractAssertMessage(t *testing.T, store ConversationStore, run *AgentRun, conversation *Conversation, trigger *ChannelMessage) {
	t.Helper()
	messages, err := store.ListChannelMessages(t.Context(), ChannelMessageFilter{Scope: run.Scope, ConversationID: conversation.ID, Limit: 100})
	if err != nil || len(messages) != 2 {
		t.Fatalf("terminal result lost or duplicated: messages=%#v error=%v", messages, err)
	}
	var report *ChannelMessage
	for _, message := range messages {
		if message.ID != trigger.ID {
			report = message
		}
	}
	if report == nil || report.Content != "The requested result is ready" || report.ReplyToMessageID != trigger.ID || len(report.References) != 1 || report.References[0].ID != run.ID {
		t.Fatalf("result missed the original chat/context: %#v", report)
	}
}

func TestTerminalRunReportingContractCommitCrashRestartRecovery(t *testing.T) {
	eventWaitContractFixtures(t, func(t *testing.T, fixture eventWaitContractFixture) {
		store := terminalReportingContractFixture(t, fixture)
		now := eventWaitContractEpoch
		run, conversation, trigger := terminalReportingContractRun(t, store, Scope{Kind: "tenant", ID: "report-restart"}, now)
		run = terminalReportingContractComplete(t, store, run, now)
		intent, err := store.GetRunTerminalReport(t.Context(), run.Scope, run.ID, run.Status)
		if err != nil || intent.Run == nil || intent.Run.Revision != run.Revision || intent.DeliveredAt != nil {
			t.Fatalf("terminal commit did not atomically save the promised report: %#v, %v", intent, err)
		}
		if fixture.reopen != nil {
			fixture.store = fixture.reopen()
			store = terminalReportingContractFixture(t, fixture)
		}
		worker := terminalReportingContractWorker(t, store, now)
		if delivered, err := worker.ProcessBatch(t.Context()); err != nil || delivered != 1 {
			t.Fatalf("restart did not deliver saved intent: delivered=%d error=%v", delivered, err)
		}
		terminalReportingContractAssertMessage(t, store, run, conversation, trigger)
		if delivered, err := worker.ProcessBatch(t.Context()); err != nil || delivered != 0 {
			t.Fatalf("acknowledged notification remained runnable: delivered=%d error=%v", delivered, err)
		}
		intent, err = store.GetRunTerminalReport(t.Context(), run.Scope, run.ID, run.Status)
		if err != nil || intent.DeliveredAt == nil || intent.Run != nil || intent.LeaseOwner != "" {
			t.Fatalf("delivery not acknowledged or duplicate snapshot retained: %#v, %v", intent, err)
		}
		canonical, err := store.GetAgentRun(t.Context(), run.Scope, run.ID)
		if err != nil || canonical.Revision != run.Revision || canonical.Status != run.Status {
			t.Fatal("reporting re-executed or mutated the completed workflow")
		}
	})
}

func TestTerminalRunReportingContractCrashAfterPostBeforeAck(t *testing.T) {
	eventWaitContractFixtures(t, func(t *testing.T, fixture eventWaitContractFixture) {
		store := terminalReportingContractFixture(t, fixture)
		now := eventWaitContractEpoch
		run, conversation, trigger := terminalReportingContractRun(t, store, Scope{Kind: "tenant", ID: "report-ack-crash"}, now)
		run = terminalReportingContractComplete(t, store, run, now)
		claims, err := store.ClaimRunTerminalReports(t.Context(), RunTerminalReportingClaim{WorkerID: "dead-worker", Now: now, LeaseDuration: time.Minute, Limit: 1})
		if err != nil || len(claims) != 1 {
			t.Fatalf("claim: %#v, %v", claims, err)
		}
		if err := projectTerminalRunReporting(t.Context(), store, claims[0].Run); err != nil {
			t.Fatal(err)
		}
		if fixture.reopen != nil {
			fixture.store = fixture.reopen()
			store = terminalReportingContractFixture(t, fixture)
		}
		now = now.Add(2 * time.Minute)
		if err := store.CompleteRunTerminalReport(t.Context(), RunTerminalReportingCompletion{
			Scope: run.Scope, RunID: run.ID, Status: run.Status, WorkerID: claims[0].LeaseOwner, LeaseExpiresAt: *claims[0].LeaseExpiresAt, Now: now,
		}); !errors.Is(err, ErrLeaseLost) {
			t.Fatalf("expired delivery attempt was not fenced: %v", err)
		}
		if delivered, err := terminalReportingContractWorker(t, store, now).ProcessBatch(t.Context()); err != nil || delivered != 1 {
			t.Fatalf("accepted result did not replay and acknowledge: delivered=%d error=%v", delivered, err)
		}
		terminalReportingContractAssertMessage(t, store, run, conversation, trigger)
		intent, err := store.GetRunTerminalReport(t.Context(), run.Scope, run.ID, run.Status)
		if err != nil || intent.DeliveredAt == nil || intent.Attempts != 2 {
			t.Fatalf("recovery acknowledgment was not durable: %#v, %v", intent, err)
		}
	})
}

type terminalReportingConflictStore struct {
	terminalReportingContractStore
	mu        sync.Mutex
	remaining int
}

func (s *terminalReportingConflictStore) CommitChannelMessage(ctx context.Context, record ChannelMessageCommitRecord) (*ChannelMessageCommitResult, error) {
	s.mu.Lock()
	if s.remaining > 0 {
		s.remaining--
		s.mu.Unlock()
		return nil, ErrRevisionConflict
	}
	s.mu.Unlock()
	return s.terminalReportingContractStore.CommitChannelMessage(ctx, record)
}

func TestTerminalRunReportingContractConflictBackoffAndActiveScopeGate(t *testing.T) {
	eventWaitContractFixtures(t, func(t *testing.T, fixture eventWaitContractFixture) {
		store := terminalReportingContractFixture(t, fixture)
		now := eventWaitContractEpoch
		run, conversation, trigger := terminalReportingContractRun(t, store, Scope{Kind: "tenant", ID: "report-retry"}, now)
		run = terminalReportingContractComplete(t, store, run, now)
		conflicted := &terminalReportingConflictStore{terminalReportingContractStore: store, remaining: 2}
		worker := terminalReportingContractWorker(t, conflicted, now)
		if delivered, err := worker.ProcessBatch(t.Context()); !errors.Is(err, ErrRevisionConflict) || delivered != 0 {
			t.Fatalf("exhausted revision retry should retain work: delivered=%d error=%v", delivered, err)
		}
		intent, err := store.GetRunTerminalReport(t.Context(), run.Scope, run.ID, run.Status)
		if err != nil || intent.DeliveredAt != nil || intent.LeaseOwner != "" || !intent.AvailableAt.After(now) || worker.Stats().Retried != 1 {
			t.Fatalf("projection failure was lost or hot-polled: %#v, %v", intent, err)
		}
		if delivered, err := worker.ProcessBatch(t.Context()); err != nil || delivered != 0 {
			t.Fatalf("backoff was ignored: delivered=%d error=%v", delivered, err)
		}
		now = intent.AvailableAt
		worker.now = func() time.Time { return now }
		worker.options.ScopeAllowed = func(_ context.Context, scope Scope) (bool, error) {
			if scope != run.Scope {
				t.Errorf("tenant gate checked wrong scope: %#v", scope)
			}
			return false, nil
		}
		if delivered, err := worker.ProcessBatch(t.Context()); err == nil || delivered != 0 {
			t.Fatalf("inactive tenant's result was delivered: delivered=%d error=%v", delivered, err)
		}
		intent, err = store.GetRunTerminalReport(t.Context(), run.Scope, run.ID, run.Status)
		if err != nil || intent.DeliveredAt != nil {
			t.Fatal("inactive tenant's pending report was discarded")
		}
		now = intent.AvailableAt
		worker.options.ScopeAllowed = func(context.Context, Scope) (bool, error) { return true, nil }
		if delivered, err := worker.ProcessBatch(t.Context()); err != nil || delivered != 1 {
			t.Fatalf("active tenant did not recover its report: delivered=%d error=%v", delivered, err)
		}
		terminalReportingContractAssertMessage(t, store, run, conversation, trigger)
	})
}

func TestTerminalRunReportingContractConcurrentFastpathAndDurableRetry(t *testing.T) {
	eventWaitContractFixtures(t, func(t *testing.T, fixture eventWaitContractFixture) {
		store := terminalReportingContractFixture(t, fixture)
		now := eventWaitContractEpoch
		run, conversation, trigger := terminalReportingContractRun(t, store, Scope{Kind: "tenant", ID: "report-concurrent"}, now)
		run = terminalReportingContractComplete(t, store, run, now)
		var group sync.WaitGroup
		failures := make(chan error, 6)
		for attempt := 0; attempt < 6; attempt++ {
			group.Go(func() {
				if attempt%2 == 0 {
					if err := projectTerminalRunReporting(t.Context(), store, run); err != nil {
						failures <- err
					}
					return
				}
				if _, err := terminalReportingContractWorker(t, store, now).ProcessBatch(t.Context()); err != nil {
					failures <- err
				}
			})
		}
		group.Wait()
		close(failures)
		for err := range failures {
			// A concurrent revision race can be retried by the durable worker;
			// immutable-content/idempotency conflicts must never be accepted.
			if !errors.Is(err, ErrRevisionConflict) {
				t.Fatal(err)
			}
		}
		terminalReportingContractAssertMessage(t, store, run, conversation, trigger)
		intent, err := store.GetRunTerminalReport(t.Context(), run.Scope, run.ID, run.Status)
		if err != nil {
			t.Fatal(err)
		}
		if intent.DeliveredAt == nil {
			now = intent.AvailableAt.Add(time.Second)
			if delivered, err := terminalReportingContractWorker(t, store, now).ProcessBatch(t.Context()); err != nil || delivered != 1 {
				t.Fatalf("concurrent delivery did not eventually acknowledge: delivered=%d error=%v", delivered, err)
			}
		}
		terminalReportingContractAssertMessage(t, store, run, conversation, trigger)
	})
}

func TestTerminalRunReportingContractAtomicInsertEligibility(t *testing.T) {
	eventWaitContractFixtures(t, func(t *testing.T, fixture eventWaitContractFixture) {
		store := terminalReportingContractFixture(t, fixture)
		cases := []struct {
			name       string
			status     AgentRunStatus
			milestones interface{}
			at         time.Time
			mutate     func(*AgentRun)
			want       bool
		}{
			{name: "completed", status: AgentRunStatusCompleted, milestones: []string{"completed"}, want: true},
			{name: "failed", status: AgentRunStatusFailed, milestones: []string{"failed"}, want: true},
			{name: "canceled", status: AgentRunStatusCanceled, milestones: []string{"failed"}, want: true},
			{name: "completion-disabled", status: AgentRunStatusCompleted, milestones: []string{"failed"}},
			{name: "failure-disabled", status: AgentRunStatusFailed, milestones: []string{"completed"}},
			{name: "root-missing", status: AgentRunStatusCompleted, milestones: []string{"completed"}, mutate: func(r *AgentRun) { delete(r.Context, runReportingContextRootRunID) }},
			{name: "child", status: AgentRunStatusCompleted, milestones: []string{"completed"}, mutate: func(r *AgentRun) { r.Context[runReportingContextRootRunID] = "different-root" }},
			{name: "destination-missing", status: AgentRunStatusCompleted, milestones: []string{"completed"}, mutate: func(r *AgentRun) { r.Context[conversationRunContextConversationID] = "" }},
			{name: "trigger-missing", status: AgentRunStatusCompleted, milestones: []string{"completed"}, mutate: func(r *AgentRun) { r.Context[conversationRunContextTriggerID] = "" }},
			{name: "milestones-malformed", status: AgentRunStatusCompleted, milestones: "completed"},
			{name: "milestones-empty", status: AgentRunStatusCompleted},
			{name: "trimmed-root", status: AgentRunStatusCompleted, milestones: []string{"completed"}, want: true, mutate: func(r *AgentRun) { r.Context[runReportingContextRootRunID] = "\t\u2000" + r.ID + "\u3000\n" }},
			{name: "unicode-blank-destination", status: AgentRunStatusCompleted, milestones: []string{"completed"}, mutate: func(r *AgentRun) { r.Context[conversationRunContextConversationID] = "\t\u2000\u3000\n" }},
			{name: "fraction-at-second-boundary", status: AgentRunStatusCompleted, milestones: []string{"completed"}, want: true, at: eventWaitContractEpoch.Add(999999 * time.Microsecond)},
			{name: "fraction-with-offset", status: AgentRunStatusCompleted, milestones: []string{"completed"}, want: true, at: eventWaitContractEpoch.Add(999999 * time.Microsecond).In(time.FixedZone("IST", 19800))},
		}
		for _, test := range cases {
			t.Run(test.name, func(t *testing.T) {
				id := uuid.NewString()
				run := &AgentRun{
					ID: id, RootRunID: id, Scope: Scope{Kind: "tenant", ID: "report-eligibility"}, Kind: RunKindAgentWork,
					Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "report-agent"}, AssignedAgentID: "report-agent",
					Goal: "Report completed work", Source: RunSourceSchedule, Status: test.status, Revision: 1,
					CreatedAt: eventWaitContractEpoch, UpdatedAt: eventWaitContractEpoch,
					Context: map[string]interface{}{runReportingContextRootRunID: id, conversationRunContextConversationID: "original-chat", conversationRunContextTriggerID: "original-message", runReportingContextMilestones: test.milestones},
				}
				if test.mutate != nil {
					test.mutate(run)
				}
				if !test.at.IsZero() {
					run.CreatedAt, run.UpdatedAt = test.at, test.at
				}
				_, err := store.CreateAgentRunWithEvent(t.Context(), run, &ActivityEvent{ID: uuid.NewString(), Scope: run.Scope, RunID: id, EventType: "run.completed", Summary: "Imported durable completion", CreatedAt: eventWaitContractEpoch})
				if err != nil {
					t.Fatal(err)
				}
				intent, err := store.GetRunTerminalReport(t.Context(), run.Scope, id, run.Status)
				if test.want {
					if err != nil || intent == nil || intent.Run == nil || intent.Status != run.Status || !intent.AvailableAt.Equal(run.UpdatedAt) {
						t.Fatalf("terminal creation lost its report: %#v, %v", intent, err)
					}
				} else if !errors.Is(err, ErrRunTerminalReportNotFound) {
					t.Fatalf("ineligible completion enqueued a report: %#v, %v", intent, err)
				}
			})
		}
	})
}

func TestTerminalRunReportingContractImmutableSnapshotScopedClaimsAndFencing(t *testing.T) {
	eventWaitContractFixtures(t, func(t *testing.T, fixture eventWaitContractFixture) {
		store := terminalReportingContractFixture(t, fixture)
		now := eventWaitContractEpoch
		first, _, _ := terminalReportingContractRun(t, store, Scope{Kind: "tenant", ID: "report-first"}, now)
		second, _, _ := terminalReportingContractRun(t, store, Scope{Kind: "tenant", ID: "report-second"}, now)
		first = terminalReportingContractComplete(t, store, first, now)
		second = terminalReportingContractComplete(t, store, second, now)
		changed := cloneAgentRun(first)
		changed.Revision++
		changed.UpdatedAt = now.Add(time.Second)
		changed.Output["summary"] = "A different later canonical payload"
		_, err := store.UpdateAgentRunWithEvent(t.Context(), changed, first.Revision, &ActivityEvent{ID: uuid.NewString(), Scope: first.Scope, RunID: first.ID, EventType: "run.updated", Summary: "Update canonical terminal metadata", CreatedAt: changed.UpdatedAt}, nil)
		if err != nil {
			t.Fatal(err)
		}
		claims, err := store.ClaimRunTerminalReports(t.Context(), RunTerminalReportingClaim{Scope: &first.Scope, WorkerID: "same-worker", Now: now, LeaseDuration: time.Minute, Limit: 1})
		if err != nil || len(claims) != 1 || claims[0].RunID != first.ID || claims[0].Run.Output["summary"] != "The requested result is ready" {
			t.Fatalf("scope isolation or immutable report snapshot was lost: %#v, %v", claims, err)
		}
		completion := RunTerminalReportingCompletion{Scope: first.Scope, RunID: first.ID, Status: first.Status, WorkerID: "wrong-worker", LeaseExpiresAt: *claims[0].LeaseExpiresAt, Now: now}
		if err := store.CompleteRunTerminalReport(t.Context(), completion); !errors.Is(err, ErrLeaseLost) {
			t.Fatalf("foreign worker acknowledged report: %v", err)
		}
		completion.WorkerID = claims[0].LeaseOwner
		completion.LeaseExpiresAt = completion.LeaseExpiresAt.Add(time.Nanosecond)
		if err := store.CompleteRunTerminalReport(t.Context(), completion); !errors.Is(err, ErrLeaseLost) {
			t.Fatalf("altered lease token acknowledged report: %v", err)
		}
		if err := store.RetryRunTerminalReport(t.Context(), RunTerminalReportingRetry{RunTerminalReportingCompletion: completion, AvailableAt: now.Add(time.Second)}); !errors.Is(err, ErrLeaseLost) {
			t.Fatalf("altered lease token rescheduled report: %v", err)
		}
		completion.LeaseExpiresAt = *claims[0].LeaseExpiresAt
		completion.Scope = second.Scope
		if err := store.CompleteRunTerminalReport(t.Context(), completion); !errors.Is(err, ErrLeaseLost) {
			t.Fatalf("foreign scope acknowledged report: %v", err)
		}
		completion.Scope = first.Scope
		if err := store.CompleteRunTerminalReport(t.Context(), completion); err != nil {
			t.Fatal(err)
		}
		changed.Revision++
		_, err = store.UpdateAgentRunWithEvent(t.Context(), changed, changed.Revision-1, &ActivityEvent{ID: uuid.NewString(), Scope: changed.Scope, RunID: changed.ID, EventType: "run.updated", Summary: "Update already reported terminal metadata", CreatedAt: changed.UpdatedAt}, nil)
		if err != nil {
			t.Fatal(err)
		}
		claims, err = store.ClaimRunTerminalReports(t.Context(), RunTerminalReportingClaim{WorkerID: "global-worker", Now: now.Add(time.Second), LeaseDuration: time.Minute, Limit: 1})
		if err != nil || len(claims) != 1 || claims[0].RunID != second.ID || claims[0].Scope != second.Scope {
			t.Fatalf("acknowledged identity requeued or global claim missed the other scope: %#v, %v", claims, err)
		}
	})
}

func TestTerminalRunReportingContractFailedTerminalCommitRollsBackIntent(t *testing.T) {
	eventWaitContractFixtures(t, func(t *testing.T, fixture eventWaitContractFixture) {
		store := terminalReportingContractFixture(t, fixture)
		now := eventWaitContractEpoch
		run, _, _ := terminalReportingContractRun(t, store, Scope{Kind: "tenant", ID: "report-atomic-failure"}, now)
		activity := NewRunActivityService(store, store)
		activity.now = func() time.Time { return now }
		running, _, err := activity.TransitionRun(t.Context(), run.Scope, run.ID, RunTransitionRequest{ExpectedRevision: run.Revision, Status: AgentRunStatusRunning})
		if err != nil {
			t.Fatal(err)
		}
		// Fail the audit write AFTER the canonical terminal write and its
		// enqueue trigger. A rollback must remove both the completion and intent.
		switch database := store.(type) {
		case *SQLiteStore:
			_, err = database.db.ExecContext(t.Context(), `CREATE TRIGGER reporting_contract_activity_failure BEFORE INSERT ON run_activity BEGIN SELECT RAISE(ABORT,'injected activity failure'); END`)
			t.Cleanup(func() {
				_, _ = database.db.ExecContext(context.Background(), `DROP TRIGGER IF EXISTS reporting_contract_activity_failure`)
			})
		case *PostgresStore:
			_, err = database.db.ExecContext(t.Context(), `CREATE FUNCTION `+database.table("reporting_contract_activity_failure")+`() RETURNS trigger LANGUAGE plpgsql AS $report_test$ BEGIN RAISE EXCEPTION 'injected activity failure'; END; $report_test$;
				CREATE TRIGGER reporting_contract_activity_failure BEFORE INSERT ON `+database.table("run_activity")+` FOR EACH ROW EXECUTE FUNCTION `+database.table("reporting_contract_activity_failure")+`()`)
		default:
			// Memory commits cannot fail after validation; reject the terminal
			// revision at the same command boundary instead.
			running.Revision--
		}
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = activity.TransitionRun(t.Context(), run.Scope, run.ID, RunTransitionRequest{ExpectedRevision: running.Revision, Status: AgentRunStatusCompleted})
		if err == nil {
			t.Fatal("injected terminal commit failure was accepted")
		}
		canonical, err := store.GetAgentRun(t.Context(), run.Scope, run.ID)
		if err != nil || canonical.Status != AgentRunStatusRunning {
			t.Fatalf("failed terminal transaction changed the Run: %#v, %v", canonical, err)
		}
		if intent, err := store.GetRunTerminalReport(t.Context(), run.Scope, run.ID, AgentRunStatusCompleted); !errors.Is(err, ErrRunTerminalReportNotFound) {
			t.Fatalf("failed terminal transaction left a delivery intent: %#v, %v", intent, err)
		}
	})
}
